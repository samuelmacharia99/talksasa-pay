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

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/jobs"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var (
	operator  = audit.CLI("jobs-test")
	payWorker = audit.System("test-payments")
)

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *testdb.DB
	logger   *slog.Logger
	products *product.Service
	payers   *payer.Service
	requests *paymentrequest.Service
	ledger   *ledger.Service
	events   *events.Emitter
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
	alloc := allocation.NewService(logger, ev)
	return &env{
		t: t, ctx: context.Background(), db: d, logger: logger,
		products: product.NewService(product.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		payers:   payer.NewService(d.App, logger),
		requests: paymentrequest.NewService(d.App, alloc, ev, logger),
		ledger:   ledger.NewService(d.App, alloc, logger),
		events:   ev,
	}
}

func (e *env) deps(batch int) jobs.Deps {
	return jobs.Deps{Pool: e.db.App, Events: e.events, Logger: e.logger, AttemptRetention: 90 * 24 * time.Hour, BatchSize: batch}
}

func (e *env) product(prefix string) uuid.UUID {
	e.t.Helper()
	c, err := e.products.Create(e.ctx, operator, product.CreateParams{
		Name: prefix + " product", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatalf("create product %s: %v", prefix, err)
	}
	return c.Product.ID
}

var seq atomic.Int64

func (e *env) payer(productID uuid.UUID) payer.Payer {
	e.t.Helper()
	ref := fmt.Sprintf("C%d", seq.Add(1))
	p, _, err := e.payers.Upsert(e.ctx, operator, productID, payer.UpsertParams{
		ExternalRef: ref, DisplayName: "Customer " + ref, Email: "c" + ref + "@example.com", Phones: []string{"0712345678"},
	})
	if err != nil {
		e.t.Fatalf("create payer: %v", err)
	}
	return p
}

// request creates an open request. expiry: nil = never expires; otherwise
// the request is created with a future expiry and then backdated by the
// owner role to expiry (the API rejects past expiries).
func (e *env) request(pr payer.Payer, amount int64, expiry *time.Time) uuid.UUID {
	e.t.Helper()
	n := seq.Add(1)
	p := paymentrequest.CreateParams{
		PayerAccountID: pr.ID, ProductReference: fmt.Sprintf("INV-%d", n), Description: "Invoice",
		Amount: amount, IdempotencyKey: fmt.Sprintf("idem-%d", n),
	}
	if expiry != nil {
		future := time.Now().Add(time.Hour)
		p.ExpiresAt = &future
	}
	c, err := e.requests.Create(e.ctx, audit.Product(pr.ProductID), pr.ProductID, p)
	if err != nil {
		e.t.Fatalf("create request: %v", err)
	}
	if expiry != nil {
		e.setExpiry(c.Request.ID, *expiry)
	}
	return c.Request.ID
}

func (e *env) setExpiry(id uuid.UUID, at time.Time) {
	e.t.Helper()
	if _, err := e.db.Owner.Exec(e.ctx, `UPDATE payment_requests SET expires_at = $2 WHERE id = $1`, id, at); err != nil {
		e.t.Fatalf("backdate expiry: %v", err)
	}
}

var receiptSeq atomic.Int64

func (e *env) pay(pr payer.Payer, amount money.KES, target *uuid.UUID) error {
	ch := store.TxnChannelMpesaC2b
	if target != nil {
		ch = store.TxnChannelMpesaStk
	}
	receipt := fmt.Sprintf("TST%07d", receiptSeq.Add(1))
	_, _, err := e.ledger.RecordAndAllocate(e.ctx, payWorker, ledger.IncomingPayment{
		Channel: ch, Receipt: receipt, Amount: amount, Phone: "254712345678", BillRef: pr.AccountNumber,
		ProductID: &pr.ProductID, PayerAccountID: &pr.ID, PaymentRequestID: target,
		RawPayload: []byte(`{"TransID":"` + receipt + `"}`),
	})
	return err
}

func (e *env) state(id uuid.UUID) (store.PaymentRequestStatus, int64) {
	e.t.Helper()
	var st store.PaymentRequestStatus
	var paid int64
	if err := e.db.App.QueryRow(e.ctx, `SELECT status, amount_paid FROM payment_requests WHERE id = $1`, id).Scan(&st, &paid); err != nil {
		e.t.Fatal(err)
	}
	return st, paid
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
	q := store.New(e.db.App)
	drift, err := q.FindBalanceDrift(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(drift) != 0 {
		e.t.Fatalf("balance drift: %+v", drift)
	}
	bad, err := q.VerifyAuditChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if bad != 0 {
		e.t.Fatalf("audit chain broken at %d", bad)
	}
}

func ago(d time.Duration) *time.Time { t := time.Now().Add(-d); return &t }

func TestExpirySweepOnlyExpiresDueOpenRequests(t *testing.T) {
	e := newEnv(t)
	pid := e.product("EXPA")
	pr := e.payer(pid)

	var due []uuid.UUID
	for range 5 {
		due = append(due, e.request(pr, 1000, ago(time.Minute)))
	}
	future := e.request(pr, 1000, nil)
	e.setExpiry(future, time.Now().Add(time.Hour))
	noExpiry := e.request(pr, 1000, nil)

	// Paid and partially paid, then made "due": they must not expire.
	partial := e.request(pr, 1000, nil)
	if err := e.pay(pr, 400, &partial); err != nil {
		t.Fatal(err)
	}
	e.setExpiry(partial, *ago(time.Minute))
	paid := e.request(pr, 1000, nil)
	if err := e.pay(pr, 1000, &paid); err != nil {
		t.Fatal(err)
	}
	e.setExpiry(paid, *ago(time.Minute))
	cancelled := e.request(pr, 1000, nil)
	if _, err := e.requests.Cancel(e.ctx, operator, pid, cancelled); err != nil {
		t.Fatal(err)
	}
	e.setExpiry(cancelled, *ago(time.Minute))

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
		if st, _ := e.state(id); st != store.PaymentRequestStatusExpired {
			t.Fatalf("due request %s is %s", id, st)
		}
		if c := e.eventCount(id, events.PaymentRequestExpired); c != 1 {
			t.Fatalf("request %s has %d expired events", id, c)
		}
	}
	for id, want := range map[uuid.UUID]store.PaymentRequestStatus{
		future: store.PaymentRequestStatusOpen, noExpiry: store.PaymentRequestStatusOpen,
		partial: store.PaymentRequestStatusPartiallyPaid, paid: store.PaymentRequestStatusPaid,
		cancelled: store.PaymentRequestStatusCancelled,
	} {
		if st, _ := e.state(id); st != want {
			t.Fatalf("request %s is %s, want %s", id, st, want)
		}
		if c := e.eventCount(id, events.PaymentRequestExpired); c != 0 {
			t.Fatalf("request %s (%s) got an expired event", id, want)
		}
	}

	// The event carries the request's new state and has a webhook delivery.
	var status string
	var deliveries int
	if err := e.db.App.QueryRow(e.ctx, `
SELECT e.data->>'status', (SELECT count(*) FROM webhook_deliveries d WHERE d.event_id = e.id)
  FROM events e WHERE e.subject_id = $1 AND e.type = $2`, due[0], events.PaymentRequestExpired).Scan(&status, &deliveries); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || deliveries != 1 {
		t.Fatalf("expired event: status=%q deliveries=%d", status, deliveries)
	}

	// Audit: one row per product per batch, counts adding up, system actor.
	var rows, total int
	if err := e.db.App.QueryRow(e.ctx, `
SELECT count(*), coalesce(sum((details->>'count')::int), 0) FROM audit_log
 WHERE action = 'payment_requests.expired' AND actor_type = 'system' AND actor_id = 'expiry-job' AND product_id = $1`, pid).Scan(&rows, &total); err != nil {
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
		if c := e.eventCount(id, events.PaymentRequestExpired); c != 1 {
			t.Fatalf("request %s has %d expired events after rerun", id, c)
		}
	}
	e.assertIntegrity()
}

func TestExpiryConcurrentSweepsNeverDoubleExpire(t *testing.T) {
	e := newEnv(t)
	var ids []uuid.UUID
	for _, prefix := range []string{"EXPB", "EXPC"} {
		pid := e.product(prefix)
		pr := e.payer(pid)
		for range 30 {
			ids = append(ids, e.request(pr, 500, ago(time.Minute)))
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
		if st, _ := e.state(id); st != store.PaymentRequestStatusExpired {
			t.Fatalf("request %s is %s", id, st)
		}
		if c := e.eventCount(id, events.PaymentRequestExpired); c != 1 {
			t.Fatalf("request %s has %d expired events", id, c)
		}
	}
	e.assertIntegrity()
}

func TestExpiryRacingPaymentNeverBothPaidAndExpired(t *testing.T) {
	e := newEnv(t)
	pid := e.product("EXPD")
	w := jobs.NewExpiryWorker(e.deps(jobs.DefaultBatchSize))

	outcomes := map[store.PaymentRequestStatus]int{}
	for i := range 20 {
		pr := e.payer(pid)
		id := e.request(pr, 1000, ago(time.Second))

		var wg sync.WaitGroup
		start := make(chan struct{})
		var payErr, sweepErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// Unaddressed C2B: allocation picks the oldest payable request, or
			// credits the payer if the request already expired.
			payErr = e.pay(pr, 1000, nil)
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
		if payErr != nil || sweepErr != nil {
			t.Fatalf("round %d: pay=%v sweep=%v", i, payErr, sweepErr)
		}

		st, amountPaid := e.state(id)
		p, err := e.payers.Get(e.ctx, pid, pr.ID)
		if err != nil {
			t.Fatal(err)
		}
		expired, paidEv := e.eventCount(id, events.PaymentRequestExpired), e.eventCount(id, events.PaymentRequestPaid)
		switch st {
		case store.PaymentRequestStatusPaid:
			if amountPaid != 1000 || p.CreditBalance != 0 || expired != 0 || paidEv != 1 {
				t.Fatalf("round %d paid: amount_paid=%d credit=%d expired_events=%d paid_events=%d", i, amountPaid, p.CreditBalance, expired, paidEv)
			}
		case store.PaymentRequestStatusExpired:
			if amountPaid != 0 || p.CreditBalance != 1000 || expired != 1 || paidEv != 0 {
				t.Fatalf("round %d expired: amount_paid=%d credit=%d expired_events=%d paid_events=%d", i, amountPaid, p.CreditBalance, expired, paidEv)
			}
		default:
			t.Fatalf("round %d: request ended %s", i, st)
		}
		outcomes[st]++
	}
	t.Logf("race outcomes: %v", outcomes)
	e.assertIntegrity()
}

func TestRetentionDeletesOnlyOldAttempts(t *testing.T) {
	e := newEnv(t)
	pid := e.product("RETA")
	pr := e.payer(pid)
	reqID := e.request(pr, 1000, nil)
	var eventID uuid.UUID
	if err := e.db.App.QueryRow(e.ctx, `SELECT id FROM events WHERE subject_id = $1 LIMIT 1`, reqID).Scan(&eventID); err != nil {
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
	res, err := client.Insert(e.ctx, queue.WebhookArgs{EventID: uuid.New(), ProductID: uuid.New()}, nil)
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
