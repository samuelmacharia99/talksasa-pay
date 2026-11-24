// Package queue holds the River (Postgres-backed job queue) plumbing shared
// by the API process (insert-only) and the worker process (runs jobs):
// job argument types, queue names, the in-transaction webhook enqueuer and
// River's schema migration.
//
// Job args are the contract between the two processes: add fields
// compatibly, never rename a Kind.
package queue

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Queue names.
const (
	QueueWebhooks    = "webhooks"
	QueueMaintenance = "maintenance"
	// QueueMpesa processes Safaricom callbacks and STK status checks. Kept
	// apart so a webhook backlog never delays recording money.
	QueueMpesa = "mpesa"
)

// WebhookArgs asks the worker to deliver one event to its product.
type WebhookArgs struct {
	EventID   uuid.UUID `json:"event_id"`
	ProductID uuid.UUID `json:"product_id"`
}

// Kind implements river.JobArgs.
func (WebhookArgs) Kind() string { return "webhook_delivery" }

// WebhookMaxAttempts bounds retries (about 3 days with the worker's backoff).
const WebhookMaxAttempts = 25

// InsertOpts implements river.JobArgsWithInsertOpts. Unique by event while
// queued/running, so a retry of the emitting code can't double-deliver.
func (WebhookArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueWebhooks, MaxAttempts: WebhookMaxAttempts,
		UniqueOpts: river.UniqueOpts{ByArgs: true},
	}
}

// ExpireRequestsArgs runs the periodic expiry sweep.
type ExpireRequestsArgs struct{}

// Kind implements river.JobArgs.
func (ExpireRequestsArgs) Kind() string { return "expire_payment_requests" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ExpireRequestsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
}

// RetentionArgs prunes diagnostic data (webhook attempt logs).
type RetentionArgs struct{}

// Kind implements river.JobArgs.
func (RetentionArgs) Kind() string { return "retention" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (RetentionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: time.Hour}}
}

// ProcessCallbackArgs asks the worker to process one stored Daraja
// callback (mpesa_callbacks row). Inserted in the same transaction as the
// row, so every stored callback gets processed.
type ProcessCallbackArgs struct {
	CallbackID uuid.UUID `json:"callback_id"`
}

// Kind implements river.JobArgs.
func (ProcessCallbackArgs) Kind() string { return "mpesa_process_callback" }

// ProcessCallbackMaxAttempts: retries cover DB outages; processing is
// idempotent (receipt uniqueness, row locks), so retrying is always safe.
const ProcessCallbackMaxAttempts = 20

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ProcessCallbackArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueMpesa, MaxAttempts: ProcessCallbackMaxAttempts,
		UniqueOpts: river.UniqueOpts{ByArgs: true},
	}
}

// EnqueueCallback inserts the processing job for a stored callback in tx.
func (e *Enqueuer) EnqueueCallback(ctx context.Context, tx pgx.Tx, callbackID uuid.UUID) error {
	_, err := e.c.InsertTx(ctx, tx, ProcessCallbackArgs{CallbackID: callbackID}, nil)
	return err
}

// STKSweepArgs runs the periodic STK status check (STK query for pushes
// whose callback is late; give up on pushes stuck before Daraja answered).
type STKSweepArgs struct{}

// Kind implements river.JobArgs.
func (STKSweepArgs) Kind() string { return "mpesa_stk_sweep" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (STKSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMpesa, MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: 15 * time.Second}}
}

// Client is a River client over pgx.
type Client = river.Client[pgx.Tx]

// NewInsertOnly builds a client that can only insert jobs (API process).
func NewInsertOnly(pool *pgxpool.Pool, logger *slog.Logger) (*Client, error) {
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("queue: river client: %w", err)
	}
	return c, nil
}

// Enqueuer implements events.Enqueuer with River's transactional insert.
type Enqueuer struct{ c *Client }

// NewEnqueuer wraps a River client.
func NewEnqueuer(c *Client) *Enqueuer { return &Enqueuer{c: c} }

// EnqueueWebhook inserts the delivery job in tx: it exists iff tx commits.
func (e *Enqueuer) EnqueueWebhook(ctx context.Context, tx pgx.Tx, productID, eventID uuid.UUID) error {
	_, err := e.c.InsertTx(ctx, tx, WebhookArgs{EventID: eventID, ProductID: productID}, nil)
	return err
}
