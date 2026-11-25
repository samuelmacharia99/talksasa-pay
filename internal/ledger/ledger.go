// Package ledger records money movements in the append-only transactions
// table. It never updates an amount and never deletes; corrections are new
// reversal or adjustment rows that reference the original and carry a reason
// and the person who made them.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	ErrInvalidChannel  = errors.New("ledger: channel not valid for an incoming payment")
	ErrMissingReceipt  = errors.New("ledger: a succeeded M-Pesa payment needs its receipt number")
	ErrInvalidReceipt  = errors.New("ledger: receipt must be 1-64 letters or digits")
	ErrPayloadNotJSON  = errors.New("ledger: raw payload must be a JSON object")
	ErrNotReversible   = errors.New("ledger: only succeeded incoming payments can be reversed")
	ErrAlreadyReversed = errors.New("ledger: transaction has already been reversed")
	ErrReasonRequired  = errors.New("ledger: a reason of at least 3 characters is required")
	ErrActorRequired   = errors.New("ledger: corrections must be made by a named person")
	ErrReceiptConflict = errors.New("ledger: receipt already recorded with different details")
	// ErrReversalReceipt: an M-Pesa reversal must carry Safaricom's reversal
	// receipt so it can be matched against the paybill statement.
	ErrReversalReceipt = errors.New("ledger: the M-Pesa reversal receipt number is required")
)

// IncomingPayment is a confirmed payment as reported by a provider.
type IncomingPayment struct {
	Channel         store.TxnChannel // mpesa_stk | mpesa_c2b
	Receipt         string           // M-Pesa receipt, e.g. SKL4ABCD12
	ProviderRequest string           // CheckoutRequestID (STK), empty for C2B
	Amount          money.KES
	Phone           string     // raw; normalised here (C2B may send a masked/hashed value: pass "")
	AccountNumber   string     // account reference as typed / sent with the prompt
	AppID           *uuid.UUID // nil: unmatched (unknown prefix)
	ChargeID        *uuid.UUID // the charge the money was for, when known
	OccurredAt      *time.Time
	RawPayload      json.RawMessage // provider payload, already free of secrets
}

// Transaction is the service-level view of a ledger row.
type Transaction = store.Transaction

// RecordIncoming stores a succeeded incoming payment. If the receipt is
// already in the ledger it returns the existing row with created=false and
// writes nothing: duplicate callbacks are harmless.
func RecordIncoming(ctx context.Context, q *store.Queries, in IncomingPayment) (txn Transaction, created bool, err error) {
	if in.Channel != store.TxnChannelMpesaStk && in.Channel != store.TxnChannelMpesaC2b {
		return Transaction{}, false, ErrInvalidChannel
	}
	if in.Amount <= 0 || in.Amount > money.MaxKES {
		return Transaction{}, false, money.ErrNonPositive
	}
	receipt := strings.ToUpper(strings.TrimSpace(in.Receipt))
	if receipt == "" {
		return Transaction{}, false, ErrMissingReceipt
	}
	if !validReceipt(receipt) {
		return Transaction{}, false, ErrInvalidReceipt
	}
	var ph *string
	if strings.TrimSpace(in.Phone) != "" {
		n, err := phone.Normalize(in.Phone)
		if err != nil {
			return Transaction{}, false, err
		}
		ph = &n
	}
	if len(in.RawPayload) > 0 && !json.Valid(in.RawPayload) {
		return Transaction{}, false, ErrPayloadNotJSON
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Transaction{}, false, err
	}
	now := time.Now()
	row, err := q.InsertTransaction(ctx, store.InsertTransactionParams{
		ID: id, AppID: in.AppID, ChargeID: in.ChargeID, Direction: store.TxnDirectionIn, Kind: store.TxnKindPayment,
		Channel: in.Channel, Provider: "mpesa", Status: store.TxnStatusSucceeded, Amount: int64(in.Amount),
		ProviderTransactionID: &receipt, ProviderRequestID: optStr(in.ProviderRequest), Phone: ph,
		AccountNumber: optStr(truncate(strings.TrimSpace(in.AccountNumber), 64)), RawPayload: in.RawPayload,
		OccurredAt: in.OccurredAt, CompletedAt: &now,
	})
	if db.IsNotFound(err) {
		existing, gerr := q.GetTransactionByReceipt(ctx, store.GetTransactionByReceiptParams{Provider: "mpesa", ProviderTransactionID: &receipt})
		if gerr != nil {
			return Transaction{}, false, fmt.Errorf("ledger: load duplicate receipt: %w", gerr)
		}
		// Same receipt with a different amount means something is badly
		// wrong upstream; never silently accept it.
		if existing.Amount != int64(in.Amount) || existing.Direction != store.TxnDirectionIn {
			return existing, false, ErrReceiptConflict
		}
		return existing, false, nil
	}
	if err != nil {
		return Transaction{}, false, fmt.Errorf("ledger: insert transaction: %w", err)
	}
	return row, true, nil
}

// ErrNotImplemented marks the gateway-core stubs below.
// TODO(gateway-core): remove once SucceedCharge and Reverse are implemented.
var ErrNotImplemented = errors.New("ledger: not implemented yet (gateway-core)")

// SucceedResult says what SucceedCharge did with a payment.
type SucceedResult struct {
	// Charge is the charge after the call.
	Charge store.Charge
	// Succeeded: this payment made the charge succeeded (emit
	// charge.succeeded). False when the charge had already succeeded with
	// another transaction: the payment is a duplicate (emit
	// payment.received with duplicate_of_charge = Charge.ID).
	Succeeded bool
}

// SucceedCharge applies a succeeded, receipted payment (txnID, whose
// charge_id is chargeID) to its charge under a row lock, in q's transaction.
// Money rules 3 and 6: a pending, needs_review, expired or cancelled charge
// becomes succeeded; an already succeeded (or reversed) charge is left alone
// and the payment is reported as a duplicate. The charges_guard trigger
// re-checks app, charge, receipt and exact amount.
func SucceedCharge(ctx context.Context, q *store.Queries, chargeID, txnID uuid.UUID) (SucceedResult, error) {
	// TODO(gateway-core): LockChargeInternal, check txn (succeeded, receipt,
	// same app/charge, exact amount), MarkChargeSucceeded or report duplicate.
	return SucceedResult{}, ErrNotImplemented
}

// Service performs ledger operations that need their own database transaction.
type Service struct {
	pool   *db.Pool
	logger *slog.Logger
}

// NewService builds a ledger service.
func NewService(pool *db.Pool, logger *slog.Logger) *Service {
	return &Service{pool: pool, logger: logger}
}

// ReverseParams describes a staff reversal of a recorded payment (money
// rule 9), e.g. after Safaricom reverses a transaction.
type ReverseParams struct {
	OriginalID      uuid.UUID
	Reason          string
	ProviderReceipt string // Safaricom's reversal receipt; required for M-Pesa payments
}

// ReverseResult is what Reverse recorded.
type ReverseResult struct {
	Reversal Transaction
	// Charge is the charge the original payment paid (now reversed), if any.
	Charge *store.Charge
}

// Reverse records a reversal transaction against a succeeded incoming
// payment. If that payment paid a charge, the charge becomes reversed. The
// app gets transaction.reversed; the action is audited. Admin (or CLI
// operator) only, with a reason. There is no credit or reopening logic.
func (s *Service) Reverse(ctx context.Context, actor audit.Actor, p ReverseParams) (ReverseResult, error) {
	if actor.Type != store.AuditActorTypeAdmin && actor.Type != store.AuditActorTypeCli {
		return ReverseResult{}, ErrActorRequired
	}
	reason := strings.TrimSpace(p.Reason)
	if len(reason) < 3 || len(reason) > 500 {
		return ReverseResult{}, ErrReasonRequired
	}
	// TODO(gateway-core): in one db.WithTx: LockTransactionInternal, check
	// reversible (GetReversalOf), InsertTransaction kind=reversal (receipt
	// rule: ErrReversalReceipt for M-Pesa), MarkChargeReversed when
	// charges.transaction_id = original, emit transaction.reversed, audit.
	return ReverseResult{}, ErrNotImplemented
}

func validReceipt(r string) bool {
	if len(r) == 0 || len(r) > 64 {
		return false
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
