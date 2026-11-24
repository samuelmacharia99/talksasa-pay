package webhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var operator = audit.CLI("webhook-test")

// receiver is an httptest webhook endpoint with a scripted response.
type receiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []captured
	hits  atomic.Int64
	reply func(n int) (status int, hdr map[string]string, body string)
}

type captured struct {
	hdr  http.Header
	body []byte
	at   time.Time
}

func newReceiver(t *testing.T, reply func(n int) (int, map[string]string, string)) *receiver {
	r := &receiver{reply: reply}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		n := int(r.hits.Add(1))
		r.mu.Lock()
		r.reqs = append(r.reqs, captured{hdr: req.Header.Clone(), body: b, at: time.Now()})
		r.mu.Unlock()
		status, hdr, body := r.reply(n)
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) last() captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1]
}

func always(status int) func(int) (int, map[string]string, string) {
	return func(int) (int, map[string]string, string) { return status, nil, `{"ok":true}` }
}

type harness struct {
	t        *testing.T
	ctx      context.Context
	db       *testdb.DB
	products *product.Service
	logs     *syncBuffer
	logger   *slog.Logger
	worker   *Worker
	river    *queue.Client
	emitter  *events.Emitter
}

type harnessOpts struct {
	threshold int
	cooldown  time.Duration
	noStart   bool
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	d := testdb.New(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	kr, err := crypto.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), 0)
	if err != nil {
		t.Fatal(err)
	}
	products := product.NewService(product.Options{
		Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
	})
	hc, err := NewHTTPClient(ClientOptions{Env: config.EnvDevelopment, AllowLoopback: true, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(Options{
		Pool: d.App, Secrets: products, Logger: logger, Client: hc, Env: config.EnvDevelopment, Timeout: 3 * time.Second,
		BreakerThreshold: o.threshold, BreakerCooldown: o.cooldown,
		Backoff: func(int) time.Duration { return 20 * time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	rc, err := river.NewClient(riverpgxv5.New(d.App), &river.Config{
		Logger:            slog.New(slog.DiscardHandler),
		Queues:            map[string]river.QueueConfig{queue.QueueWebhooks: {MaxWorkers: 10}},
		Workers:           workers,
		FetchCooldown:     5 * time.Millisecond,
		FetchPollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !o.noStart {
		if err := rc.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = rc.Stop(sctx)
		})
	}
	return &harness{
		t: t, ctx: ctx, db: d, products: products, logs: logs, logger: logger, worker: w, river: rc,
		emitter: events.NewEmitter(apiview.View{PayBaseURL: "https://pay.test/"}, queue.NewEnqueuer(rc)),
	}
}

func (h *harness) product(prefix, hookURL string) product.Created {
	h.t.Helper()
	c, err := h.products.Create(h.ctx, operator, product.CreateParams{Name: prefix + " product", AccountPrefix: prefix, WebhookURL: hookURL})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// emit writes an event (+ delivery row + River job) like the real services do.
func (h *harness) emit(productID uuid.UUID) uuid.UUID {
	h.t.Helper()
	var id uuid.UUID
	err := db.WithTx(h.ctx, h.db.App, operator.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		id, err = h.emitter.Emit(ctx, q, productID, events.PaymentRequestPaid, "payment_request", uuid.New(),
			map[string]any{"object": "payment_request", "amount": 1500, "status": "paid"})
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) delivery(eventID uuid.UUID) store.WebhookDelivery {
	h.t.Helper()
	d, err := store.New(h.db.App).GetWebhookDelivery(h.ctx, eventID)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) waitStatus(eventID uuid.UUID, want store.WebhookDeliveryStatus) store.WebhookDelivery {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		d := h.delivery(eventID)
		if d.Status == want {
			return d
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("delivery %s: status %s after 15s, want %s (attempts %d, last error %v)", eventID, d.Status, want, d.Attempts, deref(d.LastError))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) attempts(eventID uuid.UUID) []store.WebhookAttempt {
	h.t.Helper()
	a, err := store.New(h.db.App).ListWebhookAttempts(h.ctx, eventID)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestWorkerDelivers(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	rcv := newReceiver(t, always(200))
	p := h.product("DLV", rcv.srv.URL+"/hooks?token=receiver-token-123")
	evID := h.emit(p.Product.ID)

	d := h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	if d.Attempts != 1 || d.DeliveredAt == nil || d.LastStatusCode == nil || *d.LastStatusCode != 200 {
		t.Fatalf("delivery row: %+v", d)
	}
	got := rcv.last()
	if got.hdr.Get("Content-Type") != "application/json" || got.hdr.Get("User-Agent") != UserAgent ||
		got.hdr.Get(HeaderEventID) != evID.String() || got.hdr.Get(HeaderEventType) != events.PaymentRequestPaid {
		t.Fatalf("headers: %v", got.hdr)
	}
	sig := got.hdr.Get(HeaderSignature)
	if strings.Count(sig, "v1=") != 1 || !Verify([]byte(p.WebhookSecret), sig, got.body, time.Now(), SignatureTolerance) {
		t.Fatalf("signature %q does not verify", sig)
	}
	var env struct {
		Object     string          `json:"object"`
		ID         uuid.UUID       `json:"id"`
		Type       string          `json:"type"`
		APIVersion string          `json:"api_version"`
		CreatedAt  time.Time       `json:"created_at"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(got.body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Object != "event" || env.ID != evID || env.Type != events.PaymentRequestPaid || env.APIVersion != apiview.APIVersion ||
		env.CreatedAt.IsZero() || !strings.Contains(string(env.Data), `"amount":1500`) {
		t.Fatalf("envelope: %s", got.body)
	}

	at := h.attempts(evID)
	if len(at) != 1 || at[0].Attempt != 1 || at[0].StatusCode == nil || *at[0].StatusCode != 200 ||
		at[0].UrlHost != strings.TrimPrefix(rcv.srv.URL, "http://") || deref(at[0].ResponseBody) != `{"ok":true}` || at[0].Error != nil {
		t.Fatalf("attempt log: %+v", at)
	}
	h.assertNoSecrets(p.WebhookSecret, "receiver-token-123", "/hooks")

	// Idempotent: running the job again for a delivered event sends nothing.
	err := h.worker.Work(h.ctx, &river.Job[queue.WebhookArgs]{
		JobRow: &rivertype.JobRow{ID: 999999, Attempt: 1, MaxAttempts: 25},
		Args:   queue.WebhookArgs{EventID: evID, ProductID: p.Product.ID},
	})
	if err != nil || rcv.hits.Load() != 1 || h.delivery(evID).Attempts != 1 {
		t.Fatalf("redelivered a delivered event: err=%v hits=%d", err, rcv.hits.Load())
	}
}

// assertNoSecrets checks the attempt log, delivery rows and worker logs.
func (h *harness) assertNoSecrets(needles ...string) {
	h.t.Helper()
	var dump string
	err := h.db.Owner.QueryRow(h.ctx, `SELECT coalesce(string_agg(a::text, '|'), '') || '#' ||
		(SELECT coalesce(string_agg(d::text, '|'), '') FROM webhook_deliveries d) FROM webhook_attempts a`).Scan(&dump)
	if err != nil {
		h.t.Fatal(err)
	}
	logs := h.logs.String()
	for _, n := range needles {
		if strings.Contains(dump, n) {
			h.t.Errorf("attempt/delivery rows contain %q", n)
		}
		if strings.Contains(logs, n) {
			h.t.Errorf("logs contain %q", n)
		}
	}
	if strings.Contains(logs, "v1=") {
		h.t.Error("logs contain a signature")
	}
}

func TestWorkerRetriesThenDelivers(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	rcv := newReceiver(t, func(n int) (int, map[string]string, string) {
		if n <= 2 {
			return 500, nil, "boom\x00\x1b[31m"
		}
		return 204, nil, ""
	})
	p := h.product("RTY", rcv.srv.URL)
	evID := h.emit(p.Product.ID)
	d := h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	if d.Attempts != 3 || rcv.hits.Load() != 3 {
		t.Fatalf("attempts %d hits %d", d.Attempts, rcv.hits.Load())
	}
	at := h.attempts(evID)
	if len(at) != 3 || *at[0].StatusCode != 500 || deref(at[0].Error) != "HTTP 500" || deref(at[0].ResponseBody) != "boom[31m" ||
		*at[2].StatusCode != 204 || at[2].ResponseBody != nil {
		t.Fatalf("attempt log: %+v / %q", at, deref(at[0].ResponseBody))
	}
	// The same event id on every attempt (receivers de-duplicate on it).
	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	for _, c := range rcv.reqs {
		if c.hdr.Get(HeaderEventID) != evID.String() {
			t.Fatal("event id changed between attempts")
		}
	}
}

func TestWorkerRetries4xx(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	rcv := newReceiver(t, func(n int) (int, map[string]string, string) {
		if n == 1 {
			return 401, nil, "bad signature"
		}
		return 200, nil, "ok"
	})
	p := h.product("FXX", rcv.srv.URL)
	evID := h.emit(p.Product.ID)
	d := h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	if d.Attempts != 2 {
		t.Fatalf("attempts %d", d.Attempts)
	}
	if at := h.attempts(evID); *at[0].StatusCode != 401 || deref(at[0].ResponseBody) != "bad signature" {
		t.Fatalf("attempt log: %+v", at)
	}
}

func TestWorkerSkips(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	rcv := newReceiver(t, always(200))
	none := h.product("NOU", "")
	evID := h.emit(none.Product.ID)
	d := h.waitStatus(evID, store.WebhookDeliveryStatusSkipped)
	if deref(d.LastError) != "no webhook URL configured" || d.Attempts != 0 {
		t.Fatalf("skip: %+v", d)
	}

	sus := h.product("SUS", rcv.srv.URL)
	if _, err := h.products.SetStatus(h.ctx, operator, sus.Product.ID, store.ProductStatusSuspended); err != nil {
		t.Fatal(err)
	}
	evID = h.emit(sus.Product.ID)
	d = h.waitStatus(evID, store.WebhookDeliveryStatusSkipped)
	if deref(d.LastError) != "product suspended" || rcv.hits.Load() != 0 {
		t.Fatalf("suspended: %+v hits=%d", d, rcv.hits.Load())
	}
	// The event is still in the pull feed.
	if _, err := store.New(h.db.App).GetEventForProduct(h.ctx, store.GetEventForProductParams{ProductID: sus.Product.ID, ID: evID}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSignsWithPreviousSecretDuringGrace(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	rcv := newReceiver(t, always(200))
	p := h.product("ROT", rcv.srv.URL)
	oldSecret := p.WebhookSecret
	newSecret, err := h.products.RotateWebhookSecret(h.ctx, operator, p.Product.ID)
	if err != nil {
		t.Fatal(err)
	}

	evID := h.emit(p.Product.ID)
	h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	got := rcv.last()
	sig := got.hdr.Get(HeaderSignature)
	if strings.Count(sig, "v1=") != 2 {
		t.Fatalf("want two signatures during grace: %s", sig)
	}
	for _, s := range []string{oldSecret, newSecret} {
		if !Verify([]byte(s), sig, got.body, time.Now(), SignatureTolerance) {
			t.Fatal("signature does not verify with both secrets")
		}
	}

	// Grace over: only the new secret signs.
	if _, err := h.db.Owner.Exec(h.ctx, `UPDATE products SET webhook_secret_prev_expires_at = now() - interval '1 second' WHERE id = $1`, p.Product.ID); err != nil {
		t.Fatal(err)
	}
	evID = h.emit(p.Product.ID)
	h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	got = rcv.last()
	sig = got.hdr.Get(HeaderSignature)
	if strings.Count(sig, "v1=") != 1 || !Verify([]byte(newSecret), sig, got.body, time.Now(), SignatureTolerance) ||
		Verify([]byte(oldSecret), sig, got.body, time.Now(), SignatureTolerance) {
		t.Fatalf("after grace: %s", sig)
	}
	h.assertNoSecrets(oldSecret, newSecret)
}

func TestWorkerCircuitBreakerSnoozes(t *testing.T) {
	h := newHarness(t, harnessOpts{threshold: 2, cooldown: time.Hour})
	rcv := newReceiver(t, always(503))
	p := h.product("BRK", rcv.srv.URL)
	evID := h.emit(p.Product.ID)

	// Two failures open the breaker; the next run is snoozed, not attempted.
	deadline := time.Now().Add(15 * time.Second)
	var state string
	var attempt int
	var snoozes int64
	for {
		err := h.db.Owner.QueryRow(h.ctx, `SELECT state::text, attempt, coalesce((metadata->>'snoozes')::bigint, 0)
			FROM river_job WHERE args->>'event_id' = $1`, evID.String()).Scan(&state, &attempt, &snoozes)
		if err != nil {
			t.Fatal(err)
		}
		if snoozes >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never snoozed: state=%s attempt=%d hits=%d", state, attempt, rcv.hits.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !h.worker.endpoints.open(p.Product.ID) {
		t.Fatal("breaker not open")
	}
	d := h.delivery(evID)
	if rcv.hits.Load() != 2 || d.Attempts != 2 || d.Status != store.WebhookDeliveryStatusPending {
		t.Fatalf("hits=%d delivery=%+v", rcv.hits.Load(), d)
	}
	// The snooze did not consume an attempt, and the job waits for the cool-down.
	if state != "scheduled" || attempt != 2 {
		t.Fatalf("job state=%s attempt=%d", state, attempt)
	}

	// Other events for the same endpoint are held too, without attempts.
	ev2 := h.emit(p.Product.ID)
	time.Sleep(300 * time.Millisecond)
	if d2 := h.delivery(ev2); d2.Attempts != 0 || rcv.hits.Load() != 2 {
		t.Fatalf("breaker let a request through: %+v hits=%d", d2, rcv.hits.Load())
	}
}

func TestWorkerFinalAttemptFailsThenAdminRedelivers(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	var healthy atomic.Bool
	rcv := newReceiver(t, func(int) (int, map[string]string, string) {
		if healthy.Load() {
			return 200, nil, "ok"
		}
		return 500, nil, "down"
	})
	p := h.product("FIN", rcv.srv.URL)

	// An event whose job has only 2 attempts (instead of 25).
	var evID uuid.UUID
	err := db.WithTx(h.ctx, h.db.App, operator.String(), func(ctx context.Context, q *store.Queries, tx pgx.Tx) error {
		var err error
		evID, err = events.NewEmitter(apiview.View{}, nil).Emit(ctx, q, p.Product.ID, events.PaymentRequestCreated, "payment_request", uuid.New(), map[string]any{"a": 1})
		if err != nil {
			return err
		}
		_, err = h.river.InsertTx(ctx, tx, queue.WebhookArgs{EventID: evID, ProductID: p.Product.ID}, &river.InsertOpts{MaxAttempts: 2})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d := h.waitStatus(evID, store.WebhookDeliveryStatusFailed)
	if d.Attempts != 2 || deref(d.LastError) != "HTTP 500" {
		t.Fatalf("failed row: %+v", d)
	}
	var state string
	waitFor(t, func() bool {
		_ = h.db.Owner.QueryRow(h.ctx, `SELECT state::text FROM river_job WHERE args->>'event_id' = $1`, evID.String()).Scan(&state)
		return state == "discarded"
	})

	deliveries := NewDeliveries(h.db.App, h.river)
	healthy.Store(true)
	rd, err := deliveries.Redeliver(h.ctx, operator, evID)
	if err != nil || rd.Status != store.WebhookDeliveryStatusPending {
		t.Fatalf("redeliver: %v %+v", err, rd)
	}
	d = h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	if d.Attempts != 3 {
		t.Fatalf("attempts after redelivery: %d", d.Attempts)
	}
	if at := h.attempts(evID); len(at) != 3 || at[2].Attempt != 3 {
		t.Fatalf("attempt numbering: %+v", at)
	}
	if _, err := deliveries.Redeliver(h.ctx, operator, evID); !errors.Is(err, ErrNotRedeliverable) {
		t.Fatalf("delivered event redelivered: %v", err)
	}
}

func TestWorkerSkippedCanBeRedelivered(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	rcv := newReceiver(t, always(200))
	p := h.product("SKR", "")
	evID := h.emit(p.Product.ID)
	h.waitStatus(evID, store.WebhookDeliveryStatusSkipped)
	// The original job is `completed` and still holds its unique key.
	if _, err := h.products.SetWebhookURL(h.ctx, operator, p.Product.ID, rcv.srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDeliveries(h.db.App, h.river).Redeliver(h.ctx, operator, evID); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(evID, store.WebhookDeliveryStatusDelivered)
	if rcv.hits.Load() != 1 {
		t.Fatalf("hits %d", rcv.hits.Load())
	}
}

func TestWorkerHonoursRetryAfter(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	rcv := newReceiver(t, func(int) (int, map[string]string, string) {
		return 429, map[string]string{"Retry-After": "120"}, "slow down"
	})
	p := h.product("RAF", rcv.srv.URL)
	evID := h.emit(p.Product.ID)
	job := &river.Job[queue.WebhookArgs]{
		JobRow: &rivertype.JobRow{ID: 4242, Attempt: 1, MaxAttempts: 25},
		Args:   queue.WebhookArgs{EventID: evID, ProductID: p.Product.ID},
	}
	if err := h.worker.Work(h.ctx, job); err == nil {
		t.Fatal("429 treated as success")
	}
	next := h.worker.NextRetry(job)
	if wait := time.Until(next); wait < 119*time.Second || wait > 121*time.Second {
		t.Fatalf("next retry in %s, want ~120s", wait)
	}
	// Consumed: the next failure without Retry-After uses the normal backoff.
	if wait := time.Until(h.worker.NextRetry(job)); wait > time.Second {
		t.Fatalf("stale Retry-After reused: %s", wait)
	}
	if d := h.delivery(evID); d.Status != store.WebhookDeliveryStatusPending || d.Attempts != 1 || *d.LastStatusCode != 429 {
		t.Fatalf("row: %+v", d)
	}
}

func TestWorkerBlocksPrivateDestinationsInProduction(t *testing.T) {
	h := newHarness(t, harnessOpts{noStart: true})
	hc, err := NewHTTPClient(ClientOptions{Env: config.EnvProduction, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(Options{Pool: h.db.App, Secrets: h.products, Logger: h.logger, Client: hc, Env: config.EnvDevelopment})
	if err != nil {
		t.Fatal(err)
	}
	rcv := newReceiver(t, always(200))
	p := h.product("SSR", rcv.srv.URL+"/x?k=receiver-token-999")
	evID := h.emit(p.Product.ID)
	err = w.Work(h.ctx, &river.Job[queue.WebhookArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: 1, MaxAttempts: 25},
		Args:   queue.WebhookArgs{EventID: evID, ProductID: p.Product.ID},
	})
	if err == nil || rcv.hits.Load() != 0 {
		t.Fatalf("loopback reached: err=%v hits=%d", err, rcv.hits.Load())
	}
	at := h.attempts(evID)
	if len(at) != 1 || at[0].StatusCode != nil || !strings.HasPrefix(deref(at[0].Error), "destination address not allowed") {
		t.Fatalf("attempt: %+v %s", at, deref(at[0].Error))
	}
	h.assertNoSecrets(p.WebhookSecret, "receiver-token-999")

	// And a production worker refuses plain http:// outright.
	wp, err := NewWorker(Options{Pool: h.db.App, Secrets: h.products, Logger: h.logger, Client: hc, Env: config.EnvProduction})
	if err != nil {
		t.Fatal(err)
	}
	ev2 := h.emit(p.Product.ID)
	err = wp.Work(h.ctx, &river.Job[queue.WebhookArgs]{
		JobRow: &rivertype.JobRow{ID: 2, Attempt: 1, MaxAttempts: 25},
		Args:   queue.WebhookArgs{EventID: ev2, ProductID: p.Product.ID},
	})
	if err == nil || h.delivery(ev2).Status != store.WebhookDeliveryStatusFailed {
		t.Fatalf("http URL in production: %v %+v", err, h.delivery(ev2))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 15s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// syncBuffer collects log output from concurrent goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
