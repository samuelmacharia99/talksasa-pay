// Package events is the transactional outbox. Emit writes an event row, its
// webhook delivery row and the River delivery job using the caller's
// transaction, so an event exists if and only if the change it describes
// committed. Apps read the same events from GET /v1/events.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Event types. Part of the public contract: add, never rename.
const (
	ChargeSucceeded   = "charge.succeeded"    // paid with an M-Pesa receipt (also late money on an expired/cancelled charge)
	ChargeNeedsReview = "charge.needs_review" // Safaricom said paid without a receipt: staff are checking
	ChargeExpired     = "charge.expired"
	ChargeCancelled   = "charge.cancelled"
	// STK push lifecycle. A failed prompt does not fail the charge.
	STKPushSent        = "stk_push.sent"         // prompt is on the customer's phone
	STKPushFailed      = "stk_push.failed"       // cancelled, timed out, insufficient funds, rejected...
	STKPushSucceeded   = "stk_push.succeeded"    // customer paid; receipt recorded
	STKPushNeedsReview = "stk_push.needs_review" // Safaricom says paid but sent no receipt: staff are checking
	// Money routed to the app that did not pay a waiting charge (paybill
	// payment without a match, or a second payment for a succeeded charge).
	PaymentReceived     = "payment.received"
	TransactionReversed = "transaction.reversed"
)

// Subject types (events.subject_type).
const (
	SubjectCharge      = "charge"
	SubjectTransaction = "transaction"
	SubjectSTKPush     = "stk_push"
)

// AllTypes lists every event type (docs, contract tests, SDKs).
var AllTypes = []string{
	ChargeSucceeded, ChargeNeedsReview, ChargeExpired, ChargeCancelled,
	STKPushSent, STKPushFailed, STKPushSucceeded, STKPushNeedsReview,
	PaymentReceived, TransactionReversed,
}

// ErrNoTx means Emit was called outside a transaction. Events must commit
// atomically with the change, so this is a programming error.
var ErrNoTx = errors.New("events: Emit requires queries bound to a transaction")

// Enqueuer schedules webhook delivery of an event inside tx (River InsertTx).
type Enqueuer interface {
	EnqueueWebhook(ctx context.Context, tx pgx.Tx, appID, eventID uuid.UUID) error
}

// Emitter writes events.
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
func (e *Emitter) Emit(ctx context.Context, q *store.Queries, appID uuid.UUID, typ, subjectType string, subjectID uuid.UUID, data any) (uuid.UUID, error) {
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
		ID: id, AppID: appID, Type: typ, SubjectType: subjectType, SubjectID: subjectID, Data: body,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("events: insert %s: %w", typ, err)
	}
	if err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{EventID: id, AppID: appID}); err != nil {
		return uuid.Nil, fmt.Errorf("events: insert delivery: %w", err)
	}
	if e.enq != nil {
		if err := e.enq.EnqueueWebhook(ctx, tx, appID, id); err != nil {
			return uuid.Nil, fmt.Errorf("events: enqueue webhook: %w", err)
		}
	}
	return id, nil
}

// EmitCharge emits typ with the charge's current state (and its newest
// prompt) as data.
func (e *Emitter) EmitCharge(ctx context.Context, q *store.Queries, appID, chargeID uuid.UUID, typ string) error {
	c, err := q.GetChargeForApp(ctx, store.GetChargeForAppParams{AppID: appID, ID: chargeID})
	if err != nil {
		return fmt.Errorf("events: load charge: %w", err)
	}
	var latest *store.StkPush
	p, err := q.GetLatestSTKPushForCharge(ctx, chargeID)
	switch {
	case err == nil:
		latest = &p
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("events: load latest stk push: %w", err)
	}
	_, err = e.Emit(ctx, q, appID, typ, SubjectCharge, chargeID, e.view.Charge(c, latest))
	return err
}

// EmitTransaction emits typ (transaction.reversed) with the transaction as data.
func (e *Emitter) EmitTransaction(ctx context.Context, q *store.Queries, appID, txnID uuid.UUID, typ string) error {
	t, err := q.GetTransactionViewForApp(ctx, store.GetTransactionViewForAppParams{AppID: &appID, ID: txnID})
	if err != nil {
		return fmt.Errorf("events: load transaction: %w", err)
	}
	_, err = e.Emit(ctx, q, appID, typ, SubjectTransaction, txnID, e.view.Transaction(t))
	return err
}

// EmitPaymentReceived emits payment.received for a transaction routed to the
// app. duplicateOf is the already-succeeded charge the money named, if any.
func (e *Emitter) EmitPaymentReceived(ctx context.Context, q *store.Queries, appID, txnID uuid.UUID, duplicateOf *uuid.UUID) error {
	t, err := q.GetTransactionViewForApp(ctx, store.GetTransactionViewForAppParams{AppID: &appID, ID: txnID})
	if err != nil {
		return fmt.Errorf("events: load transaction: %w", err)
	}
	_, err = e.Emit(ctx, q, appID, PaymentReceived, SubjectTransaction, txnID,
		apiview.PaymentReceived{Transaction: e.view.Transaction(t), DuplicateOfCharge: duplicateOf})
	return err
}

// EmitSTKPush emits typ with the push's current state as data.
func (e *Emitter) EmitSTKPush(ctx context.Context, q *store.Queries, appID, pushID uuid.UUID, typ string) error {
	p, err := q.GetSTKPushForApp(ctx, store.GetSTKPushForAppParams{AppID: appID, ID: pushID})
	if err != nil {
		return fmt.Errorf("events: load stk push: %w", err)
	}
	_, err = e.Emit(ctx, q, appID, typ, SubjectSTKPush, pushID, apiview.STKPushOf(p))
	return err
}
