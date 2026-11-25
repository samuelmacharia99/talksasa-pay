package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Pending STK transactions. These helpers run inside the caller's database
// transaction (q bound to a pgx.Tx) so the ledger change commits atomically
// with the STK push row and the events describing it. The status lifecycle
// itself is enforced by the transactions guard trigger.

// ErrNotPending: the transaction is not pending/needs_review any more (a
// concurrent callback, sweeper or staff decision got there first).
var ErrNotPending = errors.New("ledger: transaction is no longer pending")

// StartSTKParams describes the pending row an STK push will complete.
type StartSTKParams struct {
	AppID         uuid.UUID
	ChargeID      uuid.UUID
	Amount        money.KES // always the charge's exact amount
	Phone         string    // normalised 2547XXXXXXXX
	AccountNumber string    // the charge's account number (Daraja AccountReference)
}

// StartSTK inserts a pending mpesa_stk payment (no receipt).
func StartSTK(ctx context.Context, q *store.Queries, p StartSTKParams) (Transaction, error) {
	if p.Amount <= 0 || p.Amount > money.MaxKES {
		return Transaction{}, money.ErrNonPositive
	}
	if !phone.IsNormalized(p.Phone) {
		return Transaction{}, phone.ErrInvalid
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Transaction{}, err
	}
	ph := p.Phone
	row, err := q.InsertPendingSTKTransaction(ctx, store.InsertPendingSTKTransactionParams{
		ID: id, AppID: &p.AppID, ChargeID: &p.ChargeID, Amount: int64(p.Amount), Phone: &ph,
		AccountNumber: optStr(truncate(strings.TrimSpace(p.AccountNumber), 64)),
	})
	if err != nil {
		return Transaction{}, fmt.Errorf("ledger: insert pending stk transaction: %w", err)
	}
	return row, nil
}

// CompleteSTKParams completes a pending/needs_review row with Safaricom's
// receipt. The caller has already checked the amount and that the receipt
// is not on another row (a race still fails on the unique constraint).
type CompleteSTKParams struct {
	TransactionID uuid.UUID
	Receipt       string
	Phone         string // optional, raw (kept only if the row has none)
	OccurredAt    *time.Time
	RawPayload    json.RawMessage
}

// CompleteSTK marks a pending or needs_review transaction succeeded.
func CompleteSTK(ctx context.Context, q *store.Queries, p CompleteSTKParams) (Transaction, error) {
	receipt := strings.ToUpper(strings.TrimSpace(p.Receipt))
	if receipt == "" {
		return Transaction{}, ErrMissingReceipt
	}
	if !validReceipt(receipt) {
		return Transaction{}, ErrInvalidReceipt
	}
	var ph *string
	if n, err := phone.Normalize(p.Phone); err == nil && strings.TrimSpace(p.Phone) != "" {
		ph = &n
	}
	if len(p.RawPayload) > 0 && !json.Valid(p.RawPayload) {
		return Transaction{}, ErrPayloadNotJSON
	}
	row, err := q.CompletePendingTransaction(ctx, store.CompletePendingTransactionParams{
		Receipt: &receipt, Phone: ph, OccurredAt: p.OccurredAt, RawPayload: p.RawPayload, ID: p.TransactionID,
	})
	if db.IsNotFound(err) {
		return Transaction{}, ErrNotPending
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("ledger: complete stk transaction: %w", err)
	}
	return row, nil
}

// FailSTK marks a pending or needs_review transaction failed. code and
// reason are truncated to the column limits.
func FailSTK(ctx context.Context, q *store.Queries, id uuid.UUID, code, reason string, raw json.RawMessage) (Transaction, error) {
	if len(raw) > 0 && !json.Valid(raw) {
		return Transaction{}, ErrPayloadNotJSON
	}
	row, err := q.FailPendingTransaction(ctx, store.FailPendingTransactionParams{
		FailureCode: optStr(truncate(strings.TrimSpace(code), 32)), FailureReason: optStr(truncate(strings.TrimSpace(reason), 500)),
		RawPayload: raw, ID: id,
	})
	if db.IsNotFound(err) {
		return Transaction{}, ErrNotPending
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("ledger: fail stk transaction: %w", err)
	}
	return row, nil
}

// MarkNeedsReview moves a pending transaction to needs_review (staff must
// decide). A transaction that is no longer pending is returned as
// ErrNotPending.
func MarkNeedsReview(ctx context.Context, q *store.Queries, id uuid.UUID, code, reason string) (Transaction, error) {
	row, err := q.MarkTransactionNeedsReview(ctx, store.MarkTransactionNeedsReviewParams{
		FailureCode: optStr(truncate(strings.TrimSpace(code), 32)), FailureReason: optStr(truncate(strings.TrimSpace(reason), 500)), ID: id,
	})
	if db.IsNotFound(err) {
		return Transaction{}, ErrNotPending
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("ledger: mark needs review: %w", err)
	}
	return row, nil
}

// ValidReceipt reports whether r is a normalised M-Pesa receipt
// (1-64 upper-case letters or digits).
func ValidReceipt(r string) bool { return validReceipt(r) }
