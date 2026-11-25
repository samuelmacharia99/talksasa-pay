package stk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Actors for work the service does on its own.
var (
	callbackActor = audit.System("mpesa-callback")
	sweeperActor  = audit.System("stk-sweeper")
)

// Transaction failure codes written by this package.
const (
	failRejected    = "rejected"        // Daraja refused the push: no prompt was shown
	failNotSent     = "not_sent"        // never heard back from Daraja when sending
	failNoReceipt   = "no_receipt"      // status query said paid; no receipt
	failNoAnswer    = "no_answer"       // no definite answer before GiveUpAfter
	failMismatch    = "amount_mismatch" // callback amount differs from the push
	failReceiptUsed = "receipt_taken"   // callback receipt already on another row
	failBadCallback = "bad_callback"    // success callback we could not use
	failStaff       = "staff_failed"    // staff resolved: no money arrived
)

// Send sends a prompt for the charge's exact amount.
//
//  1. Tx A: lock the charge; check payable, amount <= mpesa.MaxAmount, no
//     live push; insert the pending mpesa_stk transaction and the push
//     (initiating). Replays (same app + idempotency key) return the
//     existing push with replayed=true.
//  2. Call Daraja OUTSIDE any transaction.
//  3. Tx B: accepted -> push sent (+ checkout ids, next_query_at), emit
//     stk_push.sent; rejected -> push failed, transaction failed, emit
//     stk_push.failed, return the push with ErrDaraja; transient (outcome
//     unknown) -> leave initiating for the sweeper, return the push.
func (s *Service) Send(ctx context.Context, actor audit.Actor, p SendParams) (push Push, replayed bool, err error) {
	if !s.available() {
		return Push{}, false, ErrUnavailable
	}
	if err := actor.Validate(); err != nil {
		return Push{}, false, err
	}
	ph, err := phone.Normalize(p.Phone)
	if err != nil {
		return Push{}, false, err
	}
	var (
		key  *string
		hash []byte
	)
	switch p.Source {
	case SourceApp:
		k := strings.TrimSpace(p.IdempotencyKey)
		if k == "" || len(k) > 255 || strings.IndexFunc(k, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return Push{}, false, ErrInvalidIdempotency
		}
		key, hash = &k, requestHash(p.ChargeID, ph)
		existing, ok, err := s.byKey(ctx, p.AppID, k)
		if err != nil {
			return Push{}, false, err
		}
		if ok {
			return replay(existing, p.ChargeID, hash)
		}
	case SourcePayPage:
		// No key: the one-live-push rule stops repeats.
	default:
		return Push{}, false, fmt.Errorf("stk: unknown source %q", p.Source)
	}

	var d darajaArgs
	err = db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		push, d, err = s.startTx(ctx, q, p, ph, key, hash)
		return err
	})
	if err != nil {
		oneLive := db.IsUniqueViolation(err, "stk_pushes_one_live")
		var inProg *InProgressError
		if key != nil && (oneLive || errors.As(err, &inProg) || db.IsUniqueViolation(err, "stk_pushes_app_id_idempotency_key_key")) {
			// A concurrent call with the same key may have won either index.
			existing, ok, gerr := s.byKey(ctx, p.AppID, *key)
			if gerr != nil {
				return Push{}, false, gerr
			}
			if ok {
				return replay(existing, p.ChargeID, hash)
			}
		}
		if oneLive {
			return Push{}, false, s.inProgress(ctx, p.ChargeID)
		}
		return Push{}, false, err
	}

	resp, derr := s.o.Client.STKPush(ctx, mpesa.STKPushRequest{
		Phone: ph, Amount: push.Amount, AccountReference: d.accountRef, TransactionDesc: d.desc,
	})
	// Record Daraja's answer even if the caller has gone away: the prompt
	// may already be on the customer's phone.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	log := s.o.Logger.With(slog.String("stk_push_id", push.ID.String()), slog.String("charge_id", push.ChargeID.String()),
		slog.String("phone", tlog.MaskPhone(ph)), slog.Int64("amount", push.Amount))

	switch {
	case derr == nil:
		if strings.TrimSpace(resp.CheckoutRequestID) == "" || len(resp.CheckoutRequestID) > 128 {
			// Accepted without a usable checkout id: we cannot follow it up.
			// Leave it initiating; a callback can still match it as an orphan.
			s.alarm(bctx, "Daraja accepted an STK push without a usable CheckoutRequestID", slog.String("stk_push_id", push.ID.String()))
			return push, false, nil
		}
		sent, err := s.markSent(bctx, actor, push.ID, resp)
		if err != nil {
			s.alarm(bctx, "STK push accepted by Daraja but not saved; its callback will be matched as an orphan",
				slog.String("stk_push_id", push.ID.String()), slog.String("checkout_request_id", resp.CheckoutRequestID),
				slog.String("error", err.Error()))
			return push, false, nil
		}
		log.InfoContext(bctx, "stk push sent", slog.String("checkout_request_id", resp.CheckoutRequestID))
		return sent, false, nil
	case mpesa.IsRejected(derr):
		var me *mpesa.Error
		_ = errors.As(derr, &me)
		failed, err := s.markRejected(bctx, actor, push.ID, me)
		if err != nil {
			log.ErrorContext(bctx, "stk push rejected by Daraja; saving the failure failed (the sweeper will fail it)", slog.String("error", err.Error()))
			return push, false, ErrDaraja
		}
		log.WarnContext(bctx, "stk push rejected by Daraja", slog.String("error", derr.Error()))
		return failed, false, ErrDaraja
	default:
		// Transient or unknown: the prompt may or may not have been sent.
		log.WarnContext(bctx, "stk push outcome unknown; left for the sweeper", slog.String("error", derr.Error()))
		return push, false, nil
	}
}

type darajaArgs struct {
	accountRef, desc string
}

func (s *Service) startTx(ctx context.Context, q *store.Queries, p SendParams, ph string, key *string, hash []byte) (Push, darajaArgs, error) {
	// TODO(gateway-core): review payability rules (rule 4: re-prompt after a
	// failed prompt, one live prompt; needs_review/expired/cancelled not payable).
	c, err := q.LockCharge(ctx, store.LockChargeParams{AppID: p.AppID, ID: p.ChargeID})
	if db.IsNotFound(err) {
		return Push{}, darajaArgs{}, ErrNotFound
	}
	if err != nil {
		return Push{}, darajaArgs{}, err
	}
	if c.Status != store.ChargeStatusPending || !c.ExpiresAt.After(s.o.Now()) {
		return Push{}, darajaArgs{}, ErrNotPayable // not pending, or expired but not yet swept
	}
	a, err := q.GetApp(ctx, p.AppID)
	if err != nil {
		return Push{}, darajaArgs{}, err
	}
	if a.Status != store.AppStatusActive {
		return Push{}, darajaArgs{}, ErrNotPayable
	}
	if c.Amount > mpesa.MaxAmount {
		return Push{}, darajaArgs{}, ErrAmountTooLarge
	}
	if live, err := q.GetLiveSTKPushForCharge(ctx, c.ID); err == nil {
		return Push{}, darajaArgs{}, &InProgressError{LiveID: live.ID}
	} else if !db.IsNotFound(err) {
		return Push{}, darajaArgs{}, err
	}
	txn, err := ledger.StartSTK(ctx, q, ledger.StartSTKParams{
		AppID: p.AppID, ChargeID: c.ID, Amount: money.KES(c.Amount), Phone: ph, AccountNumber: c.AccountNumber,
	})
	if err != nil {
		return Push{}, darajaArgs{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Push{}, darajaArgs{}, err
	}
	next := s.o.Now().Add(s.o.InitiatingTimeout)
	push, err := q.InsertSTKPush(ctx, store.InsertSTKPushParams{
		ID: id, AppID: p.AppID, ChargeID: c.ID, TransactionID: txn.ID, Source: string(p.Source),
		Phone: ph, Amount: c.Amount, IdempotencyKey: key, RequestHash: hash, NextQueryAt: &next,
	})
	if err != nil {
		return Push{}, darajaArgs{}, err
	}
	return push, darajaArgs{accountRef: c.AccountNumber, desc: c.Reference}, nil
}

// markSent is Tx B after Daraja accepted the push.
func (s *Service) markSent(ctx context.Context, actor audit.Actor, id uuid.UUID, resp mpesa.STKPushResponse) (Push, error) {
	var out Push
	err := db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		cur, err := q.LockSTKPush(ctx, id)
		if err != nil {
			return err
		}
		if cur.Status != store.StkPushStatusInitiating {
			// A callback (matched as an orphan) or the sweeper got here
			// first; their outcome stands.
			out = cur
			return nil
		}
		out, err = s.sendPushTx(ctx, q, cur, optStr(truncate(resp.MerchantRequestID, 128)), strings.TrimSpace(resp.CheckoutRequestID))
		return err
	})
	return out, err
}

// sendPushTx moves an initiating push to sent with Safaricom's ids, links
// the checkout id on its transaction and emits stk_push.sent.
func (s *Service) sendPushTx(ctx context.Context, q *store.Queries, cur Push, merchantID *string, checkoutID string) (Push, error) {
	next := s.o.Now().Add(s.o.QueryAfter)
	out, err := q.MarkSTKPushSent(ctx, store.MarkSTKPushSentParams{
		MerchantRequestID: merchantID, CheckoutRequestID: &checkoutID, NextQueryAt: &next, ID: cur.ID,
	})
	if err != nil {
		return Push{}, fmt.Errorf("stk: mark sent: %w", err)
	}
	if err := s.linkCheckout(ctx, q, cur.TransactionID, checkoutID); err != nil {
		return Push{}, err
	}
	if err := s.o.Events.EmitSTKPush(ctx, q, out.AppID, out.ID, events.STKPushSent); err != nil {
		return Push{}, err
	}
	return out, nil
}

// linkCheckout stores the checkout id on the push's transaction unless
// another mpesa_stk row already carries it (transactions_provider_request_uq).
func (s *Service) linkCheckout(ctx context.Context, q *store.Queries, txnID uuid.UUID, checkoutID string) error {
	other, err := q.GetTransactionByProviderRequest(ctx, store.GetTransactionByProviderRequestParams{
		Channel: store.TxnChannelMpesaStk, ProviderRequestID: &checkoutID,
	})
	if err == nil {
		if other.ID != txnID {
			s.alarm(ctx, "checkout id already linked to another transaction", slog.String("transaction_id", txnID.String()),
				slog.String("other_transaction_id", other.ID.String()), slog.String("checkout_request_id", checkoutID))
		}
		return nil
	}
	if !db.IsNotFound(err) {
		return err
	}
	if err := q.SetTransactionProviderRequest(ctx, store.SetTransactionProviderRequestParams{ProviderRequestID: &checkoutID, ID: txnID}); err != nil {
		return fmt.Errorf("stk: link checkout id: %w", err)
	}
	return nil
}

// markRejected is Tx B after Daraja refused the push: nothing was sent.
func (s *Service) markRejected(ctx context.Context, actor audit.Actor, id uuid.UUID, me *mpesa.Error) (Push, error) {
	desc := "Safaricom did not accept the prompt"
	var code *string
	if me != nil {
		if m := strings.TrimSpace(me.Message); m != "" {
			desc = m
		}
		code = optStr(truncate(me.Code, 32))
	}
	var out Push
	err := db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		cur, err := q.LockSTKPush(ctx, id)
		if err != nil {
			return err
		}
		if cur.Status != store.StkPushStatusInitiating {
			out = cur
			return nil
		}
		out, err = s.failPushTx(ctx, q, cur, code, desc, failRejected, desc)
		return err
	})
	return out, err
}

// failPushTx fails a live/unknown push and its pending transaction, and
// emits stk_push.failed.
func (s *Service) failPushTx(ctx context.Context, q *store.Queries, cur Push, resultCode *string, resultDesc, txnCode, txnReason string) (Push, error) {
	out, err := q.FinishSTKPush(ctx, store.FinishSTKPushParams{
		Status: store.StkPushStatusFailed, ResultCode: resultCode, ResultDesc: optStr(truncate(resultDesc, 500)), ID: cur.ID,
	})
	if err != nil {
		return Push{}, fmt.Errorf("stk: fail push: %w", err)
	}
	txn, err := q.LockTransactionInternal(ctx, cur.TransactionID)
	if err != nil {
		return Push{}, err
	}
	if txn.Status == store.TxnStatusPending || txn.Status == store.TxnStatusNeedsReview {
		if _, err := ledger.FailSTK(ctx, q, txn.ID, txnCode, txnReason, nil); err != nil {
			return Push{}, err
		}
	}
	if err := s.o.Events.EmitSTKPush(ctx, q, out.AppID, out.ID, events.STKPushFailed); err != nil {
		return Push{}, err
	}
	return out, nil
}

// reviewPushTx: Safaricom says (or may have said) paid but we have no usable
// receipt. Push sent -> unknown, pending transaction -> needs_review, emit
// stk_push.needs_review. Staff resolve it.
func (s *Service) reviewPushTx(ctx context.Context, q *store.Queries, cur Push, resultCode *string, resultDesc, txnCode, txnReason string) (Push, error) {
	out := cur
	if cur.Status == store.StkPushStatusSent {
		var err error
		out, err = q.FinishSTKPush(ctx, store.FinishSTKPushParams{
			Status: store.StkPushStatusUnknown, ResultCode: resultCode, ResultDesc: optStr(truncate(resultDesc, 500)), ID: cur.ID,
		})
		if err != nil {
			return Push{}, fmt.Errorf("stk: push to unknown: %w", err)
		}
	}
	// An anomaly reported by Safaricom must stop staff from confirming,
	// even when the transaction was already in review (e.g. the status
	// check got there first): needs_review's failure code is set once and
	// may say "no_receipt".
	if txnCode == failMismatch || txnCode == failReceiptUsed {
		if err := q.BlockSTKPushConfirm(ctx, store.BlockSTKPushConfirmParams{Reason: &txnCode, ID: cur.ID}); err != nil {
			return Push{}, fmt.Errorf("stk: block confirm: %w", err)
		}
	}
	txn, err := q.LockTransactionInternal(ctx, cur.TransactionID)
	if err != nil {
		return Push{}, err
	}
	changed := out.Status != cur.Status
	if txn.Status == store.TxnStatusPending {
		if _, err := ledger.MarkNeedsReview(ctx, q, txn.ID, txnCode, txnReason); err != nil {
			return Push{}, err
		}
		changed = true
	}
	if changed {
		if err := s.o.Events.EmitSTKPush(ctx, q, out.AppID, out.ID, events.STKPushNeedsReview); err != nil {
			return Push{}, err
		}
	}
	return out, nil
}

func (s *Service) byKey(ctx context.Context, appID uuid.UUID, key string) (Push, bool, error) {
	existing, err := store.New(s.o.Pool).GetSTKPushByIdempotencyKey(ctx, store.GetSTKPushByIdempotencyKeyParams{AppID: appID, IdempotencyKey: &key})
	if db.IsNotFound(err) {
		return Push{}, false, nil
	}
	if err != nil {
		return Push{}, false, err
	}
	return existing, true, nil
}

func replay(existing Push, chargeID uuid.UUID, hash []byte) (Push, bool, error) {
	if existing.ChargeID != chargeID || !bytes.Equal(existing.RequestHash, hash) {
		return Push{}, false, ErrIdempotencyConflict
	}
	return existing, true, nil
}

func (s *Service) inProgress(ctx context.Context, chargeID uuid.UUID) error {
	live, err := store.New(s.o.Pool).GetLiveSTKPushForCharge(ctx, chargeID)
	if err != nil && !db.IsNotFound(err) {
		return err
	}
	return &InProgressError{LiveID: live.ID} // uuid.Nil if it finished meanwhile
}

// requestHash fingerprints what an idempotent push asked for.
func requestHash(chargeID uuid.UUID, phone string) []byte {
	h := sha256.New()
	h.Write(chargeID[:])
	h.Write([]byte{0})
	h.Write([]byte(phone))
	return h.Sum(nil)
}

// Get returns an app's push.
func (s *Service) Get(ctx context.Context, appID, id uuid.UUID) (Push, error) {
	p, err := store.New(s.o.Pool).GetSTKPushForApp(ctx, store.GetSTKPushForAppParams{AppID: appID, ID: id})
	if db.IsNotFound(err) {
		return Push{}, ErrNotFound
	}
	return p, err
}

// LatestForCharge returns the newest push for a charge created after since,
// or nil (pay page).
func (s *Service) LatestForCharge(ctx context.Context, chargeID uuid.UUID, since time.Time) (*Push, error) {
	p, err := store.New(s.o.Pool).LatestSTKPushForChargeSince(ctx, store.LatestSTKPushForChargeSinceParams{ChargeID: chargeID, Since: since})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListForCharge lists a charge's pushes, newest first (admin).
func (s *Service) ListForCharge(ctx context.Context, chargeID uuid.UUID, limit int) ([]Push, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return store.New(s.o.Pool).ListSTKPushesForCharge(ctx, store.ListSTKPushesForChargeParams{ChargeID: chargeID, Lim: int32(limit)})
}

// alarm logs something staff must look at (money anomaly, lost callback).
func (s *Service) alarm(ctx context.Context, msg string, attrs ...any) {
	s.o.Logger.ErrorContext(ctx, msg, append(attrs, slog.Bool("alarm", true))...)
}

func optStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
