package jobs

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// DefaultBatchSize bounds the rows one maintenance transaction touches, so
// row locks are held briefly and a long backlog never becomes one huge
// transaction.
const DefaultBatchSize = 500

// maintenanceTimeout is the River job timeout for a full sweep. A sweep
// works in committed batches, so a timeout only defers the rest to the next
// run.
const maintenanceTimeout = 10 * time.Minute

// Actor recorded on every row the maintenance jobs change.
var (
	expiryActor    = audit.System("expiry-job")
	retentionActor = audit.System("retention-job")
)

// ExpiryWorker closes open payment requests whose expires_at has passed.
//
// Money rules:
//   - Only fully unpaid `open` requests expire. A partially paid request
//     stays payable (the query's status = 'open' predicate, and the
//     payment_requests guard trigger, both enforce it).
//   - Each request's payment_request.expired event is written in the same
//     transaction as its status change.
//   - Race-safe with payments: the batch is selected FOR NO KEY UPDATE SKIP
//     LOCKED. A request an allocation has locked is skipped (and picked up by
//     a later sweep if it is still open); a request the sweep has locked
//     makes the allocation wait, after which it re-reads the row, sees
//     `expired` and leaves it alone. Whichever commits first wins, so a
//     request is never both paid and expired.
//
// Idempotent: re-running finds nothing more to do; concurrent sweeps skip
// each other's rows.
//
// Audit: one audit_log row per product per batch ("payment_requests.expired",
// with the count and ids), not one per request. Expiry is a routine
// scheduled effect, each request already gets its own immutable event row,
// and the audit chain is serialised by a global lock, so per-request audit
// rows would add contention and noise for no extra traceability.
type ExpiryWorker struct {
	river.WorkerDefaults[queue.ExpireRequestsArgs]
	pool      db.Beginner
	events    *events.Emitter
	logger    *slog.Logger
	metrics   *Metrics
	batchSize int
}

// Work implements river.Worker.
func (w *ExpiryWorker) Work(ctx context.Context, _ *river.Job[queue.ExpireRequestsArgs]) error {
	_, err := w.Sweep(ctx)
	return err
}

// Timeout implements river.Worker.
func (w *ExpiryWorker) Timeout(*river.Job[queue.ExpireRequestsArgs]) time.Duration {
	return maintenanceTimeout
}

// Sweep expires due requests batch by batch until a batch comes back short
// or ctx ends. It returns how many requests it expired (all committed, even
// when it also returns an error).
func (w *ExpiryWorker) Sweep(ctx context.Context) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := w.expireBatch(ctx)
		total += n
		if err != nil {
			w.logger.ErrorContext(ctx, "expiry batch failed", slog.Int("expired_so_far", total), slog.Any("error", err))
			return total, err
		}
		if n < w.batchSize {
			break
		}
	}
	if total > 0 {
		w.logger.InfoContext(ctx, "payment requests expired", slog.Int("count", total))
	}
	return total, nil
}

func (w *ExpiryWorker) expireBatch(ctx context.Context) (int, error) {
	var n int
	err := db.WithTx(ctx, w.pool, expiryActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		rows, err := q.ExpireDuePaymentRequests(ctx, int32(w.batchSize))
		if err != nil {
			return fmt.Errorf("jobs: expire requests: %w", err)
		}
		// RETURNING order is unspecified; emit in expiry order so the event
		// feed reads naturally.
		slices.SortFunc(rows, func(a, b store.PaymentRequest) int {
			if c := a.ExpiresAt.Compare(*b.ExpiresAt); c != 0 {
				return c
			}
			return cmp.Compare(a.ID.String(), b.ID.String())
		})
		byProduct := map[uuid.UUID][]string{}
		var products []uuid.UUID
		for _, r := range rows {
			if err := w.events.EmitPaymentRequest(ctx, q, r.ProductID, r.ID, events.PaymentRequestExpired); err != nil {
				return err
			}
			if _, ok := byProduct[r.ProductID]; !ok {
				products = append(products, r.ProductID)
			}
			byProduct[r.ProductID] = append(byProduct[r.ProductID], r.ID.String())
		}
		for _, pid := range products {
			ids := byProduct[pid]
			if err := audit.Record(ctx, q, audit.Entry{
				Actor: expiryActor, Action: "payment_requests.expired", EntityType: "payment_request", ProductID: &pid,
				Details: map[string]any{"count": len(ids), "payment_request_ids": ids},
			}); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	w.metrics.expired(n)
	return n, nil
}

// RetentionWorker deletes webhook attempt logs older than the retention
// period. They are diagnostics with no money meaning; the delivery summary
// (webhook_deliveries) and the events themselves are kept forever.
// Idempotent; deletes in batches so it never holds many locks at once.
type RetentionWorker struct {
	river.WorkerDefaults[queue.RetentionArgs]
	pool      *db.Pool
	logger    *slog.Logger
	metrics   *Metrics
	retention time.Duration
	batchSize int
	now       func() time.Time
}

// Work implements river.Worker.
func (w *RetentionWorker) Work(ctx context.Context, _ *river.Job[queue.RetentionArgs]) error {
	_, err := w.Prune(ctx)
	return err
}

// Timeout implements river.Worker.
func (w *RetentionWorker) Timeout(*river.Job[queue.RetentionArgs]) time.Duration {
	return maintenanceTimeout
}

// Prune deletes old attempts and returns how many it removed. One audit row
// records each run that deleted anything.
func (w *RetentionWorker) Prune(ctx context.Context) (int64, error) {
	before := w.now().Add(-w.retention)
	q := store.New(w.pool)
	var total int64
	var runErr error
	for {
		if runErr = ctx.Err(); runErr != nil {
			break
		}
		n, err := q.DeleteOldWebhookAttempts(ctx, store.DeleteOldWebhookAttemptsParams{Before: before, BatchSize: int32(w.batchSize)})
		total += n
		if err != nil {
			runErr = fmt.Errorf("jobs: delete webhook attempts: %w", err)
			break
		}
		if n < int64(w.batchSize) {
			break
		}
	}
	w.metrics.pruned(total)
	if total > 0 {
		w.logger.InfoContext(ctx, "webhook attempts pruned", slog.Int64("count", total), slog.Time("before", before))
		// Record what was deleted even if a later batch failed; use a
		// context that survives cancellation so the record is not lost.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := db.WithTx(actx, w.pool, retentionActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
			return audit.Record(ctx, q, audit.Entry{
				Actor: retentionActor, Action: "webhook_attempts.pruned", EntityType: "webhook_attempt",
				Details: map[string]any{"count": total, "attempted_before": before.UTC().Format(time.RFC3339)},
			})
		}); err != nil && runErr == nil {
			runErr = err
		}
	}
	return total, runErr
}
