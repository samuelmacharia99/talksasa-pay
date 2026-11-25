package jobs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"

	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/stk"
)

// Metrics are the worker's Prometheus metrics. A nil *Metrics records nothing.
type Metrics struct {
	jobs     *prometheus.CounterVec
	duration *prometheus.HistogramVec
	expiredN prometheus.Counter
	prunedN  prometheus.Counter
	// M-Pesa: callbacks by outcome (processed, failed) and STK sweep
	// outcomes (queried, failed, needs_review, rescheduled).
	callbacks *prometheus.CounterVec
	sweepN    *prometheus.CounterVec
}

// NewMetrics registers the worker metrics.
func NewMetrics(r *metrics.Registry) *Metrics {
	m := &Metrics{
		jobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "talksasa_pay", Name: "jobs_total",
			Help: "Job runs by kind and outcome (completed, failed, cancelled, snoozed).",
		}, []string{"kind", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "talksasa_pay", Name: "job_duration_seconds", Help: "Job run time by kind.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		}, []string{"kind"}),
		expiredN: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "talksasa_pay", Name: "charges_expired_total", Help: "Charges expired by the expiry job.",
		}),
		prunedN: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "talksasa_pay", Name: "webhook_attempts_pruned_total", Help: "Webhook attempt log rows deleted by retention.",
		}),
	}
	m.callbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "talksasa_pay", Name: "mpesa_callbacks_total",
		Help: "M-Pesa callback processing attempts by outcome (processed, failed; failed attempts are retried).",
	}, []string{"outcome"})
	m.sweepN = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "talksasa_pay", Name: "mpesa_stk_sweep_pushes_total",
		Help: "STK pushes handled by the status sweep by outcome (queried, failed, needs_review, rescheduled).",
	}, []string{"outcome"})
	for _, o := range []string{"processed", "failed"} {
		m.callbacks.WithLabelValues(o)
	}
	for _, o := range []string{"queried", "failed", "needs_review", "rescheduled"} {
		m.sweepN.WithLabelValues(o)
	}
	r.Prometheus().MustRegister(m.jobs, m.duration, m.expiredN, m.prunedN, m.callbacks, m.sweepN)
	return m
}

func (m *Metrics) callback(ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.callbacks.WithLabelValues("processed").Inc()
	} else {
		m.callbacks.WithLabelValues("failed").Inc()
	}
}

func (m *Metrics) sweep(r stk.SweepResult) {
	if m == nil {
		return
	}
	for o, n := range map[string]int{"queried": r.Queried, "failed": r.Failed, "needs_review": r.NeedsReview, "rescheduled": r.Rescheduled} {
		if n > 0 {
			m.sweepN.WithLabelValues(o).Add(float64(n))
		}
	}
}

func (m *Metrics) expired(n int) {
	if m != nil && n > 0 {
		m.expiredN.Add(float64(n))
	}
}

func (m *Metrics) pruned(n int64) {
	if m != nil && n > 0 {
		m.prunedN.Add(float64(n))
	}
}

func (m *Metrics) observe(ev *river.Event) {
	if m == nil || ev == nil || ev.Job == nil {
		return
	}
	var outcome string
	switch ev.Kind {
	case river.EventKindJobCompleted:
		outcome = "completed"
	case river.EventKindJobFailed:
		outcome = "failed"
	case river.EventKindJobCancelled:
		outcome = "cancelled"
	case river.EventKindJobSnoozed:
		outcome = "snoozed"
	default:
		return
	}
	m.jobs.WithLabelValues(ev.Job.Kind, outcome).Inc()
	if ev.JobStats != nil {
		m.duration.WithLabelValues(ev.Job.Kind).Observe(ev.JobStats.RunDuration.Seconds())
	}
}

// Runner runs a working River client until its context ends, then shuts it
// down gracefully.
type Runner struct {
	client  *queue.Client
	logger  *slog.Logger
	metrics *Metrics
	// ShutdownTimeout is how long in-flight jobs may run after shutdown
	// begins before their contexts are cancelled.
	shutdownTimeout time.Duration
	running         atomic.Bool
}

// NewRunner wraps a client built by NewClient. m may be nil.
func NewRunner(c *queue.Client, logger *slog.Logger, m *Metrics, shutdownTimeout time.Duration) *Runner {
	return &Runner{client: c, logger: logger, metrics: m, shutdownTimeout: shutdownTimeout}
}

// hardStopTimeout bounds the wait for cancelled jobs to return after
// StopAndCancel.
const hardStopTimeout = 10 * time.Second

// Run starts the client and blocks until ctx is done (normal shutdown) or the
// client stops by itself (an error). On shutdown it stops fetching, lets
// in-flight jobs finish for up to the shutdown timeout, then cancels them.
// Interrupted jobs are retried by River later (all our jobs are idempotent).
func (r *Runner) Run(ctx context.Context) error {
	events, unsubscribe := r.client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed,
		river.EventKindJobCancelled, river.EventKindJobSnoozed)
	done := make(chan struct{})
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				r.metrics.observe(ev)
			case <-done:
				return
			}
		}
	}()
	defer func() {
		close(done)
		<-observed
		unsubscribe()
	}()

	// Not ctx itself: cancelling Start's context makes River hard-stop
	// immediately, and shutdown here is staged.
	if err := r.client.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	r.running.Store(true)
	r.logger.Info("worker started")

	select {
	case <-ctx.Done():
	case <-r.client.Stopped():
		r.running.Store(false)
		return errors.New("jobs: river client stopped unexpectedly")
	}
	r.running.Store(false)

	r.logger.Info("worker shutting down: no new jobs, waiting for running ones", slog.Duration("timeout", r.shutdownTimeout))
	sctx, cancel := context.WithTimeout(context.Background(), r.shutdownTimeout)
	defer cancel()
	if err := r.client.Stop(sctx); err == nil {
		r.logger.Info("worker stopped")
		return nil
	}
	r.logger.Warn("jobs still running at the shutdown deadline; cancelling them (they will be retried)")
	hctx, hcancel := context.WithTimeout(context.Background(), hardStopTimeout)
	defer hcancel()
	if err := r.client.StopAndCancel(hctx); err != nil {
		return errors.New("jobs: worker did not stop after cancelling running jobs")
	}
	r.logger.Info("worker stopped (running jobs cancelled)")
	return nil
}

// Healthy reports whether the client is started and not shutting down.
func (r *Runner) Healthy() bool { return r.running.Load() }

// HealthHandler serves GET /healthz: 200 while the client runs, else 503.
func (r *Runner) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !r.Healthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("stopped\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
}
