package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/stk"
)

// STKService is what the M-Pesa jobs need (*stk.Service implements it).
type STKService interface {
	ProcessCallback(ctx context.Context, callbackID uuid.UUID) error
	Sweep(ctx context.Context, limit int) (stk.SweepResult, error)
}

// DefaultMpesaConcurrency is the mpesa queue's worker count when
// Deps.MpesaConcurrency is 0.
const DefaultMpesaConcurrency = 20

// STKSweepEvery is how often the leader schedules the STK status sweep.
const STKSweepEvery = 15 * time.Second

// STKSweepBatch bounds the pushes one sweep checks; the rest wait for the
// next run (every STKSweepEvery).
const STKSweepBatch = 200

const (
	// callbackTimeout bounds processing one callback (a few short DB
	// transactions). On timeout River retries; processing is idempotent.
	callbackTimeout = 30 * time.Second
	// sweepTimeout bounds one sweep: up to STKSweepBatch Daraja queries,
	// each bounded by the client's own timeout.
	sweepTimeout = 2 * time.Minute
)

// CallbackWorker processes one stored Daraja callback
// (queue.ProcessCallbackArgs) through the STK service. An error makes River
// retry with backoff (up to queue.ProcessCallbackMaxAttempts); the callback
// row stays in the inbox either way, so nothing Safaricom sent is lost.
type CallbackWorker struct {
	river.WorkerDefaults[queue.ProcessCallbackArgs]
	stk     STKService
	logger  *slog.Logger
	metrics *Metrics
}

// NewCallbackWorker builds the callback worker (exported for tests).
func NewCallbackWorker(d Deps) *CallbackWorker {
	return &CallbackWorker{stk: d.STK, logger: d.Logger.With(slog.String("job", "mpesa_callback")), metrics: d.Metrics}
}

// Work implements river.Worker.
func (w *CallbackWorker) Work(ctx context.Context, job *river.Job[queue.ProcessCallbackArgs]) error {
	if err := w.stk.ProcessCallback(ctx, job.Args.CallbackID); err != nil {
		w.metrics.callback(false)
		w.logger.WarnContext(ctx, "mpesa callback processing failed; will retry",
			slog.String("callback_id", job.Args.CallbackID.String()), slog.Int("attempt", job.Attempt), slog.Any("error", err))
		return err
	}
	w.metrics.callback(true)
	return nil
}

// Timeout implements river.Worker.
func (w *CallbackWorker) Timeout(*river.Job[queue.ProcessCallbackArgs]) time.Duration {
	return callbackTimeout
}

// STKSweepWorker runs the periodic STK status check (queue.STKSweepArgs):
// pushes stuck before Daraja answered are failed, and sent pushes whose
// callback is late are queried. Scheduled by the leader only; the args are
// unique per period, so concurrent workers never double-run it, and the
// sweep itself locks rows with SKIP LOCKED.
type STKSweepWorker struct {
	river.WorkerDefaults[queue.STKSweepArgs]
	stk     STKService
	logger  *slog.Logger
	metrics *Metrics
	batch   int
}

// NewSTKSweepWorker builds the sweep worker (exported for tests).
func NewSTKSweepWorker(d Deps) *STKSweepWorker {
	return &STKSweepWorker{stk: d.STK, logger: d.Logger.With(slog.String("job", "mpesa_stk_sweep")), metrics: d.Metrics, batch: STKSweepBatch}
}

// Work implements river.Worker.
func (w *STKSweepWorker) Work(ctx context.Context, _ *river.Job[queue.STKSweepArgs]) error {
	res, err := w.stk.Sweep(ctx, w.batch)
	// Whatever the sweep finished before an error is committed: count it.
	w.metrics.sweep(res)
	if res != (stk.SweepResult{}) {
		w.logger.InfoContext(ctx, "stk sweep",
			slog.Int("queried", res.Queried), slog.Int("failed", res.Failed),
			slog.Int("needs_review", res.NeedsReview), slog.Int("rescheduled", res.Rescheduled))
	}
	if err != nil {
		w.logger.ErrorContext(ctx, "stk sweep failed", slog.Any("error", err))
		return err
	}
	return nil
}

// Timeout implements river.Worker.
func (w *STKSweepWorker) Timeout(*river.Job[queue.STKSweepArgs]) time.Duration {
	return sweepTimeout
}
