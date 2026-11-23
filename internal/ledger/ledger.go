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
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/allocation"
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
	ErrNotReversible   = errors.New("ledger: only allocated incoming payments can be reversed")
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
	Channel          store.TxnChannel // mpesa_stk | mpesa_c2b
	Receipt          string           // M-Pesa receipt, e.g. SKL4ABCD12
	ProviderRequest  string           // CheckoutRequestID (STK), empty for C2B
	Amount           money.KES
	Phone            string // raw; normalised here (C2B may send a masked/hashed value: pass "")
	BillRef          string // account reference as typed
	ProductID        *uuid.UUID
	PayerAccountID   *uuid.UUID
	PaymentRequestID *uuid.UUID // target request (STK / hosted page)
	OccurredAt       *time.Time
	RawPayload       json.RawMessage // provider payload, already free of secrets
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
		ID: id, ProductID: in.ProductID, Direction: store.TxnDirectionIn, Kind: store.TxnKindPayment,
		Channel: in.Channel, Provider: "mpesa", Status: store.TxnStatusSucceeded, Amount: int64(in.Amount),
		ProviderTransactionID: &receipt, ProviderRequestID: optStr(in.ProviderRequest), Phone: ph,
		BillRef: optStr(truncate(strings.TrimSpace(in.BillRef), 64)), PayerAccountID: in.PayerAccountID,
		PaymentRequestID: in.PaymentRequestID, RawPayload: in.RawPayload, OccurredAt: in.OccurredAt,
		CompletedAt: &now,
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

// Service performs ledger operations that need their own database transaction.
type Service struct {
	pool   *db.Pool
	alloc  *allocation.Service
	logger *slog.Logger
}

// NewService builds a ledger service.
func NewService(pool *db.Pool, alloc *allocation.Service, logger *slog.Logger) *Service {
	return &Service{pool: pool, alloc: alloc, logger: logger}
}

// RecordAndAllocate records an incoming payment matched to a payer and
// allocates it, atomically. Duplicate receipts return the original
// allocation with Duplicate=true.
func (s *Service) RecordAndAllocate(ctx context.Context, actor audit.Actor, in IncomingPayment) (Transaction, allocation.Result, error) {
	var (
		txn Transaction
		res allocation.Result
	)
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		var created bool
		txn, created, err = RecordIncoming(ctx, q, in)
		if err != nil {
			return err
		}
		if txn.PayerAccountID == nil {
			return nil // unmatched: suspense handles it
		}
		res, err = s.alloc.AllocateTransaction(ctx, q, actor, txn.ID)
		if err != nil {
			return err
		}
		if !created {
			res.Duplicate = true
		}
		return nil
	})
	if err != nil {
		return Transaction{}, allocation.Result{}, err
	}
	if res.Duplicate {
		s.logger.InfoContext(ctx, "duplicate receipt ignored", slog.String("transaction_id", txn.ID.String()),
			slog.String("channel", string(txn.Channel)))
	}
	return txn, res, nil
}

// ReverseParams describes a correction of a recorded payment, e.g. after
// Safaricom reverses a transaction.
type ReverseParams struct {
	OriginalID      uuid.UUID
	Reason          string
	ProviderReceipt string // Safaricom's reversal receipt; required for M-Pesa payments
}

// Reverse records a reversal of an allocated incoming payment and undoes its
// allocations. Only a named person (admin or CLI operator) may do this.
func (s *Service) Reverse(ctx context.Context, actor audit.Actor, p ReverseParams) (Transaction, allocation.Result, error) {
	if actor.Type != store.AuditActorTypeAdmin && actor.Type != store.AuditActorTypeCli {
		return Transaction{}, allocation.Result{}, ErrActorRequired
	}
	reason := strings.TrimSpace(p.Reason)
	if len(reason) < 3 || len(reason) > 500 {
		return Transaction{}, allocation.Result{}, ErrReasonRequired
	}
	var (
		rev Transaction
		res allocation.Result
	)
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		orig, err := q.LockTransactionInternal(ctx, p.OriginalID)
		if err != nil {
			return fmt.Errorf("ledger: load original: %w", err)
		}
		if orig.Kind != store.TxnKindPayment || orig.Status != store.TxnStatusSucceeded || orig.PayerAccountID == nil {
			return ErrNotReversible
		}
		if _, err := q.GetReversalOf(ctx, &orig.ID); err == nil {
			return ErrAlreadyReversed
		} else if !db.IsNotFound(err) {
			return err
		}
		batch, err := q.GetPaymentBatchForTransaction(ctx, &orig.ID)
		if db.IsNotFound(err) {
			return ErrNotReversible
		}
		if err != nil {
			return err
		}

		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		var receipt *string
		if r := strings.ToUpper(strings.TrimSpace(p.ProviderReceipt)); r != "" {
			if !validReceipt(r) {
				return ErrInvalidReceipt
			}
			receipt = &r
		} else if orig.Provider != "manual" {
			return ErrReversalReceipt
		}
		createdBy := actor.String()
		now := time.Now()
		rev, err = q.InsertTransaction(ctx, store.InsertTransactionParams{
			ID: id, ProductID: orig.ProductID, Direction: store.TxnDirectionOut, Kind: store.TxnKindReversal,
			Channel: orig.Channel, Provider: orig.Provider, Status: store.TxnStatusSucceeded, Amount: orig.Amount,
			ProviderTransactionID: receipt, Phone: orig.Phone, PayerAccountID: orig.PayerAccountID,
			PaymentRequestID: orig.PaymentRequestID, ReversesTransactionID: &orig.ID,
			Reason: &reason, CreatedBy: &createdBy, CompletedAt: &now,
		})
		if db.IsNotFound(err) {
			return ErrReceiptConflict
		}
		if err != nil {
			return fmt.Errorf("ledger: insert reversal: %w", err)
		}
		res, err = s.alloc.ReverseBatch(ctx, q, actor, rev.ID, batch, reason)
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "transaction.reversed", EntityType: "transaction", EntityID: orig.ID.String(),
			ProductID: orig.ProductID,
			Details:   map[string]any{"reversal_id": rev.ID.String(), "amount": orig.Amount, "reason": reason},
		})
	})
	if err != nil {
		return Transaction{}, allocation.Result{}, err
	}
	s.logger.WarnContext(ctx, "payment reversed", slog.String("original_id", p.OriginalID.String()),
		slog.String("reversal_id", rev.ID.String()), slog.Int64("amount", rev.Amount), slog.String("actor", actor.String()))
	return rev, res, nil
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
