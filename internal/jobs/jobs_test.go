package jobs_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/jobs"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var (
	operator  = audit.CLI("jobs-test")
	payWorker = audit.System("test-payments")
)

type env struct {
	t      *testing.T
	ctx    context.Context
	db     *testdb.DB
	logger *slog.Logger
	apps   *app.Service
	events *events.Emitter
	// prefixes by app id (seeded charges need the app's prefix).
	prefixes map[uuid.UUID]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	kr, err := crypto.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), 0)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := queue.NewInsertOnly(d.App, logger)
	if err != nil {
		t.Fatal(err)
	}
	ev := events.NewEmitter(apiview.View{PayBaseURL: "https://pay.test/", Paybill: "174379"}, queue.NewEnqueuer(rc))
	return &env{
		t: t, ctx: context.Background(), db: d, logger: logger,
		apps: app.NewService(app.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		events:   ev,
		prefixes: map[uuid.UUID]string{},
	}
}

func (e *env) deps(batch int) jobs.Deps {
	return jobs.Deps{Pool: e.db.App, Events: e.events, Logger: e.logger, AttemptRetention: 90 * 24 * time.Hour, BatchSize: batch}
}

func (e *env) app(prefix string) uuid.UUID {
	e.t.Helper()
	c, err := e.apps.Create(e.ctx, operator, app.CreateParams{
		Name: prefix + " app", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatalf("create app %s: %v", prefix, err)
	}
	e.prefixes[c.App.ID] = prefix
	return c.App.ID
}

// charge seeds a pending charge expiring at expiresAt (zero = in 24 h).
func (e *env) charge(appID uuid.UUID, amount int64, expiresAt time.Time) store.Charge {
	e.t.Helper()
	return seed.Charge(e.t, e.ctx, e.db.App, appID, e.prefixes[appID], amount, seed.ChargeOpts{ExpiresAt: expiresAt})
}

func (e *env) state(id uuid.UUID) store.ChargeStatus {
	e.t.Helper()
	var st store.ChargeStatus
	if err := e.db.App.QueryRow(e.ctx, `SELECT status FROM charges WHERE id = $1`, id).Scan(&st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) eventCount(id uuid.UUID, typ string) int {
	e.t.Helper()
	var n int
	if err := e.db.App.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE subject_id = $1 AND type = $2`, id, typ).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) assertIntegrity() {
	e.t.Helper()
	bad, err := store.New(e.db.App).VerifyAuditChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if bad != 0 {
		e.t.Fatalf("audit chain broken at %d", bad)
	}
}

func ago(d time.Duration) time.Time { return time.Now().Add(-d) }

func TestExpirySweepOnlyExpiresDuePendingCharges(t *testing.T) {
	e := newEnv(t)
	pid := e.app("EXPA")
	q := store.New(e.db.App)

	var due []uuid.UUID
	for range 5 {
		due = append(due, e.charge(pid, 1000, ago(time.Minute)).ID)
	}
	future := e.charge(pid, 1000, time.Now().Add(time.Hour)).ID

	// Succeeded, cancelled and in review, all past expiry: they must not expire.
	paid := e.charge(pid, 1000, ago(time.Minute))
	seed.PayCharge(t, e.ctx, e.db.App, paid)
	cancelled := e.charge(pid, 1000, ago(time.Minute))
	if _, err := q.CancelCharge(e.ctx, store.CancelChargeParams{AppID: pid, ID: cancelled.ID}); err != nil {
		t.Fatal(err)
	}
	review := e.charge(pid, 1000, ago(time.Minute))
	if _, err := q.MarkChargeNeedsReview(e.ctx, review.ID); err != nil {
		t.Fatal(err)
	}

	// Batch size 2 so the sweep loops (2 + 2 + 1).
	w := jobs.NewExpiryWorker(e.deps(2))
	n, err := w.Sweep(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(due) {
		t.Fatalf("expired %d, want %d", n, len(due))
	}
	for _, id := range due {
		if st := e.state(id); st != store.ChargeStatusExpired {
			t.Fatalf("due charge %s is %s", id, st)
		}
		if c := e.eventCount(id, events.ChargeExpired); c != 1 {
			t.Fatalf("charge %s has %d expired events", id, c)
		}
	}
	for id, want := range map[uuid.UUID]store.ChargeStatus{
		future: store.ChargeStatusPending, paid.ID: store.ChargeStatusSucceeded,
		cancelled.ID: store.ChargeStatusCancelled, review.ID: store.ChargeStatusNeedsReview,
	} {
		if st := e.state(id); st != want {
			t.Fatalf("charge %s is %s, want %s", id, st, want)
		}
		if c := e.eventCount(id, events.ChargeExpired); c != 0 {
			t.Fatalf("charge %s (%s) got an expired event", id, want)
		}
	}

	// The event carries the charge's new state and has a webhook delivery.
	var status, object string
	var deliveries int
	if err := e.db.App.QueryRow(e.ctx, `
SELECT e.data->>'status', e.data->>'object', (SELECT count(*) FROM webhook_deliveries d WHERE d.event_id = e.id)
  FROM events e WHERE e.subject_id = $1 AND e.type = $2`, due[0], events.ChargeExpired).Scan(&status, &object, &deliveries); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || object != "charge" || deliveries != 1 {
		t.Fatalf("expired event: status=%q object=%q deliveries=%d", status, object, deliveries)
	}

	// Audit: one row per app per batch, counts adding up, system actor.
	var rows, total int
	if err := e.db.App.QueryRow(e.ctx, `
SELECT count(*), coalesce(sum((details->>'count')::int), 0) FROM audit_log
 WHERE action = 'charges.expired' AND actor_type = 'system' AND actor_id = 'expiry-job' AND app_id = $1`, pid).Scan(&rows, &total); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || total != len(due) {
		t.Fatalf("audit rows=%d total=%d, want 3 rows covering %d", rows, total, len(due))
	}

	// Idempotent: a second run finds nothing and emits nothing.
	n, err = w.Sweep(e.ctx)
	if err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
	for _, id := range due {
		if c := e.eventCount(id, events.ChargeExpired); c != 1 {
			t.Fatalf("charge %s has %d expired events after rerun", id, c)
		}
	}
	e.assertIntegrity()
}

func TestExpiryConcurrentSweepsNeverDoubleExpire(t *testing.T) {
	e := newEnv(t)
	var ids []uuid.UUID
	for _, prefix := range []string{"EXPB", "EXPC"} {
		pid := e.app(prefix)
		for range 30 {
			ids = append(ids, e.charge(pid, 500, ago(time.Minute)).ID)
		}
	}

	const sweepers = 8
	var total atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, sweepers)
	for range sweepers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := jobs.NewExpiryWorker(e.deps(7))
			<-start
			n, err := w.Sweep(e.ctx)
			total.Add(int64(n))
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if total.Load() != int64(len(ids)) {
		t.Fatalf("sweepers expired %d in total, want %d", total.Load(), len(ids))
	}
	for _, id := range ids {
		if st := e.state(id); st != store.ChargeStatusExpired {
			t.Fatalf("charge %s is %s", id, st)
		}
		if c := e.eventCount(id, events.ChargeExpired); c != 1 {
			t.Fatalf("charge %s has %d expired events", id, c)
		}
	}
	e.assertIntegrity()
}

// Late money is never dropped (rule 6): whichever of the payment and the
// sweep commits first, the charge ends succeeded, and expires at most once.
func TestExpiryRacingPaymentAlwaysEndsSucceeded(t *testing.T) {
	e := newEnv(t)
	pid := e.app("EXPD")
	w := jobs.NewExpiryWorker(e.deps(jobs.DefaultBatchSize))

	expiredFirst := 0
	for i := range 20 {
		c := e.charge(pid, 1000, ago(time.Second))
		var wg sync.WaitGroup
		start := make(chan struct{})
		var sweepErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			seed.PayCharge(t, e.ctx, e.db.App, c)
		}()
		go func() {
			defer wg.Done()
			<-start
			// Stagger so both orders (payment first, expiry first) occur.
			time.Sleep(time.Duration(i%5) * 4 * time.Millisecond)
			_, sweepErr = w.Sweep(e.ctx)
		}()
		close(start)
		wg.Wait()
		if sweepErr != nil {
			t.Fatalf("round %d: sweep=%v", i, sweepErr)
		}
		if st := e.state(c.ID); st != store.ChargeStatusSucceeded {
			t.Fatalf("round %d: charge ended %s", i, st)
		}
		n := e.eventCount(c.ID, events.ChargeExpired)
		if n > 1 {
			t.Fatalf("round %d: %d expired events", i, n)
		}
		expiredFirst += n
	}
	t.Logf("expired before the payment in %d of 20 rounds", expiredFirst)
	e.assertIntegrity()
}

func TestRetentionDeletesOnlyOldAttempts(t *testing.T) {
	e := newEnv(t)
	pid := e.app("RETA")
	c := e.charge(pid, 1000, time.Time{})
	var eventID uuid.UUID
	if err := db.WithTx(e.ctx, e.db.App, "system:test", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		eventID, err = e.events.Emit(ctx, q, pid, events.ChargeCancelled, events.SubjectCharge, c.ID, map[string]any{"id": c.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ages := []time.Duration{
		100 * 24 * time.Hour, 95 * 24 * time.Hour, 91 * 24 * time.Hour, 200 * 24 * time.Hour, 365 * 24 * time.Hour, // old
		89 * 24 * time.Hour, 10 * 24 * time.Hour, time.Hour, 0, // kept
	}
	for i, age := range ages {
		if _, err := e.db.Owner.Exec(e.ctx, `
INSERT INTO webhook_attempts (event_id, attempt, attempted_at, url_host, latency_ms) VALUES ($1, $2, now() - $3::interval, 'example.com', 5)`,
			eventID, i+1, fmt.Sprintf("%d seconds", int(age.Seconds()))); err != nil {
			t.Fatal(err)
		}
	}

	w := jobs.NewRetentionWorker(e.deps(2))
	n, err := w.Prune(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("pruned %d, want 5", n)
	}
	var left []int32
	rows, err := store.New(e.db.App).ListWebhookAttempts(e.ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		left = append(left, r.Attempt)
	}
	if fmt.Sprint(left) != "[6 7 8 9]" {
		t.Fatalf("remaining attempts %v, want [6 7 8 9]", left)
	}
	if n, err := w.Prune(e.ctx); err != nil || n != 0 {
		t.Fatalf("second prune: n=%d err=%v", n, err)
	}
	var audits int
	if err := e.db.App.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE action = 'webhook_attempts.pruned' AND (details->>'count')::int = 5`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("retention audit rows = %d, want 1", audits)
	}
	e.assertIntegrity()
}

// Concurrent audit writers (the expiry job vs. API actions) must all append
// to the hash chain. Regression test for migration 000004.
func TestAuditChainConcurrentAppends(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	errs := make(chan error, 160)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				errs <- db.WithTx(e.ctx, e.db.App, "system:audit-test", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
					return audit.Record(ctx, q, audit.Entry{Actor: audit.System("audit-test"), Action: "audit_test.appended",
						EntityType: "test", Details: map[string]any{"writer": g}})
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent audit append: %v", err)
		}
	}
	e.assertIntegrity()
}

// fakeWebhook stands in for the webhook delivery worker.
type fakeWebhook struct {
	river.WorkerDefaults[queue.WebhookArgs]
	started   chan struct{}
	hold      time.Duration // 0 = block until the job context ends
	finished  atomic.Bool
	cancelled atomic.Bool
}

func (f *fakeWebhook) Work(ctx context.Context, _ *river.Job[queue.WebhookArgs]) error {
	close(f.started)
	var wait <-chan time.Time
	if f.hold > 0 {
		wait = time.After(f.hold)
	}
	select {
	case <-wait:
		f.finished.Store(true)
		return nil
	case <-ctx.Done():
		f.cancelled.Store(true)
		return ctx.Err()
	}
}

func startRunner(t *testing.T, e *env, hook *fakeWebhook, shutdown time.Duration) (*queue.Client, *jobs.Runner, context.CancelFunc, chan error) {
	t.Helper()
	d := e.deps(0)
	d.Webhook, d.WebhookConcurrency = hook, 2
	client, err := jobs.NewClient(d, jobs.ClientOptions{ExpiryEvery: time.Minute, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runner := jobs.NewRunner(client, e.logger, nil, shutdown)
	if runner.Healthy() {
		t.Fatal("healthy before start")
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !runner.Healthy() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("runner never became healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec := httptest.NewRecorder()
	runner.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz while running = %d", rec.Code)
	}
	return client, runner, cancel, done
}

func insertWebhookJob(t *testing.T, e *env, client *queue.Client, hook *fakeWebhook) int64 {
	t.Helper()
	res, err := client.Insert(e.ctx, queue.WebhookArgs{EventID: uuid.New(), AppID: uuid.New()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-hook.started:
	case <-time.After(10 * time.Second):
		t.Fatal("webhook job never started")
	}
	return res.Job.ID
}

func waitStopped(t *testing.T, done chan error, within time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(within):
		t.Fatal("runner did not stop in time")
	}
}

func TestRunnerGracefulShutdownLetsInFlightJobFinish(t *testing.T) {
	e := newEnv(t)
	hook := &fakeWebhook{started: make(chan struct{}), hold: 700 * time.Millisecond}
	client, runner, cancel, done := startRunner(t, e, hook, 10*time.Second)
	jobID := insertWebhookJob(t, e, client, hook)

	cancel() // SIGTERM
	waitStopped(t, done, 15*time.Second)
	if !hook.finished.Load() || hook.cancelled.Load() {
		t.Fatalf("in-flight job: finished=%v cancelled=%v, want finished", hook.finished.Load(), hook.cancelled.Load())
	}
	if runner.Healthy() {
		t.Fatal("still healthy after stop")
	}
	rec := httptest.NewRecorder()
	runner.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz after stop = %d", rec.Code)
	}
	job, err := client.JobGet(e.ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != rivertype.JobStateCompleted {
		t.Fatalf("job state %s, want completed", job.State)
	}
}

func TestRunnerCancelsJobsStillRunningAtDeadline(t *testing.T) {
	e := newEnv(t)
	hook := &fakeWebhook{started: make(chan struct{})} // never finishes on its own
	client, _, cancel, done := startRunner(t, e, hook, 300*time.Millisecond)
	jobID := insertWebhookJob(t, e, client, hook)

	began := time.Now()
	cancel()
	waitStopped(t, done, 15*time.Second)
	if el := time.Since(began); el > 12*time.Second {
		t.Fatalf("shutdown took %s", el)
	}
	if !hook.cancelled.Load() || hook.finished.Load() {
		t.Fatal("job running past the deadline should have been cancelled")
	}
	job, err := client.JobGet(e.ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	// Cancelled by shutdown, so River will retry it later; never lost.
	if job.State == rivertype.JobStateCompleted || job.State == rivertype.JobStateDiscarded {
		t.Fatalf("job state %s, want it left for retry", job.State)
	}
}

func TestNewClientWithoutWebhookWorkerSkipsWebhookQueue(t *testing.T) {
	d := jobs.Deps{AttemptRetention: 48 * time.Hour}
	if q := jobs.Queues(d); len(q) != 1 || q[queue.QueueMaintenance].MaxWorkers != jobs.MaintenanceConcurrency {
		t.Fatalf("queues without webhook worker: %+v", q)
	}
	d.Webhook, d.WebhookConcurrency = &fakeWebhook{}, 7
	if q := jobs.Queues(d); q[queue.QueueWebhooks].MaxWorkers != 7 {
		t.Fatalf("queues with webhook worker: %+v", q)
	}
	if err := jobs.Register(river.NewWorkers(), jobs.Deps{}); err == nil {
		t.Fatal("Register accepted missing deps")
	}
	if len(jobs.PeriodicJobs(jobs.Deps{}, time.Minute)) != 2 {
		t.Fatal("want expiry and retention periodic jobs")
	}
}
