// Package jobs is the worker side of the River queue: the maintenance jobs
// (payment request expiry, webhook attempt retention), the registration of
// every job worker, and the Runner that starts the River client, reports
// health and shuts down gracefully.
//
// The API process only inserts jobs (queue.NewInsertOnly); the
// `talksasa-pay worker` command builds its client with NewClient and runs it
// with a Runner.
package jobs

import (
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
)

// MaintenanceConcurrency is the worker count for the maintenance queue.
const MaintenanceConcurrency = 2

// RetentionEvery is how often webhook attempt logs are pruned.
const RetentionEvery = time.Hour

// Deps are what the job workers need.
type Deps struct {
	Pool   *db.Pool
	Events *events.Emitter
	Logger *slog.Logger
	// Metrics may be nil (no metrics recorded).
	Metrics *Metrics

	// AttemptRetention: webhook_attempts older than this are deleted.
	AttemptRetention time.Duration
	// BatchSize for maintenance statements; 0 = DefaultBatchSize.
	BatchSize int

	// Webhook delivers events (queue.WebhookArgs). Optional: when nil the
	// webhooks queue is not worked (jobs accumulate until a worker that has
	// it runs) and a warning is logged.
	Webhook river.Worker[queue.WebhookArgs]
	// WebhookConcurrency is the webhooks queue's worker count.
	WebhookConcurrency int

	// STK processes M-Pesa callbacks and runs the STK status sweep. nil when
	// M-Pesa is off: the mpesa queue, its workers and the sweep are not
	// registered at all.
	STK STKService
	// MpesaConcurrency is the mpesa queue's worker count
	// (TSP_WORKER_MPESA_CONCURRENCY); 0 = DefaultMpesaConcurrency.
	MpesaConcurrency int
}

func (d Deps) validate() error {
	if d.Pool == nil || d.Events == nil || d.Logger == nil {
		return errors.New("jobs: Pool, Events and Logger are required")
	}
	if d.AttemptRetention < 24*time.Hour {
		return errors.New("jobs: AttemptRetention must be at least 24h")
	}
	if d.Webhook != nil && d.WebhookConcurrency < 1 {
		return errors.New("jobs: WebhookConcurrency must be positive")
	}
	if d.MpesaConcurrency < 0 {
		return errors.New("jobs: MpesaConcurrency must not be negative")
	}
	return nil
}

func (d Deps) mpesaConcurrency() int {
	if d.MpesaConcurrency > 0 {
		return d.MpesaConcurrency
	}
	return DefaultMpesaConcurrency
}

func (d Deps) batchSize() int {
	if d.BatchSize > 0 {
		return d.BatchSize
	}
	return DefaultBatchSize
}

// NewExpiryWorker builds the expiry worker (exported for tests and tools).
func NewExpiryWorker(d Deps) *ExpiryWorker {
	return &ExpiryWorker{pool: d.Pool, events: d.Events, logger: d.Logger.With(slog.String("job", "expiry")),
		metrics: d.Metrics, batchSize: d.batchSize()}
}

// NewRetentionWorker builds the retention worker (exported for tests and tools).
func NewRetentionWorker(d Deps) *RetentionWorker {
	return &RetentionWorker{pool: d.Pool, logger: d.Logger.With(slog.String("job", "retention")),
		metrics: d.Metrics, retention: d.AttemptRetention, batchSize: d.batchSize(), now: time.Now}
}

// Register adds every job worker to workers. Extend here when adding a job
// kind; the webhook worker comes in through Deps.Webhook.
func Register(workers *river.Workers, d Deps) error {
	if err := d.validate(); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewExpiryWorker(d)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewRetentionWorker(d)); err != nil {
		return err
	}
	if d.Webhook != nil {
		if err := river.AddWorkerSafely(workers, d.Webhook); err != nil {
			return err
		}
	}
	if d.STK != nil {
		if err := river.AddWorkerSafely(workers, NewCallbackWorker(d)); err != nil {
			return err
		}
		if err := river.AddWorkerSafely(workers, NewSTKSweepWorker(d)); err != nil {
			return err
		}
	}
	return nil
}

// Queues returns the queues this worker process works. The webhooks queue is
// only included when a webhook worker is registered, so River never fetches
// jobs it has no worker for.
func Queues(d Deps) map[string]river.QueueConfig {
	q := map[string]river.QueueConfig{queue.QueueMaintenance: {MaxWorkers: MaintenanceConcurrency}}
	if d.Webhook != nil {
		q[queue.QueueWebhooks] = river.QueueConfig{MaxWorkers: d.WebhookConcurrency}
	}
	if d.STK != nil {
		q[queue.QueueMpesa] = river.QueueConfig{MaxWorkers: d.mpesaConcurrency()}
	}
	return q
}

// PeriodicJobs schedules the maintenance jobs, plus the STK status sweep
// when M-Pesa is on (d.STK set). Only the elected leader among worker
// processes enqueues them; the args are also unique per period, so a
// leadership change cannot double-schedule.
func PeriodicJobs(d Deps, expiryEvery time.Duration) []*river.PeriodicJob {
	jobs := []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(expiryEvery),
			func() (river.JobArgs, *river.InsertOpts) { return queue.ExpireRequestsArgs{}, nil },
			&river.PeriodicJobOpts{ID: "expire_payment_requests", RunOnStart: true}),
		river.NewPeriodicJob(river.PeriodicInterval(RetentionEvery),
			func() (river.JobArgs, *river.InsertOpts) { return queue.RetentionArgs{}, nil },
			&river.PeriodicJobOpts{ID: "retention", RunOnStart: true}),
	}
	if d.STK != nil {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(STKSweepEvery),
			func() (river.JobArgs, *river.InsertOpts) { return queue.STKSweepArgs{}, nil },
			&river.PeriodicJobOpts{ID: "mpesa_stk_sweep", RunOnStart: true}))
	}
	return jobs
}

// ClientOptions tune the worker's River client.
type ClientOptions struct {
	// ExpiryEvery is the expiry sweep interval (cfg.Worker.MaintenanceEvery).
	ExpiryEvery time.Duration
	// FetchPollInterval overrides River's poll interval (tests); 0 = default.
	FetchPollInterval time.Duration
}

// NewClient builds a River client that works jobs: all workers registered,
// queues configured and maintenance jobs scheduled.
func NewClient(d Deps, o ClientOptions) (*queue.Client, error) {
	if o.ExpiryEvery <= 0 {
		return nil, errors.New("jobs: ExpiryEvery must be positive")
	}
	workers := river.NewWorkers()
	if err := Register(workers, d); err != nil {
		return nil, err
	}
	if d.Webhook == nil {
		d.Logger.Warn("no webhook worker registered: the webhooks queue is NOT being worked; webhook jobs will wait")
	}
	if d.STK == nil {
		d.Logger.Info("mpesa off: the mpesa queue (callbacks, STK status sweep) is not worked")
	}
	cfg := &river.Config{
		Logger:       d.Logger,
		Queues:       Queues(d),
		Workers:      workers,
		PeriodicJobs: PeriodicJobs(d, o.ExpiryEvery),
	}
	if o.FetchPollInterval > 0 {
		cfg.FetchPollInterval = o.FetchPollInterval
		cfg.FetchCooldown = min(o.FetchPollInterval, 100*time.Millisecond)
	}
	return river.NewClient(riverpgxv5.New(d.Pool), cfg)
}
