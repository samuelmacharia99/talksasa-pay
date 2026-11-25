package appapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/appapi"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var operator = audit.CLI("appapi-test")

const fullPhone = "254712345678"

type env struct {
	t      *testing.T
	ctx    context.Context
	db     *testdb.DB
	logs   *syncBuffer
	apps   *app.Service
	events *events.Emitter
	h      http.Handler

	mu      sync.Mutex
	bodies  []string // every response body, for leak checks
	secrets []string // strings that must never appear in a response
	remote  string
}

type tenant struct {
	ID     uuid.UUID
	Prefix string
	Key    string
	KeyID  string
}

func newEnv(t *testing.T, limiter *httpx.RateLimiter, opts ...func(*appapi.Deps)) *env {
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
	view := apiview.View{PayBaseURL: "https://pay.test/", Paybill: "174379"}
	ev := events.NewEmitter(view, nil)
	e := &env{
		t: t, ctx: context.Background(), db: d, logs: logs,
		apps: app.NewService(app.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("q", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		events: ev,
		remote: "198.51.100.7:40000",
	}
	root := chi.NewRouter()
	root.Use(httpx.RequestID, httpx.ClientIP(nil), httpx.Recover(logger))
	deps := appapi.Deps{
		Logger: logger, Pool: d.App, Apps: e.apps, View: view, KeyLimiter: limiter,
	}
	for _, o := range opts {
		o(&deps)
	}
	root.Mount("/v1", appapi.NewRouter(deps))
	e.h = root
	e.secrets = append(e.secrets, fullPhone, "0712345678", `"hosted_token"`, `"idempotency_key"`, "IdempotencyKey")
	return e
}

func (e *env) tenant(prefix string) tenant {
	e.t.Helper()
	c, err := e.apps.Create(e.ctx, operator, app.CreateParams{
		Name: prefix + " app", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	k, err := e.apps.IssueAPIKey(e.ctx, operator, c.App.ID, "test", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.secrets = append(e.secrets, k.Plaintext, k.Plaintext[strings.LastIndexByte(k.Plaintext, '_')+1:])
	return tenant{ID: c.App.ID, Prefix: prefix, Key: k.Plaintext, KeyID: k.Key.KeyID}
}

type resp struct {
	*httptest.ResponseRecorder
	t *testing.T
}

func (r resp) json() map[string]any {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &m); err != nil {
		r.t.Fatalf("decode %q: %v", r.Body.String(), err)
	}
	return m
}

func (r resp) expect(status int, code string) map[string]any {
	r.t.Helper()
	if r.Code != status {
		r.t.Fatalf("status %d, want %d: %s", r.Code, status, r.Body.String())
	}
	m := r.json()
	if code != "" {
		if m["code"] != code {
			r.t.Fatalf("code %v, want %s: %s", m["code"], code, r.Body.String())
		}
		if ct := r.Header().Get("Content-Type"); ct != "application/problem+json" {
			r.t.Fatalf("problem content type %q", ct)
		}
	}
	return m
}

func (r resp) fieldErrors() []string {
	var out []string
	if errs, ok := r.json()["errors"].([]any); ok {
		for _, fe := range errs {
			out = append(out, fe.(map[string]any)["field"].(string))
		}
	}
	return out
}

type hdr map[string]string

func (e *env) do(method, path, key, body string, h hdr) resp {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = e.remote
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range h {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	e.mu.Lock()
	e.bodies = append(e.bodies, w.Body.String())
	e.mu.Unlock()
	return resp{w, e.t}
}

// charge seeds a pending charge (the charge service is not under test here).
func (e *env) charge(tn tenant, amount int64) store.Charge {
	e.t.Helper()
	c := seed.Charge(e.t, e.ctx, e.db.App, tn.ID, tn.Prefix, amount, seed.ChargeOpts{})
	e.secrets = append(e.secrets, c.IdempotencyKey)
	return c
}

// payCharge records a paybill payment that pays c (no events).
func (e *env) payCharge(c store.Charge) store.Transaction {
	e.t.Helper()
	txn := seed.PayCharge(e.t, e.ctx, e.db.App, c)
	time.Sleep(2 * time.Millisecond)
	return txn
}

// payment records a paybill payment routed to tn without a charge.
func (e *env) payment(tn tenant, account string, amount int64) store.Transaction {
	e.t.Helper()
	txn := seed.Payment(e.t, e.ctx, e.db.App, &tn.ID, account, amount)
	time.Sleep(2 * time.Millisecond)
	return txn
}

// emit writes a charge event through the outbox.
func (e *env) emit(tn tenant, chargeID uuid.UUID, typ string) {
	e.t.Helper()
	err := db.WithTx(e.ctx, e.db.App, "system:test", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		return e.events.EmitCharge(ctx, q, tn.ID, chargeID, typ)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// assertNoLeaks checks every response body seen by this env.
func (e *env) assertNoLeaks() {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, b := range e.bodies {
		for _, s := range e.secrets {
			if s != "" && strings.Contains(b, s) {
				e.t.Errorf("response leaks %q: %s", s, b)
			}
		}
	}
	for _, s := range e.secrets[5:] { // API keys
		if strings.Contains(e.logs.String(), s) {
			e.t.Errorf("logs contain a secret")
		}
	}
}

func uid(t *testing.T, m map[string]any) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(m["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// ---------------------------------------------------------------------------

func TestAuthentication(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("AUTH")

	e.do("GET", "/v1/whoami", "", "", nil).expect(401, "unauthorized")
	e.do("GET", "/v1/whoami", "tsp_test_nope", "", nil).expect(401, "unauthorized")
	e.do("GET", "/v1/whoami", a.Key+"x", "", nil).expect(401, "unauthorized")
	e.do("GET", "/v1/whoami", "", "", hdr{"Authorization": "Basic " + a.Key}).expect(401, "unauthorized")
	// Unknown paths do not reveal themselves to unauthenticated callers.
	e.do("GET", "/v1/nope", "", "", nil).expect(401, "unauthorized")

	m := e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	if m["app_id"] != a.ID.String() || m["account_prefix"] != "AUTH" || m["key_id"] != a.KeyID || m["app_name"] != "AUTH app" {
		t.Fatalf("whoami: %v", m)
	}
	e.do("GET", "/v1/nope", a.Key, "", nil).expect(404, "not_found")
	e.do("DELETE", "/v1/whoami", a.Key, "", nil).expect(405, "method_not_allowed")

	// Revoked key.
	if err := e.apps.RevokeAPIKey(e.ctx, operator, a.ID, a.KeyID); err != nil {
		t.Fatal(err)
	}
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(401, "unauthorized")
	e.assertNoLeaks()
}

func TestIPAllowList(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("IPAL")
	if _, err := e.apps.SetAllowedCIDRs(e.ctx, operator, a.ID, a.KeyID, []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}); err != nil {
		t.Fatal(err)
	}
	e.remote = "198.51.100.7:1"
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(403, "ip_not_allowed")
	e.remote = "203.0.113.200:1"
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	e.remote = "[::ffff:203.0.113.9]:1" // v4-mapped
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	if _, err := e.apps.SetAllowedCIDRs(e.ctx, operator, a.ID, a.KeyID, nil); err != nil {
		t.Fatal(err)
	}
	e.remote = "198.51.100.7:1"
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
}

func TestRateLimitPerKey(t *testing.T) {
	e := newEnv(t, httpx.NewRateLimiter(1, 2))
	a, b := e.tenant("RLA"), e.tenant("RLB")
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	r := e.do("GET", "/v1/whoami", a.Key, "", nil)
	r.expect(429, "rate_limited")
	if r.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	e.do("GET", "/v1/whoami", b.Key, "", nil).expect(200, "") // other key unaffected
}

func TestCrossAppIsolation(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.tenant("ISA"), e.tenant("ISB")
	ch := e.charge(a, 1000)
	txn := e.payCharge(ch)
	e.emit(a, ch.ID, events.ChargeSucceeded)

	for _, p := range []string{"/v1/transactions/" + txn.ID.String()} {
		e.do("GET", p, b.Key, "", nil).expect(404, "not_found")
		e.do("GET", p, a.Key, "", nil).expect(200, "")
	}
	for _, p := range []string{"/v1/transactions", "/v1/transactions?charge_id=" + ch.ID.String()} {
		m := e.do("GET", p, b.Key, "", nil).expect(200, "")
		if n := len(m["data"].([]any)); n != 0 {
			t.Fatalf("%s: B sees %d of A's rows", p, n)
		}
	}
	m := e.do("GET", "/v1/events", b.Key, "", nil).expect(200, "")
	for _, ev := range m["data"].([]any) {
		if strings.Contains(fmt.Sprint(ev), ch.ID.String()) {
			t.Fatal("B sees A's events")
		}
	}
	// A cursor issued to A is only a position: in B it reveals nothing of A.
	e.settledFeed(a.ID)
	am := e.do("GET", "/v1/events?limit=1", a.Key, "", nil).expect(200, "")
	bm := e.do("GET", "/v1/events?after="+am["next_cursor"].(string), b.Key, "", nil).expect(200, "")
	if strings.Contains(fmt.Sprint(bm), ch.ID.String()) {
		t.Fatal("cursor leaked A's events to B")
	}
	e.assertNoLeaks()
}

func TestTransactions(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("TXN")
	ch := e.charge(a, 1000)
	t1 := e.payCharge(ch)
	t2 := e.payment(a, "TXN-1042", 50) // paybill payment, no charge
	start := time.Now()
	t3 := e.payment(a, "TXN-7", 20)

	m := e.do("GET", "/v1/transactions/"+t1.ID.String(), a.Key, "", nil).expect(200, "")
	if m["object"] != "transaction" || m["amount"].(float64) != 1000 || m["kind"] != "payment" || m["direction"] != "in" ||
		m["channel"] != "mpesa_c2b" || m["status"] != "succeeded" || m["phone"] != "2547*****678" || m["currency"] != "KES" ||
		m["charge_id"] != ch.ID.String() || m["account_number"] != ch.AccountNumber {
		t.Fatalf("txn: %v", m)
	}
	if _, ok := m["allocations"]; ok {
		t.Fatal("allocations are gone from the transaction view")
	}
	e.do("GET", "/v1/transactions/"+uuid.NewString(), a.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/transactions/xyz", a.Key, "", nil).expect(404, "not_found")

	// List with paging.
	var got []string
	cursor := ""
	for {
		p := "/v1/transactions?limit=2"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		m := e.do("GET", p, a.Key, "", nil).expect(200, "")
		for _, d := range m["data"].([]any) {
			got = append(got, d.(map[string]any)["id"].(string))
		}
		nc, _ := m["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	want := []string{t3.ID.String(), t2.ID.String(), t1.ID.String()}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	count := func(q string) int {
		return len(e.do("GET", "/v1/transactions?"+q, a.Key, "", nil).expect(200, "")["data"].([]any))
	}
	if n := count("charge_id=" + ch.ID.String()); n != 1 {
		t.Fatalf("charge filter %d", n)
	}
	if n := count("since=" + start.UTC().Format(time.RFC3339Nano)); n != 1 {
		t.Fatalf("since filter %d", n)
	}
	if n := count("since=2999-01-01T00:00:00Z"); n != 0 {
		t.Fatalf("future since %d", n)
	}
	for _, q := range []string{"since=yesterday", "limit=-1", "cursor=AAAA", "charge_id=nope"} {
		e.do("GET", "/v1/transactions?"+q, a.Key, "", nil).expect(400, "validation_failed")
	}
	e.assertNoLeaks()
}

func TestEventsFeed(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("EVF")
	var ids []uuid.UUID
	for i := 0; i < 8; i++ {
		ch := e.charge(a, int64(100+i))
		ids = append(ids, ch.ID)
		e.emit(a, ch.ID, events.ChargeCancelled)
	}

	all := e.settledFeed(a.ID)
	if len(all) != 8 {
		t.Fatalf("%d events", len(all))
	}

	for _, limit := range []int{1, 3, 200} {
		var got []map[string]any
		after := ""
		for i := 0; ; i++ {
			if i > 100 {
				t.Fatal("feed does not terminate")
			}
			p := fmt.Sprintf("/v1/events?limit=%d", limit)
			if after != "" {
				p += "&after=" + after
			}
			m := e.do("GET", p, a.Key, "", nil).expect(200, "")
			data := m["data"].([]any)
			if len(data) == 0 {
				if m["next_cursor"] != nil || m["has_more"] != false {
					t.Fatalf("empty page: %v", m)
				}
				break
			}
			for _, d := range data {
				got = append(got, d.(map[string]any))
			}
			after = m["next_cursor"].(string)
			if want := len(got) < len(all); m["has_more"] != want {
				t.Fatalf("limit %d: has_more=%v after %d of %d", limit, m["has_more"], len(got), len(all))
			}
		}
		if len(got) != len(all) {
			t.Fatalf("limit %d: %d events, want %d", limit, len(got), len(all))
		}
		for i, ev := range got {
			if ev["id"] != all[i].ID.String() || ev["type"] != all[i].Type || ev["object"] != "event" ||
				ev["api_version"] != apiview.APIVersion || ev["created_at"] == nil {
				t.Fatalf("event %d: %v vs %s %s", i, ev, all[i].ID, all[i].Type)
			}
			data, ok := ev["data"].(map[string]any)
			if !ok || data["object"] != "charge" || data["id"] != ids[i].String() {
				t.Fatalf("event %d data: %v", i, ev["data"])
			}
		}
	}

	// A new event after the stored cursor is delivered next.
	last := e.do("GET", "/v1/events?limit=200", a.Key, "", nil).expect(200, "")["next_cursor"].(string)
	ch := e.charge(a, 10)
	e.emit(a, ch.ID, events.ChargeExpired)
	e.settledFeed(a.ID)
	m := e.do("GET", "/v1/events?after="+last, a.Key, "", nil).expect(200, "")
	data := m["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["type"] != events.ChargeExpired || m["has_more"] != false {
		t.Fatalf("incremental: %v", m)
	}

	txnCursor := e.do("GET", "/v1/transactions?limit=1", a.Key, "", nil).expect(200, "")["next_cursor"]
	bad := []string{"after=bad", "limit=500", "after=" + strings.Repeat("A", 300)}
	if c, ok := txnCursor.(string); ok {
		bad = append(bad, "after="+c)
	}
	for _, q := range bad {
		e.do("GET", "/v1/events?"+q, a.Key, "", nil).expect(400, "validation_failed")
	}
	e.assertNoLeaks()
}

// settledFeed waits until every committed event of the app is visible in
// the gap-free feed and returns the feed. The feed hides events newer than
// the oldest running transaction, and transaction ids are cluster-wide, so
// tests running in parallel (other databases on the same server) can delay
// visibility briefly.
func (e *env) settledFeed(appID uuid.UUID) []store.Event {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var total int
		if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE app_id = $1`, appID).Scan(&total); err != nil {
			e.t.Fatal(err)
		}
		feed, err := store.New(e.db.App).ListEventsFeed(e.ctx, store.ListEventsFeedParams{AppID: appID, Lim: 10_000})
		if err != nil {
			e.t.Fatal(err)
		}
		if len(feed) == total {
			return feed
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("feed shows %d of %d events", len(feed), total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------

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
