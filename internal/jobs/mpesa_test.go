package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/talksasa/talksasa-pay/internal/jobs"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/stk"
)

// fakeSTK records calls; failures makes ProcessCallback fail that many times.
type fakeSTK struct {
	mu        sync.Mutex
	processed []uuid.UUID
	calls     int
	failures  int
	sweeps    int
	sweepLim  int
	sweepRes  stk.SweepResult
	sweepErr  error
	sweptOnce chan struct{}
}

func newFakeSTK() *fakeSTK {
	return &fakeSTK{sweptOnce: make(chan struct{})}
}

func (f *fakeSTK) ProcessCallback(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return errors.New("database unavailable")
	}
	f.processed = append(f.processed, id)
	return nil
}

func (f *fakeSTK) Sweep(_ context.Context, limit int) (stk.SweepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	f.sweepLim = limit
	if f.sweeps == 1 {
		close(f.sweptOnce)
	}
	return f.sweepRes, f.sweepErr
}

func quietDeps() jobs.Deps {
	return jobs.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AttemptRetention: 48 * time.Hour}
}

func TestMpesaJobsOnlyWhenEnabled(t *testing.T) {
	d := quietDeps()
	if _, ok := jobs.Queues(d)[queue.QueueMpesa]; ok {
		t.Fatal("mpesa queue configured with M-Pesa off")
	}
	if n := len(jobs.PeriodicJobs(d, time.Minute)); n != 2 {
		t.Fatalf("periodic jobs with M-Pesa off = %d, want 2", n)
	}

	d.STK = newFakeSTK()
	if q := jobs.Queues(d); q[queue.QueueMpesa].MaxWorkers != jobs.DefaultMpesaConcurrency {
		t.Fatalf("mpesa queue default concurrency: %+v", q)
	}
	d.MpesaConcurrency = 5
	if q := jobs.Queues(d); q[queue.QueueMpesa].MaxWorkers != 5 {
		t.Fatalf("mpesa queue concurrency: %+v", q)
	}
	if n := len(jobs.PeriodicJobs(d, time.Minute)); n != 3 {
		t.Fatalf("periodic jobs with M-Pesa on = %d, want 3", n)
	}
	d.MpesaConcurrency = -1
	if err := jobs.Register(river.NewWorkers(), d); err == nil {
		t.Fatal("Register accepted a negative MpesaConcurrency")
	}
}

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestCallbackWorkerReportsFailureForRetry(t *testing.T) {
	reg := metrics.NewRegistry()
	d := quietDeps()
	f := newFakeSTK()
	f.failures = 1
	d.STK, d.Metrics = f, jobs.NewMetrics(reg)
	w := jobs.NewCallbackWorker(d)
	id := uuid.New()
	job := &river.Job[queue.ProcessCallbackArgs]{JobRow: &rivertype.JobRow{Attempt: 1}, Args: queue.ProcessCallbackArgs{CallbackID: id}}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("a failed ProcessCallback must fail the job so River retries it")
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if len(f.processed) != 1 || f.processed[0] != id {
		t.Fatalf("processed %v", f.processed)
	}
	if w.Timeout(job) <= 0 {
		t.Fatal("callback job needs a timeout")
	}
	body := scrape(t, reg)
	for _, want := range []string{
		`talksasa_pay_mpesa_callbacks_total{outcome="processed"} 1`,
		`talksasa_pay_mpesa_callbacks_total{outcome="failed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestSTKSweepWorkerCountsOutcomes(t *testing.T) {
	reg := metrics.NewRegistry()
	d := quietDeps()
	f := newFakeSTK()
	f.sweepRes = stk.SweepResult{Queried: 4, Failed: 1, NeedsReview: 2, Rescheduled: 1}
	d.STK, d.Metrics = f, jobs.NewMetrics(reg)
	w := jobs.NewSTKSweepWorker(d)
	if err := w.Work(context.Background(), &river.Job[queue.STKSweepArgs]{JobRow: &rivertype.JobRow{}}); err != nil {
		t.Fatal(err)
	}
	if f.sweepLim != jobs.STKSweepBatch {
		t.Fatalf("sweep limit %d", f.sweepLim)
	}
	// Partial progress before an error is still counted, and the error
	// fails the job.
	f.sweepRes, f.sweepErr = stk.SweepResult{Queried: 1}, errors.New("boom")
	if err := w.Work(context.Background(), &river.Job[queue.STKSweepArgs]{JobRow: &rivertype.JobRow{}}); err == nil {
		t.Fatal("sweep error swallowed")
	}
	body := scrape(t, reg)
	for _, want := range []string{
		`talksasa_pay_mpesa_stk_sweep_pushes_total{outcome="queried"} 5`,
		`talksasa_pay_mpesa_stk_sweep_pushes_total{outcome="failed"} 1`,
		`talksasa_pay_mpesa_stk_sweep_pushes_total{outcome="needs_review"} 2`,
		`talksasa_pay_mpesa_stk_sweep_pushes_total{outcome="rescheduled"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

// End to end through River: the sweep runs on start (leader), and a
// callback job that fails is retried until it succeeds.
func TestMpesaJobsRunThroughRiver(t *testing.T) {
	e := newEnv(t)
	f := newFakeSTK()
	f.failures = 1
	d := e.deps(0)
	d.STK = f
	client, err := jobs.NewClient(d, jobs.ClientOptions{ExpiryEvery: time.Minute, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runner := jobs.NewRunner(client, e.logger, nil, 5*time.Second)
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	defer func() {
		cancel()
		waitStopped(t, done, 15*time.Second)
	}()

	select {
	case <-f.sweptOnce:
	case <-time.After(20 * time.Second):
		t.Fatal("STK sweep never ran (RunOnStart)")
	}

	id := uuid.New()
	res, err := client.Insert(e.ctx, queue.ProcessCallbackArgs{CallbackID: id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The first attempt fails; River retries it (backoff ~1s) and the
	// second attempt succeeds.
	deadline := time.Now().Add(20 * time.Second)
	for {
		job, err := client.JobGet(e.ctx, res.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == rivertype.JobStateCompleted {
			if len(job.Errors) != 1 || job.Attempt != 2 {
				t.Fatalf("completed with attempt=%d errors=%d, want 2 and 1", job.Attempt, len(job.Errors))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback job state %s, want completed after retry", job.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.processed) != 1 || f.processed[0] != id || f.calls != 2 {
		t.Fatalf("processed=%v calls=%d", f.processed, f.calls)
	}
}
