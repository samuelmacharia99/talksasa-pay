package stk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// CallbackKind is what a callback carries.
type CallbackKind = store.MpesaCallbackKind

// MaxCallbackBytes caps a stored callback body.
const MaxCallbackBytes = 16 << 10

// StoreCallback stores a callback body verbatim and enqueues its processing
// job in the same transaction. Called by the /cb HTTP handler BEFORE it
// acknowledges Safaricom. raw must be a JSON object (<= 16 KiB).
func (s *Service) StoreCallback(ctx context.Context, kind CallbackKind, remoteIP netip.Addr, raw []byte) (uuid.UUID, error) {
	if !kind.Valid() {
		return uuid.Nil, fmt.Errorf("stk: unknown callback kind %q", kind)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(raw) > MaxCallbackBytes || trimmed[0] != '{' || !json.Valid(trimmed) {
		return uuid.Nil, ErrInvalidCallback
	}
	if s.o.Callbacks == nil {
		return uuid.Nil, errors.New("stk: no callback job enqueuer configured")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	var ip *netip.Addr
	if remoteIP.IsValid() {
		a := remoteIP.Unmap()
		ip = &a
	}
	err = db.WithTx(ctx, s.o.Pool, callbackActor.String(), func(ctx context.Context, q *store.Queries, tx pgx.Tx) error {
		if _, err := q.InsertMpesaCallback(ctx, store.InsertMpesaCallbackParams{ID: id, Kind: kind, RemoteIp: ip, Raw: trimmed}); err != nil {
			return err
		}
		return s.o.Callbacks.EnqueueCallback(ctx, tx, id)
	})
	if _, code, ok := db.ConstraintViolation(err); ok && code == "23514" {
		return uuid.Nil, ErrInvalidCallback // e.g. jsonb rendering above the size cap
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("stk: store callback: %w", err)
	}
	return id, nil
}

// outcome is what processing one callback decided.
type outcome struct {
	note  string     // stored on the inbox row (anomaly / remark); "" = clean
	txnID *uuid.UUID // transaction the callback produced or concerns
}

// logbuf collects log records inside a (possibly retried) transaction body;
// they are written only after it commits.
type logbuf struct{ recs []logRec }

type logRec struct {
	level slog.Level
	msg   string
	attrs []any
}

func (b *logbuf) add(level slog.Level, msg string, attrs ...any) {
	b.recs = append(b.recs, logRec{level: level, msg: msg, attrs: attrs})
}
func (b *logbuf) info(msg string, attrs ...any) { b.add(slog.LevelInfo, msg, attrs...) }
func (b *logbuf) warn(msg string, attrs ...any) { b.add(slog.LevelWarn, msg, attrs...) }
func (b *logbuf) alarm(msg string, attrs ...any) {
	b.add(slog.LevelError, msg, append(attrs, slog.Bool("alarm", true))...)
}

func (s *Service) flush(ctx context.Context, b *logbuf) {
	for _, r := range b.recs {
		s.o.Logger.Log(ctx, r.level, r.msg, r.attrs...)
	}
}

var errCallbackNotFound = errors.New("stk: callback not found")

// ProcessCallback processes one stored callback (worker job). Idempotent:
// an already-processed callback is a no-op.
//
// STK success: complete the push's pending transaction with the receipt and
// pay its charge (ledger.SucceedCharge); amount mismatch or a receipt already
// on another row -> needs_review + alarm; unknown CheckoutRequestID -> match
// a single orphan push by phone+amount, else record unmatched. STK failure:
// fail the push and its transaction (a late failure after "unknown" also
// fails it). C2B: routed to an app by account prefix (unknown prefix ->
// unmatched; inactive app -> still recorded and forwarded, warning).
//
// Anomalies (unreadable bodies, amount mismatches, receipts on another row,
// foreign short codes) are final: the callback is marked processed with the
// error and an alarm is logged. Other errors (database) are returned so the
// job is retried; the attempt is recorded on the inbox row.
func (s *Service) ProcessCallback(ctx context.Context, callbackID uuid.UUID) error {
	var (
		lb  *logbuf
		out outcome
	)
	err := db.WithTx(ctx, s.o.Pool, callbackActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		lb, out = &logbuf{}, outcome{}
		cb, err := q.LockMpesaCallback(ctx, callbackID)
		if db.IsNotFound(err) {
			return errCallbackNotFound
		}
		if err != nil {
			return err
		}
		if cb.ProcessedAt != nil {
			return nil
		}
		switch cb.Kind {
		case store.MpesaCallbackKindStk:
			out, err = s.processSTK(ctx, q, cb, lb)
		case store.MpesaCallbackKindC2bConfirmation:
			out, err = s.processC2B(ctx, q, cb, lb)
		default:
			lb.alarm("mpesa callback of unknown kind", slog.String("callback_id", cb.ID.String()))
			out = outcome{note: "unknown callback kind"}
		}
		if err != nil {
			return err
		}
		return q.MarkMpesaCallbackProcessed(ctx, store.MarkMpesaCallbackProcessedParams{
			Error: optStr(truncate(out.note, 500)), TransactionID: out.txnID, ID: callbackID,
		})
	})
	if errors.Is(err, errCallbackNotFound) {
		s.o.Logger.ErrorContext(ctx, "mpesa callback job for a callback that does not exist", slog.String("callback_id", callbackID.String()))
		return nil
	}
	if err != nil {
		if ctx.Err() == nil {
			msg := truncate("processing failed (will retry): "+err.Error(), 500)
			if rerr := db.WithTx(ctx, s.o.Pool, callbackActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
				return q.RecordMpesaCallbackError(ctx, store.RecordMpesaCallbackErrorParams{Error: &msg, ID: callbackID})
			}); rerr != nil {
				s.o.Logger.WarnContext(ctx, "could not record mpesa callback error", slog.String("callback_id", callbackID.String()), slog.String("error", rerr.Error()))
			}
		}
		s.o.Logger.WarnContext(ctx, "mpesa callback processing failed; will retry", slog.String("callback_id", callbackID.String()), slog.String("error", err.Error()))
		return err
	}
	s.flush(ctx, lb)
	return nil
}

// ---------------------------------------------------------------------------
// STK callbacks
// ---------------------------------------------------------------------------

func (s *Service) processSTK(ctx context.Context, q *store.Queries, cb store.MpesaCallback, lb *logbuf) (outcome, error) {
	cbAttr := slog.String("callback_id", cb.ID.String())
	r, perr := mpesa.ParseSTKCallback(cb.Raw)
	if perr != nil {
		if errors.Is(perr, mpesa.ErrMalformed) || r.CheckoutRequestID == "" {
			lb.alarm("unreadable STK callback", cbAttr, slog.String("error", perr.Error()))
			return outcome{note: "unreadable STK callback: " + perr.Error()}, nil
		}
		// Safaricom says paid, but without a receipt/amount we can record.
		// Never guess: staff review.
		push, err := q.LockSTKPushByCheckout(ctx, &r.CheckoutRequestID)
		if db.IsNotFound(err) {
			lb.alarm("unusable successful STK callback for an unknown checkout id", cbAttr,
				slog.String("checkout_request_id", r.CheckoutRequestID), slog.String("error", perr.Error()))
			return outcome{note: "unusable successful STK callback (unknown checkout id): " + perr.Error()}, nil
		}
		if err != nil {
			return outcome{}, err
		}
		if _, err := s.reviewPushTx(ctx, q, push, &r.ResultCode, "success callback without a usable receipt", failBadCallback, perr.Error()); err != nil {
			return outcome{}, err
		}
		lb.alarm("successful STK callback without a usable receipt; sent to staff review", cbAttr,
			slog.String("stk_push_id", push.ID.String()), slog.String("error", perr.Error()))
		return outcome{note: "unusable successful STK callback: " + perr.Error(), txnID: &push.TransactionID}, nil
	}
	if !r.Succeeded() {
		return s.stkFailure(ctx, q, cb, r, lb)
	}
	return s.stkSuccess(ctx, q, cb, r, lb)
}

func (s *Service) stkFailure(ctx context.Context, q *store.Queries, cb store.MpesaCallback, r mpesa.STKResult, lb *logbuf) (outcome, error) {
	push, err := q.LockSTKPushByCheckout(ctx, &r.CheckoutRequestID)
	if db.IsNotFound(err) {
		// Failure callbacks carry no phone or amount, so an orphan push
		// cannot be matched; the sweeper fails it after InitiatingTimeout.
		lb.warn("STK failure callback for an unknown checkout id", slog.String("callback_id", cb.ID.String()),
			slog.String("checkout_request_id", r.CheckoutRequestID), slog.String("result_code", r.ResultCode))
		return outcome{note: "failure callback for an unknown checkout id"}, nil
	}
	if err != nil {
		return outcome{}, err
	}
	attrs := []any{slog.String("callback_id", cb.ID.String()), slog.String("stk_push_id", push.ID.String()), slog.String("result_code", r.ResultCode)}
	switch push.Status {
	case store.StkPushStatusSucceeded:
		txn, err := q.GetTransactionInternal(ctx, push.TransactionID)
		if err != nil {
			return outcome{}, err
		}
		if completedByC2B(txn) {
			// Expected: the customer paid at the paybill (which completed
			// this prompt) and then cancelled or ignored the prompt itself.
			lb.info("STK failure callback for a push already paid by paybill; ignored", attrs...)
			return outcome{note: "failure after the push was paid by paybill; ignored", txnID: &push.TransactionID}, nil
		}
		lb.alarm("STK failure callback after the push succeeded; ignored", attrs...)
		return outcome{note: "late failure after success; ignored", txnID: &push.TransactionID}, nil
	case store.StkPushStatusFailed:
		return outcome{txnID: &push.TransactionID}, nil // duplicate
	}
	if _, err := s.failPushTx(ctx, q, push, &r.ResultCode, r.ResultDesc, r.ResultCode, r.ResultDesc); err != nil {
		return outcome{}, err
	}
	lb.info("stk push failed", append(attrs, slog.String("result_desc", r.ResultDesc))...)
	return outcome{txnID: &push.TransactionID}, nil
}

func (s *Service) stkSuccess(ctx context.Context, q *store.Queries, cb store.MpesaCallback, r mpesa.STKResult, lb *logbuf) (outcome, error) {
	push, found, why, err := s.pushForSuccess(ctx, q, cb, r, lb)
	if err != nil {
		return outcome{}, err
	}
	if !found {
		return s.recordUnmatchedSTK(ctx, q, cb, r, lb, why)
	}
	txn, err := q.LockTransactionInternal(ctx, push.TransactionID)
	if err != nil {
		return outcome{}, err
	}
	attrs := []any{slog.String("callback_id", cb.ID.String()), slog.String("stk_push_id", push.ID.String()),
		slog.String("transaction_id", txn.ID.String()), slog.String("receipt", r.Receipt)}

	switch txn.Status {
	case store.TxnStatusSucceeded:
		if txn.ProviderTransactionID != nil && *txn.ProviderTransactionID == r.Receipt {
			return outcome{txnID: &txn.ID}, nil // duplicate callback
		}
		if completedByC2B(txn) {
			// The prompt was completed by a paybill payment of the same
			// amount, and Safaricom now confirms the prompt itself with a
			// different receipt: the customer paid twice. Both are real.
			return s.recordSecondPayment(ctx, q, cb, push, txn, r, lb)
		}
		lb.alarm("STK success callback with a different receipt for a push that already succeeded; not recorded", attrs...)
		return outcome{note: "receipt " + r.Receipt + " differs from the receipt already recorded for this push; not recorded", txnID: &txn.ID}, nil

	case store.TxnStatusPending, store.TxnStatusNeedsReview:
		if r.Amount != txn.Amount {
			reason := fmt.Sprintf("Safaricom reported KES %d (receipt %s); the prompt was for KES %d", r.Amount, r.Receipt, txn.Amount)
			if _, err := s.reviewPushTx(ctx, q, push, &r.ResultCode, "amount mismatch", failMismatch, reason); err != nil {
				return outcome{}, err
			}
			lb.alarm("STK callback amount differs from the push; sent to staff review", append(attrs,
				slog.Int64("callback_amount", r.Amount), slog.Int64("push_amount", txn.Amount))...)
			return outcome{note: "amount mismatch: " + reason, txnID: &txn.ID}, nil
		}
		other, err := q.GetTransactionByReceipt(ctx, store.GetTransactionByReceiptParams{Provider: "mpesa", ProviderTransactionID: &r.Receipt})
		if err == nil && other.ID != txn.ID {
			reason := fmt.Sprintf("receipt %s is already recorded on transaction %s", r.Receipt, other.ID)
			if _, err := s.reviewPushTx(ctx, q, push, &r.ResultCode, "receipt already recorded", failReceiptUsed, reason); err != nil {
				return outcome{}, err
			}
			lb.alarm("STK callback receipt already recorded on another transaction; sent to staff review",
				append(attrs, slog.String("other_transaction_id", other.ID.String()))...)
			return outcome{note: reason, txnID: &txn.ID}, nil
		}
		if err != nil && !db.IsNotFound(err) {
			return outcome{}, err
		}
		done, err := ledger.CompleteSTK(ctx, q, ledger.CompleteSTKParams{
			TransactionID: txn.ID, Receipt: r.Receipt, Phone: r.Phone, OccurredAt: r.OccurredAt, RawPayload: cb.Raw,
		})
		if err != nil {
			return outcome{}, err
		}
		if err := s.succeedPushTx(ctx, q, push, r.ResultDesc); err != nil {
			return outcome{}, err
		}
		if err := s.payCharge(ctx, q, done, lb, attrs); err != nil {
			return outcome{}, err
		}
		lb.info("stk payment received", append(attrs, slog.Int64("amount", done.Amount))...)
		return outcome{txnID: &done.ID}, nil

	case store.TxnStatusFailed:
		return s.successAfterGiveUp(ctx, q, cb, push, txn, r, lb)

	default:
		lb.alarm("STK success callback for a transaction in an unexpected state; not recorded", append(attrs, slog.String("status", string(txn.Status)))...)
		return outcome{note: "transaction is " + string(txn.Status) + "; not recorded", txnID: &txn.ID}, nil
	}
}

// successAfterGiveUp: Safaricom brought a receipt for a push we had already
// failed (status query said failed, no answer in time, crash before Daraja
// answered...). The money really arrived, so it is recorded as a NEW
// mpesa_stk transaction (the failed row is immutable) and the push moves
// failed -> succeeded.
func (s *Service) successAfterGiveUp(ctx context.Context, q *store.Queries, cb store.MpesaCallback, push Push, failed store.Transaction, r mpesa.STKResult, lb *logbuf) (outcome, error) {
	attrs := []any{slog.String("callback_id", cb.ID.String()), slog.String("stk_push_id", push.ID.String()),
		slog.String("failed_transaction_id", failed.ID.String()), slog.String("receipt", r.Receipt)}

	if push.Status == store.StkPushStatusSucceeded {
		// Already rescued by an earlier callback: duplicate if same receipt.
		existing, err := q.GetTransactionByReceipt(ctx, store.GetTransactionByReceiptParams{Provider: "mpesa", ProviderTransactionID: &r.Receipt})
		if err == nil && existing.Amount == r.Amount && existing.Direction == store.TxnDirectionIn {
			return outcome{txnID: &existing.ID}, nil
		}
		if err != nil && !db.IsNotFound(err) {
			return outcome{}, err
		}
		lb.alarm("another STK success callback for a push that already succeeded; not recorded", attrs...)
		return outcome{note: "push already succeeded with another receipt; not recorded", txnID: &failed.ID}, nil
	}
	if push.Status != store.StkPushStatusFailed {
		lb.alarm("STK success callback: push and transaction disagree; not recorded", append(attrs, slog.String("push_status", string(push.Status)))...)
		return outcome{note: "push is " + string(push.Status) + " but its transaction failed; not recorded", txnID: &failed.ID}, nil
	}
	if r.Amount != push.Amount {
		lb.alarm("STK payment for a push we had given up on, with a different amount; NOT recorded",
			append(attrs, slog.Int64("callback_amount", r.Amount), slog.Int64("push_amount", push.Amount))...)
		return outcome{note: fmt.Sprintf("amount mismatch on a push already failed (callback KES %d, push KES %d); not recorded", r.Amount, push.Amount), txnID: &failed.ID}, nil
	}

	// transactions_provider_request_uq: the failed row may already hold this
	// checkout id; the link is then kept on the push and the callback row.
	provReq := r.CheckoutRequestID
	if _, err := q.GetTransactionByProviderRequest(ctx, store.GetTransactionByProviderRequestParams{
		Channel: store.TxnChannelMpesaStk, ProviderRequestID: &provReq,
	}); err == nil {
		provReq = ""
	} else if !db.IsNotFound(err) {
		return outcome{}, err
	}
	in := ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaStk, Receipt: r.Receipt, ProviderRequest: provReq, Amount: money.KES(r.Amount),
		Phone: usablePhone(r.Phone), AppID: &push.AppID, ChargeID: &push.ChargeID,
		OccurredAt: r.OccurredAt, RawPayload: cb.Raw,
	}
	if failed.AccountNumber != nil {
		in.AccountNumber = *failed.AccountNumber
	}
	txn, created, err := ledger.RecordIncoming(ctx, q, in)
	if errors.Is(err, ledger.ErrReceiptConflict) {
		lb.alarm("STK receipt already recorded with different details; not recorded", attrs...)
		return outcome{note: "receipt already recorded with different details", txnID: &failed.ID}, nil
	}
	if err != nil {
		return outcome{}, err
	}
	if !created {
		lb.alarm("STK payment for a push we had given up on was already recorded on another transaction; push left failed",
			append(attrs, slog.String("transaction_id", txn.ID.String()))...)
		return outcome{note: "receipt already recorded on transaction " + txn.ID.String(), txnID: &txn.ID}, nil
	}
	if push.CheckoutRequestID == nil {
		if push, err = q.SetSTKPushCheckout(ctx, store.SetSTKPushCheckoutParams{
			MerchantRequestID: optStr(truncate(r.MerchantRequestID, 128)), CheckoutRequestID: &r.CheckoutRequestID, ID: push.ID,
		}); err != nil {
			return outcome{}, fmt.Errorf("stk: attach checkout id: %w", err)
		}
	}
	// The money is on the new transaction: stk_push.succeeded (and every
	// later read of the push) must point apps at it, not the failed row.
	if push, err = q.SetSTKPushPaidTransaction(ctx, store.SetSTKPushPaidTransactionParams{PaidTransactionID: &txn.ID, ID: push.ID}); err != nil {
		return outcome{}, fmt.Errorf("stk: link paid transaction: %w", err)
	}
	if err := s.succeedPushTx(ctx, q, push, r.ResultDesc); err != nil {
		return outcome{}, err
	}
	// Late money is never dropped (rule 6): it pays the charge even if it
	// expired or was cancelled meanwhile.
	if err := s.payCharge(ctx, q, txn, lb, attrs); err != nil {
		return outcome{}, err
	}
	lb.warn("STK payment arrived after the push was given up; recorded as a new transaction",
		append(attrs, slog.String("transaction_id", txn.ID.String()), slog.Int64("amount", txn.Amount))...)
	return outcome{txnID: &txn.ID}, nil
}

// recordSecondPayment records an STK payment whose prompt had already been
// completed by a separate paybill payment: a new mpesa_stk transaction for
// the same charge (already succeeded, so it is reported as a duplicate).
func (s *Service) recordSecondPayment(ctx context.Context, q *store.Queries, cb store.MpesaCallback, push Push, first store.Transaction, r mpesa.STKResult, lb *logbuf) (outcome, error) {
	if r.Amount <= 0 {
		return outcome{note: "second payment without an amount; not recorded", txnID: &first.ID}, nil
	}
	provReq := r.CheckoutRequestID
	if _, err := q.GetTransactionByProviderRequest(ctx, store.GetTransactionByProviderRequestParams{
		Channel: store.TxnChannelMpesaStk, ProviderRequestID: &provReq,
	}); err == nil {
		provReq = ""
	} else if !db.IsNotFound(err) {
		return outcome{}, err
	}
	in := ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaStk, Receipt: r.Receipt, ProviderRequest: provReq, Amount: money.KES(r.Amount),
		Phone: usablePhone(r.Phone), AppID: &push.AppID, ChargeID: &push.ChargeID,
		OccurredAt: r.OccurredAt, RawPayload: cb.Raw,
	}
	if first.AccountNumber != nil {
		in.AccountNumber = *first.AccountNumber
	}
	txn, created, err := ledger.RecordIncoming(ctx, q, in)
	attrs := []any{slog.String("callback_id", cb.ID.String()), slog.String("stk_push_id", push.ID.String()),
		slog.String("first_transaction_id", first.ID.String()), slog.String("receipt", r.Receipt)}
	if errors.Is(err, ledger.ErrReceiptConflict) {
		lb.alarm("STK receipt already recorded with different details; not recorded", attrs...)
		return outcome{note: "receipt already recorded with different details", txnID: &first.ID}, nil
	}
	if err != nil {
		return outcome{}, err
	}
	if !created {
		return outcome{txnID: &txn.ID}, nil // duplicate
	}
	// The charge was already paid by the paybill payment: SucceedCharge
	// reports this one as a duplicate (payment.received, duplicate_of_charge).
	if err := s.payCharge(ctx, q, txn, lb, attrs); err != nil {
		return outcome{}, err
	}
	lb.warn("customer paid a prompt and a paybill payment of the same amount; both recorded",
		append(attrs, slog.String("transaction_id", txn.ID.String()), slog.Int64("amount", txn.Amount))...)
	return outcome{note: "second payment for an STK push already completed by a paybill payment", txnID: &txn.ID}, nil
}

// succeedPushTx moves a push to succeeded (once) and emits stk_push.succeeded.
func (s *Service) succeedPushTx(ctx context.Context, q *store.Queries, push Push, desc string) error {
	if push.Status == store.StkPushStatusSucceeded {
		return nil
	}
	code := mpesa.ResultSuccess
	out, err := q.FinishSTKPush(ctx, store.FinishSTKPushParams{
		Status: store.StkPushStatusSucceeded, ResultCode: &code, ResultDesc: optStr(truncate(desc, 500)), ID: push.ID,
	})
	if err != nil {
		return fmt.Errorf("stk: push succeeded: %w", err)
	}
	return s.o.Events.EmitSTKPush(ctx, q, out.AppID, out.ID, events.STKPushSucceeded)
}

// pushForSuccess finds (and locks) the push a success callback is about: by
// checkout id, else a single orphan push (no checkout id stored) with the
// same phone and amount. why explains a miss.
func (s *Service) pushForSuccess(ctx context.Context, q *store.Queries, cb store.MpesaCallback, r mpesa.STKResult, lb *logbuf) (push Push, found bool, why string, err error) {
	checkout := r.CheckoutRequestID
	push, err = q.LockSTKPushByCheckout(ctx, &checkout)
	if err == nil {
		return push, true, "", nil
	}
	if !db.IsNotFound(err) {
		return Push{}, false, "", err
	}
	why = "no push with this checkout id"
	if ph, perr := phone.Normalize(r.Phone); perr == nil {
		cands, err := q.FindOrphanSTKPushes(ctx, store.FindOrphanSTKPushesParams{Phone: ph, Amount: r.Amount})
		if err != nil {
			return Push{}, false, "", err
		}
		chosen, pickWhy := pickOrphan(cands)
		switch {
		case len(cands) == 0:
			why = "no push with this checkout id or an orphan push with this phone and amount"
		case chosen != nil:
			p, err := q.LockSTKPush(ctx, chosen.ID)
			if err != nil {
				return Push{}, false, "", err
			}
			if p.CheckoutRequestID == nil {
				attrs := []any{slog.String("callback_id", cb.ID.String()), slog.String("stk_push_id", p.ID.String()),
					slog.String("checkout_request_id", checkout), slog.String("push_status", string(p.Status))}
				switch p.Status {
				case store.StkPushStatusInitiating:
					p, err = s.sendPushTx(ctx, q, p, optStr(truncate(r.MerchantRequestID, 128)), checkout)
					if err != nil {
						return Push{}, false, "", err
					}
					lb.warn("STK callback matched to an orphan push by phone and amount", attrs...)
					return p, true, "", nil
				case store.StkPushStatusFailed:
					lb.warn("STK callback matched to an orphan push we had failed, by phone and amount", attrs...)
					return p, true, "", nil // successAfterGiveUp attaches the checkout id
				}
			}
			// It got a checkout id meanwhile: fall through to the re-lookup.
		default:
			why = pickWhy
		}
	} else {
		why = "no push with this checkout id; callback phone unusable for orphan matching"
	}
	// Send's Tx B may have committed between the first lookup and the orphan
	// search (the push then has this checkout id).
	push, err = q.LockSTKPushByCheckout(ctx, &checkout)
	if err == nil {
		return push, true, "", nil
	}
	if !db.IsNotFound(err) {
		return Push{}, false, "", err
	}
	return Push{}, false, why, nil
}

// pickOrphan chooses the push an orphan success callback belongs to among
// candidates with the same phone and amount (newest first). A customer who
// retried after a lost Daraja answer leaves a failed push and a newer
// initiating one for the SAME charge: the single initiating push wins; if
// all are failed but for one charge, the newest is used. Anything else is
// ambiguous (the payment is then recorded unmatched for staff).
func pickOrphan(cands []store.StkPush) (*store.StkPush, string) {
	if len(cands) == 0 {
		return nil, ""
	}
	var live []store.StkPush
	sameCharge := true
	for _, c := range cands {
		if c.Status == store.StkPushStatusInitiating {
			live = append(live, c)
		}
		if c.ChargeID != cands[0].ChargeID {
			sameCharge = false
		}
	}
	switch {
	case len(live) == 1:
		return &live[0], ""
	case len(live) > 1:
		return nil, "several live orphan pushes with this phone and amount"
	case sameCharge:
		return &cands[0], ""
	}
	return nil, "several orphan pushes for different charges with this phone and amount"
}

// recordUnmatchedSTK records a successful STK payment we cannot tie to a
// push: unmatched (no app), with an alarm.
func (s *Service) recordUnmatchedSTK(ctx context.Context, q *store.Queries, cb store.MpesaCallback, r mpesa.STKResult, lb *logbuf, why string) (outcome, error) {
	provReq := r.CheckoutRequestID
	if _, err := q.GetTransactionByProviderRequest(ctx, store.GetTransactionByProviderRequestParams{
		Channel: store.TxnChannelMpesaStk, ProviderRequestID: &provReq,
	}); err == nil {
		provReq = ""
	} else if !db.IsNotFound(err) {
		return outcome{}, err
	}
	txn, created, err := ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaStk, Receipt: r.Receipt, ProviderRequest: provReq, Amount: money.KES(r.Amount),
		Phone: usablePhone(r.Phone), OccurredAt: r.OccurredAt, RawPayload: cb.Raw,
	})
	attrs := []any{slog.String("callback_id", cb.ID.String()), slog.String("checkout_request_id", r.CheckoutRequestID),
		slog.String("receipt", r.Receipt), slog.Int64("amount", r.Amount), slog.String("phone", tlog.MaskPhone(r.Phone))}
	if errors.Is(err, ledger.ErrReceiptConflict) {
		lb.alarm("STK receipt already recorded with different details; not recorded", attrs...)
		return outcome{note: "receipt already recorded with different details", txnID: &txn.ID}, nil
	}
	if err != nil {
		return outcome{}, err
	}
	if !created {
		return outcome{txnID: &txn.ID}, nil // duplicate receipt
	}
	lb.alarm("STK payment that matches no push recorded unmatched", append(attrs, slog.String("transaction_id", txn.ID.String()), slog.String("why", why))...)
	return outcome{note: "unmatched STK payment held for staff: " + why, txnID: &txn.ID}, nil
}

// ---------------------------------------------------------------------------
// C2B confirmations
// ---------------------------------------------------------------------------

func (s *Service) processC2B(ctx context.Context, q *store.Queries, cb store.MpesaCallback, lb *logbuf) (outcome, error) {
	cbAttr := slog.String("callback_id", cb.ID.String())
	c, perr := mpesa.ParseC2BConfirmation(cb.Raw)
	if perr != nil {
		lb.alarm("unusable C2B confirmation; not recorded", cbAttr, slog.String("error", perr.Error()))
		return outcome{note: "unusable C2B confirmation: " + perr.Error()}, nil
	}
	attrs := []any{cbAttr, slog.String("receipt", c.Receipt), slog.Int64("amount", c.Amount), slog.String("bill_ref", c.BillRef)}
	if sc := s.paybillShortCode(); sc != "" && c.ShortCode != sc {
		// Not our paybill: recording it would put someone else's money in
		// our ledger. Staff check it.
		lb.alarm("C2B confirmation for another short code; not recorded", append(attrs, slog.String("short_code", c.ShortCode))...)
		return outcome{note: "C2B confirmation for short code " + truncate(c.ShortCode, 32) + " (not our paybill); not recorded"}, nil
	}
	// A receipt already in the ledger (a duplicate notice, or the STK
	// callback for the same payment got here first) is never recorded again.
	if existing, err := q.GetTransactionByReceipt(ctx, store.GetTransactionByReceiptParams{Provider: "mpesa", ProviderTransactionID: &c.Receipt}); err == nil {
		attrs = append(attrs, slog.String("transaction_id", existing.ID.String()))
		if existing.Amount != c.Amount || existing.Direction != store.TxnDirectionIn {
			lb.alarm("C2B receipt already recorded with different details; not recorded", attrs...)
			return outcome{note: "receipt already recorded with different details", txnID: &existing.ID}, nil
		}
		lb.info("duplicate C2B receipt ignored", attrs...)
		return outcome{note: "duplicate receipt", txnID: &existing.ID}, nil
	} else if !db.IsNotFound(err) {
		return outcome{}, err
	}

	// Route by account-number prefix (money rule 7).
	var (
		appID     *uuid.UUID
		appActive bool
	)
	if ref, err := accountref.Parse(c.BillRef); err == nil {
		a, err := q.GetAppByPrefix(ctx, ref.Prefix)
		switch {
		case err == nil:
			appID, appActive = &a.ID, a.Status == store.AppStatusActive
		case !db.IsNotFound(err):
			return outcome{}, err
		}
	}
	in := ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaC2b, Receipt: c.Receipt, Amount: money.KES(c.Amount), Phone: usablePhone(c.Phone),
		AccountNumber: c.BillRef, AppID: appID, OccurredAt: c.OccurredAt, RawPayload: cb.Raw,
	}
	if appID == nil {
		// Unknown prefix: held unmatched for staff (no app, no event).
		txn, created, err := ledger.RecordIncoming(ctx, q, in)
		res, err := c2bRecorded(txn, created, err, lb, attrs)
		if err == nil && created {
			lb.warn("C2B payment to an unknown account prefix held unmatched", append(attrs, slog.String("transaction_id", txn.ID.String()))...)
			res.note = "unknown account prefix; held unmatched"
		}
		return res, err
	}
	attrs = append(attrs, slog.String("app_id", appID.String()))
	if !appActive {
		lb.warn("C2B payment for an inactive app recorded and forwarded as normal", attrs...)
	}

	// Exactly one live charge with this account number and amount: the
	// money pays it. Otherwise the app decides (payment.received).
	match, err := ledger.MatchLiveCharge(ctx, q, *appID, c.BillRef, c.Amount)
	if err != nil {
		return outcome{}, err
	}
	if match == nil {
		txn, created, err := ledger.RecordIncoming(ctx, q, in)
		res, err := c2bRecorded(txn, created, err, lb, attrs)
		if err != nil || !created {
			return res, err
		}
		if err := s.o.Events.EmitPaymentReceived(ctx, q, *appID, txn.ID, nil); err != nil {
			return outcome{}, err
		}
		lb.info("c2b payment forwarded to the app (no single waiting charge)", append(attrs, slog.String("transaction_id", txn.ID.String()))...)
		return res, nil
	}
	attrs = append(attrs, slog.String("charge_id", match.ID.String()))
	if res, matched, err := s.c2bForWaitingPrompt(ctx, q, cb, c, *match, lb, attrs); err != nil || matched {
		return res, err
	}
	in.ChargeID = &match.ID
	txn, created, err := ledger.RecordIncoming(ctx, q, in)
	res, err := c2bRecorded(txn, created, err, lb, attrs)
	if err != nil || !created {
		return res, err
	}
	if err := s.payCharge(ctx, q, txn, lb, attrs); err != nil {
		return outcome{}, err
	}
	lb.info("c2b payment received for a charge", append(attrs, slog.String("transaction_id", txn.ID.String()))...)
	return res, nil
}

// c2bRecorded handles RecordIncoming's outcome for a C2B payment: a receipt
// conflict or a duplicate (lost race with another notice for the same
// receipt) is final and emits nothing.
func c2bRecorded(txn store.Transaction, created bool, err error, lb *logbuf, attrs []any) (outcome, error) {
	if errors.Is(err, ledger.ErrReceiptConflict) {
		lb.alarm("C2B receipt already recorded with different details; not recorded", append(attrs, slog.String("transaction_id", txn.ID.String()))...)
		return outcome{note: "receipt already recorded with different details", txnID: &txn.ID}, nil
	}
	if err != nil {
		return outcome{}, err
	}
	if !created {
		lb.info("duplicate C2B receipt ignored", append(attrs, slog.String("transaction_id", txn.ID.String()))...)
		return outcome{note: "duplicate receipt", txnID: &txn.ID}, nil
	}
	return outcome{txnID: &txn.ID}, nil
}

// c2bForWaitingPrompt: some paybills send a C2B confirmation for STK
// payments too. The confirmation matched exactly one live charge (ch); if
// that charge has exactly one prompt still waiting (or in review) for this
// amount whose transaction has no receipt yet, the confirmation IS that
// prompt's payment (user decision 2026-11-24): complete the prompt's
// transaction with this receipt, so the STK callback carrying the same
// receipt is then a duplicate. matched=false: record a new C2B payment for
// the charge instead.
func (s *Service) c2bForWaitingPrompt(ctx context.Context, q *store.Queries, cb store.MpesaCallback, c mpesa.C2BConfirmation, ch store.Charge, lb *logbuf, attrs []any) (outcome, bool, error) {
	var ph *string
	if p := usablePhone(c.Phone); p != "" {
		ph = &p
	}
	cands, err := q.FindLiveSTKPushesForC2B(ctx, store.FindLiveSTKPushesForC2BParams{
		AppID: ch.AppID, AccountNumberNorm: ch.AccountNumberNorm, Amount: c.Amount, Phone: ph,
	})
	if err != nil {
		return outcome{}, false, err
	}
	if len(cands) != 1 || cands[0].ChargeID != ch.ID {
		if len(cands) > 1 {
			lb.warn("C2B payment matches several waiting prompts; recorded as its own payment", attrs...)
		}
		return outcome{}, false, nil
	}
	// Lock order: push, then transaction (as everywhere else). Re-check
	// after locking: a callback or the sweeper may have moved it on.
	push, err := q.LockSTKPush(ctx, cands[0].ID)
	if err != nil {
		return outcome{}, false, err
	}
	txn, err := q.LockTransactionInternal(ctx, push.TransactionID)
	if err != nil {
		return outcome{}, false, err
	}
	switch push.Status {
	case store.StkPushStatusInitiating, store.StkPushStatusSent, store.StkPushStatusUnknown:
	default:
		return outcome{}, false, nil
	}
	if push.ConfirmBlocked != nil || txn.Amount != c.Amount || txn.ChargeID == nil || *txn.ChargeID != ch.ID ||
		(txn.Status != store.TxnStatusPending && txn.Status != store.TxnStatusNeedsReview) {
		return outcome{}, false, nil
	}
	done, err := ledger.CompleteSTK(ctx, q, ledger.CompleteSTKParams{
		TransactionID: txn.ID, Receipt: c.Receipt, Phone: c.Phone, OccurredAt: c.OccurredAt, RawPayload: cb.Raw,
	})
	if err != nil {
		return outcome{}, false, err
	}
	if err := s.succeedPushTx(ctx, q, push, "Confirmed by Safaricom's paybill notice (receipt "+c.Receipt+")"); err != nil {
		return outcome{}, false, err
	}
	attrs = append(attrs, slog.String("stk_push_id", push.ID.String()))
	if err := s.payCharge(ctx, q, done, lb, attrs); err != nil {
		return outcome{}, false, err
	}
	lb.info("c2b confirmation completed a waiting stk push", append(attrs, slog.String("transaction_id", done.ID.String()))...)
	return outcome{note: "completed STK push " + push.ID.String(), txnID: &done.ID}, true, nil
}

// completedByC2B reports whether a succeeded STK transaction got its
// receipt from a paybill (C2B) confirmation rather than the STK callback
// or staff (its stored payload is then Safaricom's C2B body).
func completedByC2B(t store.Transaction) bool {
	if len(t.RawPayload) == 0 {
		return false
	}
	var probe struct {
		TransID *string `json:"TransID"`
	}
	return json.Unmarshal(t.RawPayload, &probe) == nil && probe.TransID != nil
}

// payCharge applies a succeeded payment to the charge it names (if any)
// with ledger.SucceedCharge, which emits charge.succeeded, or
// payment.received for a second payment (duplicate_of_charge) or an amount
// that differs (staff warned). Money for an inactive app is still applied
// and forwarded, with a warning (rule 8). Callers move the push to
// succeeded first, so charge.succeeded carries the succeeded prompt.
func (s *Service) payCharge(ctx context.Context, q *store.Queries, txn store.Transaction, lb *logbuf, attrs []any) error {
	if txn.ChargeID == nil {
		return nil
	}
	res, err := ledger.SucceedCharge(ctx, q, s.o.Events, *txn.ChargeID, txn.ID)
	if err != nil {
		return err
	}
	attrs = append(attrs, slog.String("charge_id", txn.ChargeID.String()), slog.String("transaction_id", txn.ID.String()))
	switch {
	case res.AmountMismatch:
		lb.alarm("payment amount differs from its charge; forwarded as payment.received, charge left unpaid",
			append(attrs, slog.Int64("amount", txn.Amount), slog.Int64("charge_amount", res.Charge.Amount))...)
	case res.Duplicate:
		lb.warn("second payment for a charge that already succeeded; forwarded as payment.received (duplicate_of_charge)", attrs...)
	}
	if txn.AppID != nil {
		a, err := q.GetApp(ctx, *txn.AppID)
		if err != nil {
			return err
		}
		if a.Status != store.AppStatusActive {
			lb.warn("payment for an inactive app recorded and forwarded as normal", attrs...)
		}
	}
	return nil
}

// usablePhone returns p normalised, or "" if it is not a usable number
// (Safaricom may mask or hash it).
func usablePhone(p string) string {
	n, err := phone.Normalize(p)
	if err != nil {
		return ""
	}
	return n
}
