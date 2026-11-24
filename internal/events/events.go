// Package events is the transactional outbox. Emit writes an event row, its
// webhook delivery row and the River delivery job using the caller's
// transaction, so an event exists if and only if the change it describes
// committed. Products read the same events from GET /v1/events.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Event types. Part of the public contract: add, never rename.
const (
	PaymentRequestCreated       = "payment_request.created"
	PaymentRequestPartiallyPaid = "payment_request.partially_paid"
	PaymentRequestPaid          = "payment_request.paid"
	PaymentRequestReopened      = "payment_request.reopened" // a payment on it was reversed
	PaymentRequestCancelled     = "payment_request.cancelled"
	PaymentRequestExpired       = "payment_request.expired"
	TransactionAllocated        = "transaction.allocated"
	TransactionReversed         = "transaction.reversed"
	PayerCreditChanged          = "payer.credit_changed" //nolint:gosec // event name, not a credential
)

// AllTypes lists every event type (docs, contract tests, SDKs).
var AllTypes = []string{
	PaymentRequestCreated, PaymentRequestPartiallyPaid, PaymentRequestPaid, PaymentRequestReopened,
	PaymentRequestCancelled, PaymentRequestExpired, TransactionAllocated, TransactionReversed, PayerCreditChanged,
}

// ErrNoTx means Emit was called outside a transaction. Events must commit
// atomically with the change, so this is a programming error.
var ErrNoTx = errors.New("events: Emit requires queries bound to a transaction")

// Enqueuer schedules webhook delivery of an event inside tx (River InsertTx).
type Enqueuer interface {
	EnqueueWebhook(ctx context.Context, tx pgx.Tx, productID, eventID uuid.UUID) error
}

// Emitter writes events. It implements allocation.Notifier.
type Emitter struct {
	view apiview.View
	enq  Enqueuer // nil: events are stored (feed works) but no webhook jobs are queued
}

// NewEmitter builds an emitter. enq may be nil only in tests and CLI tools
// that never change money.
func NewEmitter(view apiview.View, enq Enqueuer) *Emitter { return &Emitter{view: view, enq: enq} }

// View exposes the resource renderer (shared with the API handlers).
func (e *Emitter) View() apiview.View { return e.view }

// Emit stores one event and queues its webhook, in q's transaction.
func (e *Emitter) Emit(ctx context.Context, q *store.Queries, productID uuid.UUID, typ, subjectType string, subjectID uuid.UUID, data any) (uuid.UUID, error) {
	tx, ok := q.Tx()
	if !ok {
		return uuid.Nil, ErrNoTx
	}
	body, err := json.Marshal(data)
	if err != nil {
		return uuid.Nil, fmt.Errorf("events: encode %s: %w", typ, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := q.InsertEvent(ctx, store.InsertEventParams{
		ID: id, ProductID: productID, Type: typ, SubjectType: subjectType, SubjectID: subjectID, Data: body,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("events: insert %s: %w", typ, err)
	}
	if err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{EventID: id, ProductID: productID}); err != nil {
		return uuid.Nil, fmt.Errorf("events: insert delivery: %w", err)
	}
	if e.enq != nil {
		if err := e.enq.EnqueueWebhook(ctx, tx, productID, id); err != nil {
			return uuid.Nil, fmt.Errorf("events: enqueue webhook: %w", err)
		}
	}
	return id, nil
}

// EmitPaymentRequest emits typ with the request's current state as data.
func (e *Emitter) EmitPaymentRequest(ctx context.Context, q *store.Queries, productID, requestID uuid.UUID, typ string) error {
	row, err := q.GetPaymentRequestForProduct(ctx, store.GetPaymentRequestForProductParams{ProductID: productID, ID: requestID})
	if err != nil {
		return fmt.Errorf("events: load payment request: %w", err)
	}
	_, err = e.Emit(ctx, q, productID, typ, "payment_request", requestID, e.view.PaymentRequest(row))
	return err
}

// EmitTransaction emits typ with the transaction (and its allocations) as data.
func (e *Emitter) EmitTransaction(ctx context.Context, q *store.Queries, productID, txnID uuid.UUID, typ string) error {
	t, err := q.GetTransactionViewForProduct(ctx, store.GetTransactionViewForProductParams{ProductID: &productID, ID: txnID})
	if err != nil {
		return fmt.Errorf("events: load transaction: %w", err)
	}
	allocs, err := q.ListAllocationsForTransactionProduct(ctx, store.ListAllocationsForTransactionProductParams{ProductID: productID, TransactionID: &txnID})
	if err != nil {
		return fmt.Errorf("events: load allocations: %w", err)
	}
	_, err = e.Emit(ctx, q, productID, typ, "transaction", txnID, e.view.Transaction(t, allocs))
	return err
}

// Allocated implements allocation.Notifier: called inside the allocation
// transaction after a batch is written. It emits, in order: the transaction
// event, one event per request whose status changed, and the credit change.
func (e *Emitter) Allocated(ctx context.Context, q *store.Queries, n allocation.Notification) error {
	if n.Result.Duplicate || n.Result.BatchID == uuid.Nil {
		return nil // nothing changed
	}
	switch n.Kind {
	case store.AllocationBatchKindPayment:
		if err := e.EmitTransaction(ctx, q, n.ProductID, *n.TransactionID, TransactionAllocated); err != nil {
			return err
		}
	case store.AllocationBatchKindReversal:
		if err := e.EmitTransaction(ctx, q, n.ProductID, *n.TransactionID, TransactionReversed); err != nil {
			return err
		}
	}
	for _, rc := range n.Result.Requests {
		if rc.Before == rc.After {
			continue
		}
		var typ string
		switch {
		case rc.After == allocation.StatusPaid:
			typ = PaymentRequestPaid
		case n.Kind == store.AllocationBatchKindReversal:
			typ = PaymentRequestReopened
		case rc.After == allocation.StatusPartiallyPaid:
			typ = PaymentRequestPartiallyPaid
		default:
			continue
		}
		if err := e.EmitPaymentRequest(ctx, q, n.ProductID, rc.RequestID, typ); err != nil {
			return err
		}
	}
	if n.Result.CreditDelta != 0 {
		payer, err := q.GetPayer(ctx, store.GetPayerParams{ProductID: n.ProductID, ID: n.PayerID})
		if err != nil {
			return fmt.Errorf("events: load payer: %w", err)
		}
		_, err = e.Emit(ctx, q, n.ProductID, PayerCreditChanged, "payer", n.PayerID, apiview.CreditChange{
			Payer: apiview.PayerRef{ExternalRef: payer.ExternalRef, AccountNumber: payer.AccountNumber},
			Delta: int64(n.Result.CreditDelta), CreditBalance: payer.CreditBalance, TransactionID: n.TransactionID,
		})
		if err != nil {
			return err
		}
	}
	return nil
}
