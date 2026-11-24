package stk

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// ResolveParams is a staff decision on a needs_review M-Pesa transaction.
type ResolveParams struct {
	TransactionID uuid.UUID
	// Confirm with the receipt from the M-Pesa portal (marks it succeeded
	// and allocates it), or Fail (no money arrived).
	Confirm bool
	Receipt string
	Reason  string
}

// ErrAmountMismatchConfirm: a transaction sent to review because Safaricom
// reported a different amount cannot be confirmed (its amount is immutable
// and would record the wrong sum); it can only be failed. Wraps
// ErrNotResolvable.
var ErrAmountMismatchConfirm = fmt.Errorf("%w: the amount Safaricom reported differs from this transaction; it can only be failed", ErrNotResolvable)

// Resolve applies a staff decision (finance or admin; audited). Returns the
// updated transaction.
//
// Only an incoming M-Pesa payment in needs_review can be resolved. Confirm
// needs the receipt (normalised to upper case, 1-64 letters/digits) and it
// must not be on another transaction; the transaction becomes succeeded, is
// allocated (if matched to a payer) and its STK push succeeds. Fail marks
// the transaction failed and its push failed.
func (s *Service) Resolve(ctx context.Context, actor audit.Actor, p ResolveParams) (store.Transaction, error) {
	if err := actor.Validate(); err != nil {
		return store.Transaction{}, err
	}
	reason := strings.TrimSpace(p.Reason)
	if len(reason) < 3 || len(reason) > 500 {
		return store.Transaction{}, ErrReasonRequired
	}
	receipt := strings.ToUpper(strings.TrimSpace(p.Receipt))
	if p.Confirm && !ledger.ValidReceipt(receipt) {
		return store.Transaction{}, ErrInvalidReceipt
	}
	if !p.Confirm {
		receipt = ""
	}
	var out store.Transaction
	err := db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		// Lock order: push, then transaction (as the callback path).
		var push *Push
		if pr, err := q.GetSTKPushByTransaction(ctx, p.TransactionID); err == nil {
			locked, err := q.LockSTKPush(ctx, pr.ID)
			if err != nil {
				return err
			}
			push = &locked
		} else if !db.IsNotFound(err) {
			return err
		}
		txn, err := q.LockTransactionInternal(ctx, p.TransactionID)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if txn.Kind != store.TxnKindPayment || txn.Direction != store.TxnDirectionIn || txn.Provider != "mpesa" ||
			(txn.Channel != store.TxnChannelMpesaStk && txn.Channel != store.TxnChannelMpesaC2b) ||
			txn.Status != store.TxnStatusNeedsReview {
			return ErrNotResolvable
		}
		if p.Confirm {
			if txn.FailureCode != nil && *txn.FailureCode == failMismatch {
				return ErrAmountMismatchConfirm
			}
			if other, err := q.GetTransactionByReceipt(ctx, store.GetTransactionByReceiptParams{Provider: "mpesa", ProviderTransactionID: &receipt}); err == nil && other.ID != txn.ID {
				return ErrReceiptTaken
			} else if err != nil && !db.IsNotFound(err) {
				return err
			}
			if out, err = ledger.CompleteSTK(ctx, q, ledger.CompleteSTKParams{TransactionID: txn.ID, Receipt: receipt}); err != nil {
				if db.IsUniqueViolation(err, "transactions_provider_receipt_uq") {
					return ErrReceiptTaken
				}
				return err
			}
			if out.PayerAccountID != nil {
				if _, err := s.o.Alloc.AllocateTransaction(ctx, q, actor, out.ID); err != nil {
					return err
				}
			}
			if push != nil {
				switch push.Status {
				case store.StkPushStatusSent, store.StkPushStatusUnknown, store.StkPushStatusFailed:
					if err := s.succeedPushTx(ctx, q, *push, "Confirmed by staff with receipt "+receipt); err != nil {
						return err
					}
				}
			}
		} else {
			if out, err = ledger.FailSTK(ctx, q, txn.ID, failStaff, reason, nil); err != nil {
				return err
			}
			if push != nil {
				switch push.Status {
				case store.StkPushStatusInitiating, store.StkPushStatusSent, store.StkPushStatusUnknown:
					code := failStaff
					pushed, err := q.FinishSTKPush(ctx, store.FinishSTKPushParams{
						Status: store.StkPushStatusFailed, ResultCode: &code, ResultDesc: optStr(truncate(reason, 500)), ID: push.ID,
					})
					if err != nil {
						return fmt.Errorf("stk: fail push: %w", err)
					}
					if err := s.o.Events.EmitSTKPush(ctx, q, pushed.ProductID, pushed.ID, events.STKPushFailed); err != nil {
						return err
					}
				}
			}
		}
		decision := "fail"
		if p.Confirm {
			decision = "confirm"
		}
		details := map[string]any{"decision": decision, "reason": reason, "previous_status": string(txn.Status)}
		if receipt != "" {
			details["receipt"] = receipt
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "transaction.resolved", EntityType: "transaction", EntityID: txn.ID.String(),
			ProductID: txn.ProductID, Details: details,
		})
	})
	if err != nil {
		if db.IsUniqueViolation(err, "transactions_provider_receipt_uq") {
			return store.Transaction{}, ErrReceiptTaken
		}
		return store.Transaction{}, err
	}
	s.o.Logger.WarnContext(ctx, "needs_review transaction resolved by staff", slog.String("transaction_id", out.ID.String()),
		slog.String("status", string(out.Status)), slog.String("actor", actor.String()))
	return out, nil
}

// CustomerMessage is the customer-facing explanation of an STK result code
// (pay page).
func CustomerMessage(resultCode string) string {
	switch strings.TrimSpace(resultCode) {
	case mpesa.ResultCancelledByUser:
		return "You cancelled the M-Pesa prompt."
	case mpesa.ResultUnreachable:
		return "We couldn't reach your phone. Make sure it's on and try again."
	case mpesa.ResultInsufficientFunds:
		return "Your M-Pesa balance wasn't enough."
	case mpesa.ResultWrongPIN:
		return "The M-Pesa PIN was wrong."
	case mpesa.ResultTransactionExpired:
		return "The prompt expired before it was answered."
	}
	return "The payment didn't go through."
}
