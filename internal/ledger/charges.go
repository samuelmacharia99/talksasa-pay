package ledger

// Paying charges (money rules 3, 6 and 7), staff reversals (rule 9) and staff
// assignment of unmatched payments to an app.
//
// Lock order everywhere: stk push -> transaction -> charge. Functions taking
// q run inside the caller's database transaction, so the money change, the
// events describing it and the audit entry commit (or roll back) together.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	// ErrNotFound: no transaction with this id.
	ErrNotFound = errors.New("ledger: transaction not found")
	// ErrAppNotFound: AssignToApp named an app that does not exist.
	ErrAppNotFound = errors.New("ledger: app not found")
	// ErrAlreadyAssigned: the payment already belongs to an app (set once).
	ErrAlreadyAssigned = errors.New("ledger: transaction is already routed to an app")
	// ErrNotAssignable: only succeeded incoming payments can be assigned.
	ErrNotAssignable = errors.New("ledger: only succeeded incoming payments can be assigned to an app")
	// ErrNotPaying: SucceedCharge was given a transaction that cannot pay
	// the charge (not a succeeded, receipted incoming payment naming it).
	// A programming error: callers only pass payments they just recorded.
	ErrNotPaying = errors.New("ledger: transaction is not a succeeded payment for this charge")
	// ErrNoEvents: the service was built without an event emitter.
	ErrNoEvents = errors.New("ledger: no event emitter configured")
)

// ErrNotImplemented is kept for callers written against the gateway-core
// stubs; nothing returns it any more.
//
// Callers may drop it: every ledger operation is implemented.
var ErrNotImplemented = errors.New("ledger: not implemented yet (gateway-core)")

// SucceedResult says what SucceedCharge did with a payment.
type SucceedResult struct {
	// Charge is the charge after the call.
	Charge store.Charge
	// Succeeded: this payment made the charge succeeded (charge.succeeded
	// was emitted).
	Succeeded bool
	// Duplicate: the charge had already succeeded (or was reversed) with
	// another payment. The charge is left alone and this payment was
	// reported as payment.received with duplicate_of_charge = Charge.ID
	// (the customer paid twice; the app or staff refund it).
	Duplicate bool
	// AmountMismatch: the payment's amount differs from the charge amount,
	// so it cannot pay it. The charge is left alone and the payment was
	// reported as payment.received (its charge_id names the charge). Staff
	// must be warned.
	AmountMismatch bool
}

// SucceedCharge applies a succeeded, receipted payment (txnID, whose
// charge_id is chargeID) to its charge under a row lock, in q's transaction,
// and emits what the app must hear (money rules 3 and 6):
//
//   - charge pending, needs_review, expired or cancelled and the payment is
//     for exactly the charge amount: the charge becomes succeeded, emit
//     charge.succeeded;
//   - charge already succeeded or reversed (by another payment): the charge
//     is left alone, emit payment.received with duplicate_of_charge;
//   - amount differs: the charge is left alone, emit payment.received
//     naming the charge (AmountMismatch; the caller warns staff).
//
// Calling it again for the payment that already paid the charge is a no-op.
// The charges_guard trigger re-checks app, charge, receipt and exact amount.
func SucceedCharge(ctx context.Context, q *store.Queries, em *events.Emitter, chargeID, txnID uuid.UUID) (SucceedResult, error) {
	if em == nil {
		return SucceedResult{}, ErrNoEvents
	}
	c, err := q.LockChargeInternal(ctx, chargeID)
	if err != nil {
		return SucceedResult{}, fmt.Errorf("ledger: lock charge: %w", err)
	}
	t, err := q.GetTransactionInternal(ctx, txnID)
	if err != nil {
		return SucceedResult{}, fmt.Errorf("ledger: load transaction: %w", err)
	}
	if t.Kind != store.TxnKindPayment || t.Direction != store.TxnDirectionIn || t.Status != store.TxnStatusSucceeded ||
		t.ChargeID == nil || *t.ChargeID != c.ID || t.AppID == nil || *t.AppID != c.AppID ||
		(t.Provider == "mpesa" && t.ProviderTransactionID == nil) {
		return SucceedResult{}, ErrNotPaying
	}
	res := SucceedResult{Charge: c}
	switch {
	case c.TransactionID != nil && *c.TransactionID == t.ID:
		return res, nil // already applied
	case c.Status == store.ChargeStatusSucceeded || c.Status == store.ChargeStatusReversed:
		res.Duplicate = true
		return res, em.EmitPaymentReceived(ctx, q, c.AppID, t.ID, &c.ID)
	case t.Amount != c.Amount:
		res.AmountMismatch = true
		return res, em.EmitPaymentReceived(ctx, q, c.AppID, t.ID, nil)
	}
	paid, err := q.MarkChargeSucceeded(ctx, store.MarkChargeSucceededParams{TransactionID: &t.ID, ID: c.ID})
	if err != nil {
		return SucceedResult{}, fmt.Errorf("ledger: mark charge succeeded: %w", err)
	}
	res.Charge, res.Succeeded = paid, true
	return res, em.EmitCharge(ctx, q, c.AppID, c.ID, events.ChargeSucceeded)
}

// MatchLiveCharge is the paybill matching rule (money rule 7), shared by C2B
// processing and staff assignment: the app's single live (pending or
// needs_review) charge whose account number equals what the customer typed
// (normalised) and whose amount is exactly amount. nil when there is no
// match or more than one (the payment is then forwarded as
// payment.received and the app decides).
func MatchLiveCharge(ctx context.Context, q *store.Queries, appID uuid.UUID, typed string, amount int64) (*store.Charge, error) {
	norm := accountref.Normalize(typed)
	if norm == "" || amount <= 0 {
		return nil, nil
	}
	cands, err := q.FindLiveChargesByAccount(ctx, store.FindLiveChargesByAccountParams{
		AppID: appID, AccountNumberNorm: norm, Amount: amount,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: match live charge: %w", err)
	}
	if len(cands) != 1 {
		return nil, nil
	}
	return &cands[0], nil
}

// Service performs ledger operations that need their own database
// transaction (staff corrections).
type Service struct {
	pool   *db.Pool
	logger *slog.Logger
	events *events.Emitter
}

// NewService builds a ledger service. Reverse and AssignToApp also need an
// event emitter: chain WithEvents.
func NewService(pool *db.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, logger: logger}
}

// WithEvents sets the emitter used for transaction.reversed,
// payment.received and charge.succeeded, and returns s.
func (s *Service) WithEvents(em *events.Emitter) *Service {
	s.events = em
	return s
}

// ReverseParams describes a staff reversal of a recorded payment (money
// rule 9), e.g. after Safaricom reverses a transaction.
type ReverseParams struct {
	OriginalID uuid.UUID
	Reason     string
	// ProviderReceipt is Safaricom's reversal receipt, when staff have it
	// (optional; recorded once like every receipt).
	ProviderReceipt string
}

// ReverseResult is what Reverse recorded.
type ReverseResult struct {
	Reversal Transaction
	// Charge is the charge the original payment paid (now reversed), if any.
	// nil when the payment paid no charge (forwarded, unmatched or a
	// duplicate payment).
	Charge *store.Charge
}

// Reverse records a reversal transaction against a succeeded incoming
// payment. If that payment is the one that paid a charge, the charge becomes
// reversed. The app gets transaction.reversed; the action is audited. Admin
// (or CLI operator) only, with a reason of 3-500 characters. There is no
// credit or reopening logic. Reversing twice is ErrAlreadyReversed (the
// original is locked, and transactions_one_reversal backs it up).
func (s *Service) Reverse(ctx context.Context, actor audit.Actor, p ReverseParams) (ReverseResult, error) {
	if actor.Type != store.AuditActorTypeAdmin && actor.Type != store.AuditActorTypeCli {
		return ReverseResult{}, ErrActorRequired
	}
	if err := actor.Validate(); err != nil {
		return ReverseResult{}, ErrActorRequired
	}
	reason := strings.TrimSpace(p.Reason)
	if n := utf8.RuneCountInString(reason); n < 3 || n > 500 {
		return ReverseResult{}, ErrReasonRequired
	}
	receipt := strings.ToUpper(strings.TrimSpace(p.ProviderReceipt))
	if receipt != "" && !validReceipt(receipt) {
		return ReverseResult{}, ErrInvalidReceipt
	}
	if s.events == nil {
		return ReverseResult{}, ErrNoEvents
	}
	var out ReverseResult
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		out = ReverseResult{}
		orig, err := q.LockTransactionInternal(ctx, p.OriginalID)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if orig.Kind != store.TxnKindPayment || orig.Direction != store.TxnDirectionIn || orig.Status != store.TxnStatusSucceeded {
			return ErrNotReversible
		}
		if _, err := q.GetReversalOf(ctx, &orig.ID); err == nil {
			return ErrAlreadyReversed
		} else if !db.IsNotFound(err) {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		now := time.Now()
		by := actor.String()
		rev, err := q.InsertTransaction(ctx, store.InsertTransactionParams{
			ID: id, AppID: orig.AppID, ChargeID: orig.ChargeID, Direction: store.TxnDirectionOut, Kind: store.TxnKindReversal,
			Channel: orig.Channel, Provider: orig.Provider, Status: store.TxnStatusSucceeded, Amount: orig.Amount,
			ProviderTransactionID: optStr(receipt), Phone: orig.Phone, AccountNumber: orig.AccountNumber,
			ReversesTransactionID: &orig.ID, Reason: &reason, CreatedBy: &by, CompletedAt: &now,
		})
		if db.IsNotFound(err) {
			return ErrReceiptConflict // the reversal receipt is already recorded
		}
		if err != nil {
			return err
		}
		out.Reversal = rev
		if orig.ChargeID != nil {
			c, err := q.LockChargeInternal(ctx, *orig.ChargeID)
			if err != nil {
				return err
			}
			if c.Status == store.ChargeStatusSucceeded && c.TransactionID != nil && *c.TransactionID == orig.ID {
				c, err = q.MarkChargeReversed(ctx, c.ID)
				if err != nil {
					return fmt.Errorf("ledger: mark charge reversed: %w", err)
				}
				out.Charge = &c
			}
		}
		if orig.AppID != nil {
			if err := s.events.EmitTransaction(ctx, q, *orig.AppID, rev.ID, events.TransactionReversed); err != nil {
				return err
			}
		}
		details := map[string]any{
			"reversal_transaction_id": rev.ID.String(), "reason": reason, "amount": orig.Amount,
		}
		if receipt != "" {
			details["reversal_receipt"] = receipt
		}
		if orig.ChargeID != nil {
			details["charge_id"] = orig.ChargeID.String()
			details["charge_reversed"] = out.Charge != nil
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "transaction.reversed", EntityType: "transaction", EntityID: orig.ID.String(),
			AppID: orig.AppID, Details: details,
		})
	})
	if db.IsUniqueViolation(err, "transactions_one_reversal") {
		return ReverseResult{}, ErrAlreadyReversed
	}
	if db.IsUniqueViolation(err, "transactions_provider_receipt_uq") {
		return ReverseResult{}, ErrReceiptConflict
	}
	if err != nil {
		return ReverseResult{}, err
	}
	s.logger.WarnContext(ctx, "payment reversed by staff", slog.String("transaction_id", p.OriginalID.String()),
		slog.String("reversal_id", out.Reversal.ID.String()), slog.String("actor", actor.String()))
	return out, nil
}

// AssignToApp is staff routing an unmatched payment (app_id NULL, succeeded,
// incoming) to an app: app_id is set once and the action audited. If the
// payment's account number matches exactly one live charge of that app for
// exactly its amount (MatchLiveCharge, the paybill rule), it pays that
// charge (charge.succeeded); otherwise the app gets payment.received.
// Returns the updated transaction.
func (s *Service) AssignToApp(ctx context.Context, actor audit.Actor, txnID, appID uuid.UUID, reason string) (Transaction, error) {
	if err := actor.Validate(); err != nil {
		return Transaction{}, ErrActorRequired
	}
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 3 || n > 500 {
		return Transaction{}, ErrReasonRequired
	}
	if s.events == nil {
		return Transaction{}, ErrNoEvents
	}
	var (
		out      Transaction
		inactive bool
	)
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		t, err := q.LockTransactionInternal(ctx, txnID)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.AppID != nil {
			return ErrAlreadyAssigned
		}
		if t.Kind != store.TxnKindPayment || t.Direction != store.TxnDirectionIn || t.Status != store.TxnStatusSucceeded {
			return ErrNotAssignable
		}
		a, err := q.GetApp(ctx, appID)
		if db.IsNotFound(err) {
			return ErrAppNotFound
		}
		if err != nil {
			return err
		}
		inactive = a.Status != store.AppStatusActive
		if out, err = q.AssignTransactionApp(ctx, store.AssignTransactionAppParams{AppID: &appID, ID: t.ID}); err != nil {
			if db.IsNotFound(err) {
				return ErrAlreadyAssigned
			}
			return err
		}
		var typed string
		if t.AccountNumber != nil {
			typed = *t.AccountNumber
		}
		match, err := MatchLiveCharge(ctx, q, appID, typed, t.Amount)
		if err != nil {
			return err
		}
		details := map[string]any{"app_id": appID.String(), "reason": reason, "amount": t.Amount}
		if match != nil {
			if out, err = q.SetTransactionCharge(ctx, store.SetTransactionChargeParams{ChargeID: &match.ID, ID: t.ID, AppID: &appID}); err != nil {
				return fmt.Errorf("ledger: name charge: %w", err)
			}
			res, err := SucceedCharge(ctx, q, s.events, match.ID, t.ID)
			if err != nil {
				return err
			}
			details["charge_id"] = match.ID.String()
			details["charge_succeeded"] = res.Succeeded
		} else if err := s.events.EmitPaymentReceived(ctx, q, appID, t.ID, nil); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "transaction.assigned", EntityType: "transaction", EntityID: t.ID.String(),
			AppID: &appID, Details: details,
		})
	})
	if err != nil {
		return Transaction{}, err
	}
	attrs := []any{slog.String("transaction_id", txnID.String()), slog.String("app_id", appID.String()), slog.String("actor", actor.String())}
	if inactive {
		s.logger.WarnContext(ctx, "unmatched payment assigned to an inactive app; forwarded as normal", attrs...)
	} else {
		s.logger.InfoContext(ctx, "unmatched payment assigned to an app", attrs...)
	}
	return out, nil
}
