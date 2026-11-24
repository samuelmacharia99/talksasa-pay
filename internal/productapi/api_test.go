package productapi_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/productapi"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var operator = audit.CLI("productapi-test")

const fullPhone = "254712345678"

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *testdb.DB
	logs     *syncBuffer
	products *product.Service
	payers   *payer.Service
	requests *paymentrequest.Service
	ledger   *ledger.Service
	h        http.Handler

	mu        sync.Mutex
	bodies    []string // every response body, for leak checks
	secrets   []string // strings that must never appear in a response
	remote    string
	receiptNo atomic.Int64
}

type tenant struct {
	ID    uuid.UUID
	Key   string
	KeyID string
}

func newEnv(t *testing.T, limiter *httpx.RateLimiter, opts ...func(*productapi.Deps)) *env {
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
	alloc := allocation.NewService(logger, ev)
	e := &env{
		t: t, ctx: context.Background(), db: d, logs: logs,
		products: product.NewService(product.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("q", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		payers:   payer.NewService(d.App, logger),
		requests: paymentrequest.NewService(d.App, alloc, ev, logger),
		ledger:   ledger.NewService(d.App, alloc, logger),
		remote:   "198.51.100.7:40000",
	}
	root := chi.NewRouter()
	root.Use(httpx.RequestID, httpx.ClientIP(nil), httpx.Recover(logger))
	deps := productapi.Deps{
		Logger: logger, Pool: d.App, Products: e.products, Payers: e.payers, Requests: e.requests,
		View: view, KeyLimiter: limiter,
	}
	for _, o := range opts {
		o(&deps)
	}
	root.Mount("/v1", productapi.NewRouter(deps))
	e.h = root
	e.secrets = append(e.secrets, fullPhone, "0712345678", `"hosted_token"`, `"idempotency_key"`, "IdempotencyKey")
	return e
}

func (e *env) tenant(prefix string) tenant {
	e.t.Helper()
	c, err := e.products.Create(e.ctx, operator, product.CreateParams{
		Name: prefix + " product", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	k, err := e.products.IssueAPIKey(e.ctx, operator, c.Product.ID, "test", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.secrets = append(e.secrets, k.Plaintext, k.Plaintext[strings.LastIndexByte(k.Plaintext, '_')+1:])
	return tenant{ID: c.Product.ID, Key: k.Plaintext, KeyID: k.Key.KeyID}
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

func (e *env) putPayer(tn tenant, ref string) map[string]any {
	e.t.Helper()
	r := e.do("PUT", "/v1/payers/"+ref, tn.Key, `{"display_name":"Customer `+ref+`","email":"c@example.com","phones":["0712345678"]}`, nil)
	if r.Code != 201 && r.Code != 200 {
		e.t.Fatalf("put payer: %d %s", r.Code, r.Body.String())
	}
	return r.json()
}

var seq atomic.Int64

func (e *env) createRequest(tn tenant, payerRef string, amount int64) map[string]any {
	e.t.Helper()
	n := seq.Add(1)
	idem := fmt.Sprintf("idem-secret-%d", n)
	e.secrets = append(e.secrets, idem)
	r := e.do("POST", "/v1/payment-requests", tn.Key,
		fmt.Sprintf(`{"payer":%q,"reference":"INV-%d","description":"Invoice %d","amount":%d}`, payerRef, n, n, amount),
		hdr{"Idempotency-Key": idem})
	m := r.expect(201, "")
	time.Sleep(2 * time.Millisecond) // distinct created_at
	return m
}

func (e *env) pay(tn tenant, payerRef string, amount int64, target *uuid.UUID) ledger.Transaction {
	e.t.Helper()
	pa, err := store.New(e.db.App).GetPayerByExternalRef(e.ctx, store.GetPayerByExternalRefParams{ProductID: tn.ID, ExternalRef: payerRef})
	if err != nil {
		e.t.Fatal(err)
	}
	receipt := fmt.Sprintf("PAPI%06d", e.receiptNo.Add(1))
	ch := store.TxnChannelMpesaC2b
	if target != nil {
		ch = store.TxnChannelMpesaStk
	}
	txn, _, err := e.ledger.RecordAndAllocate(e.ctx, audit.System("test-worker"), ledger.IncomingPayment{
		Channel: ch, Receipt: receipt, Amount: money.KES(amount), Phone: fullPhone, BillRef: pa.AccountNumber,
		ProductID: &tn.ID, PayerAccountID: &pa.ID, PaymentRequestID: target, RawPayload: []byte(`{"TransID":"` + receipt + `"}`),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	return txn
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
	if m["product_id"] != a.ID.String() || m["account_prefix"] != "AUTH" || m["key_id"] != a.KeyID || m["product_name"] != "AUTH product" {
		t.Fatalf("whoami: %v", m)
	}
	e.do("GET", "/v1/nope", a.Key, "", nil).expect(404, "not_found")
	e.do("DELETE", "/v1/whoami", a.Key, "", nil).expect(405, "method_not_allowed")

	// Revoked key.
	if err := e.products.RevokeAPIKey(e.ctx, operator, a.ID, a.KeyID); err != nil {
		t.Fatal(err)
	}
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(401, "unauthorized")
	e.assertNoLeaks()
}

func TestIPAllowList(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("IPAL")
	if _, err := e.products.SetAllowedCIDRs(e.ctx, operator, a.ID, a.KeyID, []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}); err != nil {
		t.Fatal(err)
	}
	e.remote = "198.51.100.7:1"
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(403, "ip_not_allowed")
	e.remote = "203.0.113.200:1"
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	e.remote = "[::ffff:203.0.113.9]:1" // v4-mapped
	e.do("GET", "/v1/whoami", a.Key, "", nil).expect(200, "")
	if _, err := e.products.SetAllowedCIDRs(e.ctx, operator, a.ID, a.KeyID, nil); err != nil {
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

func TestPayers(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("PAY")

	body := `{"display_name":"Jane Doe","email":"Jane@Example.com","phones":["0712345678","+254 722 000 111"]}`
	r := e.do("PUT", "/v1/payers/cust-1", a.Key, body, nil)
	m := r.expect(201, "")
	if m["object"] != "payer" || m["external_ref"] != "cust-1" || !strings.HasPrefix(m["account_number"].(string), "PAY-") ||
		m["status"] != "active" || m["credit_balance"].(float64) != 0 || m["email"] != "jane@example.com" {
		t.Fatalf("payer: %v", m)
	}
	phones := m["phones"].([]any)
	if len(phones) != 2 || phones[0] != "2547*****678" {
		t.Fatalf("phones not masked/ordered: %v", phones)
	}
	acct := m["account_number"]

	// Same body: 200, same account number.
	m = e.do("PUT", "/v1/payers/cust-1", a.Key, body, nil).expect(200, "")
	if m["account_number"] != acct {
		t.Fatal("account number changed")
	}
	// Update replaces name, clears email and phones.
	m = e.do("PUT", "/v1/payers/cust-1", a.Key, `{"display_name":"Jane D"}`, nil).expect(200, "")
	if m["display_name"] != "Jane D" || m["email"] != nil || len(m["phones"].([]any)) != 0 {
		t.Fatalf("update: %v", m)
	}
	m = e.do("GET", "/v1/payers/cust-1", a.Key, "", nil).expect(200, "")
	if m["display_name"] != "Jane D" || m["account_number"] != acct {
		t.Fatalf("get: %v", m)
	}
	e.do("GET", "/v1/payers/nobody", a.Key, "", nil).expect(404, "not_found")

	// URL-encoded refs round-trip exactly.
	for path, want := range map[string]string{"/v1/payers/a%2Fb": "a/b", "/v1/payers/50%25": "50%", "/v1/payers/x%20y": "x y"} {
		m = e.do("PUT", path, a.Key, `{"display_name":"X"}`, nil).expect(201, "")
		if m["external_ref"] != want {
			t.Fatalf("%s: external_ref %v want %q", path, m["external_ref"], want)
		}
		e.do("GET", path, a.Key, "", nil).expect(200, "")
	}

	// Validation.
	long := strings.Repeat("x", 129)
	e.do("PUT", "/v1/payers/"+long, a.Key, `{"display_name":"X"}`, nil).expect(422, "validation_failed")
	e.do("PUT", "/v1/payers/%20lead", a.Key, `{"display_name":"X"}`, nil).expect(422, "validation_failed")
	e.do("PUT", "/v1/payers/a%00b", a.Key, `{"display_name":"X"}`, nil).expect(422, "validation_failed")
	cases := []struct {
		body, field string
	}{
		{`{}`, "display_name"},
		{`{"display_name":"  "}`, "display_name"},
		{`{"display_name":"` + strings.Repeat("n", 201) + `"}`, "display_name"},
		{`{"display_name":5}`, "display_name"},
		{`{"display_name":"X","email":"not-an-email"}`, "email"},
		{`{"display_name":"X","phones":["12345"]}`, "phones"},
		{`{"display_name":"X","phones":["0712345678","0712345679","0712345670","0712345671","0712345672","0712345673"]}`, "phones"},
		{`{"display_name":"X","phones":"0712345678"}`, "phones"},
		{`{"display_name":"X","account_number":"PAY-1"}`, "account_number"},
		{`{"display_name":"X","credit_balance":100}`, "credit_balance"},
	}
	for _, c := range cases {
		r := e.do("PUT", "/v1/payers/cust-v", a.Key, c.body, nil)
		r.expect(422, "validation_failed")
		if f := r.fieldErrors(); len(f) == 0 || f[0] != c.field {
			t.Errorf("%s: fields %v, want %s", c.body, f, c.field)
		}
	}
	e.do("PUT", "/v1/payers/cust-v", a.Key, `{"display_name":"X"`, nil).expect(400, "invalid_json")
	e.do("PUT", "/v1/payers/cust-v", a.Key, `{"display_name":"X"} {}`, nil).expect(400, "invalid_json")
	e.do("PUT", "/v1/payers/cust-v", a.Key, `[]`, nil).expect(400, "invalid_json")
	e.do("PUT", "/v1/payers/cust-v", a.Key, `{"display_name":"X"}`, hdr{"Content-Type": "text/plain"}).expect(415, "unsupported_media_type")
	e.do("PUT", "/v1/payers/cust-v", a.Key, `{"display_name":"X"}`, hdr{"Content-Type": ""}).expect(415, "unsupported_media_type")
	e.do("PUT", "/v1/payers/cust-v", a.Key, `{"display_name":"X"}`, hdr{"Content-Type": "application/json; charset=utf-8"}).expect(201, "")
	big := `{"display_name":"X","email":"` + strings.Repeat("a", 70_000) + `"}`
	e.do("PUT", "/v1/payers/cust-big", a.Key, big, nil).expect(413, "payload_too_large")
	e.assertNoLeaks()
}

func TestCreatePaymentRequest(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("PRQ")
	e.putPayer(a, "cust-1")
	body := `{"payer":"cust-1","reference":"INV-1","description":"Hosting renewal","amount":1500,"expires_at":"` +
		time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339) + `"}`

	e.do("POST", "/v1/payment-requests", a.Key, body, nil).expect(400, "idempotency_key_required")
	e.do("POST", "/v1/payment-requests", a.Key, body, hdr{"Idempotency-Key": "   "}).expect(400, "idempotency_key_required")
	e.do("POST", "/v1/payment-requests", a.Key, body, hdr{"Idempotency-Key": strings.Repeat("k", 256)}).expect(400, "validation_failed")

	e.secrets = append(e.secrets, "key-secret-1")
	r := e.do("POST", "/v1/payment-requests", a.Key, body, hdr{"Idempotency-Key": "key-secret-1"})
	m := r.expect(201, "")
	if r.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("replay header on first create")
	}
	if m["object"] != "payment_request" || m["status"] != "open" || m["amount"].(float64) != 1500 || m["outstanding"].(float64) != 1500 ||
		m["amount_paid"].(float64) != 0 || m["currency"] != "KES" || m["reference"] != "INV-1" ||
		!strings.HasPrefix(m["pay_url"].(string), "https://pay.test/") || m["expires_at"] == nil {
		t.Fatalf("created: %v", m)
	}
	pb := m["paybill"].(map[string]any)
	payerRef := m["payer"].(map[string]any)
	if pb["business_number"] != "174379" || pb["account_number"] != payerRef["account_number"] || payerRef["external_ref"] != "cust-1" {
		t.Fatalf("paybill/payer: %v", m)
	}
	id := m["id"]

	// Replay: 200 + header + same request.
	r = e.do("POST", "/v1/payment-requests", a.Key, body, hdr{"Idempotency-Key": "key-secret-1"})
	m = r.expect(200, "")
	if r.Header().Get("Idempotent-Replayed") != "true" || m["id"] != id {
		t.Fatalf("replay: %v %v", r.Header(), m)
	}
	// Same key, different body: 409.
	diff := strings.Replace(body, `"amount":1500`, `"amount":1600`, 1)
	e.do("POST", "/v1/payment-requests", a.Key, diff, hdr{"Idempotency-Key": "key-secret-1"}).expect(409, "idempotency_conflict")
	// Same live reference under a new key: 409.
	e.do("POST", "/v1/payment-requests", a.Key, body, hdr{"Idempotency-Key": "key-2"}).expect(409, "duplicate_reference")

	// Validation.
	valid := func(over string) string {
		base := map[string]any{"payer": "cust-1", "reference": "INV-V", "description": "d", "amount": 10}
		var o map[string]any
		_ = json.Unmarshal([]byte(over), &o)
		for k, v := range o {
			if v == nil {
				delete(base, k)
			} else {
				base[k] = v
			}
		}
		b, _ := json.Marshal(base)
		return string(b)
	}
	cases := []struct {
		body, field string
	}{
		{valid(`{"payer":null}`), "payer"},
		{valid(`{"reference":null}`), "reference"},
		{valid(`{"description":null}`), "description"},
		{valid(`{"amount":null}`), "amount"},
		{valid(`{"amount":0}`), "amount"},
		{valid(`{"amount":-5}`), "amount"},
		{valid(`{"amount":1000000001}`), "amount"},
		{valid(`{"amount":12.5}`), "amount"},
		{valid(`{"amount":"100"}`), "amount"},
		{valid(`{"reference":""}`), "reference"},
		{valid(`{"reference":"` + strings.Repeat("r", 129) + `"}`), "reference"},
		{valid(`{"description":"` + strings.Repeat("d", 501) + `"}`), "description"},
		{valid(`{"expires_at":"tomorrow"}`), "expires_at"},
		{valid(`{"expires_at":"2001-01-01T00:00:00Z"}`), "expires_at"},
		{valid(`{"expires_at":"` + time.Now().Add(400*24*time.Hour).UTC().Format(time.RFC3339) + `"}`), "expires_at"},
		{valid(`{"payer":"nobody"}`), "payer"},
		{valid(`{"amount_paid":10}`), "amount_paid"},
		{valid(`{"currency":"USD"}`), "currency"},
	}
	for i, c := range cases {
		r := e.do("POST", "/v1/payment-requests", a.Key, c.body, hdr{"Idempotency-Key": fmt.Sprintf("v-%d", i)})
		r.expect(422, "validation_failed")
		if f := r.fieldErrors(); len(f) == 0 || f[0] != c.field {
			t.Errorf("%s: fields %v, want %s", c.body, f, c.field)
		}
	}
	e.do("POST", "/v1/payment-requests", a.Key, valid(`{}`), hdr{"Idempotency-Key": "ct", "Content-Type": "application/x-www-form-urlencoded"}).
		expect(415, "unsupported_media_type")

	// Max amount is allowed.
	e.do("POST", "/v1/payment-requests", a.Key, valid(`{"reference":"BIG","amount":1000000000}`), hdr{"Idempotency-Key": "max"}).expect(201, "")

	// Closed payer: 409.
	e.putPayer(a, "cust-closed")
	if _, err := e.db.Owner.Exec(e.ctx, `UPDATE payer_accounts SET status = 'closed' WHERE product_id = $1 AND external_ref = 'cust-closed'`, a.ID); err != nil {
		t.Fatal(err)
	}
	e.do("POST", "/v1/payment-requests", a.Key, valid(`{"payer":"cust-closed","reference":"C"}`), hdr{"Idempotency-Key": "closed"}).
		expect(409, "payer_closed")

	// Credit is applied immediately.
	e.putPayer(a, "cust-credit")
	e.pay(a, "cust-credit", 700, nil) // no open request: all credit
	m = e.do("POST", "/v1/payment-requests", a.Key, valid(`{"payer":"cust-credit","reference":"CR","amount":500}`), hdr{"Idempotency-Key": "cr"}).expect(201, "")
	if m["status"] != "paid" || m["amount_paid"].(float64) != 500 || m["paybill"] != nil || m["paid_at"] == nil {
		t.Fatalf("credit applied: %v", m)
	}
	pm := e.do("GET", "/v1/payers/cust-credit", a.Key, "", nil).expect(200, "")
	if pm["credit_balance"].(float64) != 200 {
		t.Fatalf("credit balance: %v", pm)
	}
	e.assertNoLeaks()
}

func TestConcurrentIdempotentCreate(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("CONC")
	e.putPayer(a, "c")
	body := `{"payer":"c","reference":"R1","description":"d","amount":100}`
	var wg sync.WaitGroup
	ids := make([]string, 8)
	codes := make([]int, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/v1/payment-requests", strings.NewReader(body))
			req.RemoteAddr = e.remote
			req.Header.Set("Authorization", "Bearer "+a.Key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "same")
			w := httptest.NewRecorder()
			e.h.ServeHTTP(w, req)
			codes[i] = w.Code
			var m map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &m)
			ids[i], _ = m["id"].(string)
		}(i)
	}
	wg.Wait()
	created := 0
	for i := range ids {
		if codes[i] == 201 {
			created++
		} else if codes[i] != 200 {
			t.Fatalf("code %d", codes[i])
		}
		if ids[i] != ids[0] || ids[i] == "" {
			t.Fatalf("different ids: %v", ids)
		}
	}
	if created != 1 {
		t.Fatalf("%d creates", created)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("CAN")
	e.putPayer(a, "c")
	open := e.createRequest(a, "c", 1000)
	id := open["id"].(string)

	m := e.do("POST", "/v1/payment-requests/"+id+"/cancel", a.Key, "", nil).expect(200, "")
	if m["status"] != "cancelled" || m["closed_at"] == nil || m["paybill"] != nil {
		t.Fatalf("cancel: %v", m)
	}
	m = e.do("POST", "/v1/payment-requests/"+id+"/cancel", a.Key, "", nil).expect(200, "") // idempotent
	if m["status"] != "cancelled" {
		t.Fatalf("second cancel: %v", m)
	}

	part := e.createRequest(a, "c", 1000)
	pid := uid(t, part)
	e.pay(a, "c", 100, &pid)
	e.do("POST", "/v1/payment-requests/"+pid.String()+"/cancel", a.Key, "", nil).expect(409, "not_cancellable")
	m = e.do("GET", "/v1/payment-requests/"+pid.String(), a.Key, "", nil).expect(200, "")
	if m["status"] != "partially_paid" || m["outstanding"].(float64) != 900 {
		t.Fatalf("partial: %v", m)
	}
	e.do("POST", "/v1/payment-requests/"+uuid.NewString()+"/cancel", a.Key, "", nil).expect(404, "not_found")
	e.do("POST", "/v1/payment-requests/not-a-uuid/cancel", a.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/payment-requests/not-a-uuid", a.Key, "", nil).expect(404, "not_found")
	e.assertNoLeaks()
}

func TestCrossProductIsolation(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.tenant("ISA"), e.tenant("ISB")
	e.putPayer(a, "shared-ref")
	req := e.createRequest(a, "shared-ref", 1000)
	rid := uid(t, req)
	txn := e.pay(a, "shared-ref", 400, &rid)

	for _, p := range []string{
		"/v1/payment-requests/" + rid.String(),
		"/v1/transactions/" + txn.ID.String(),
		"/v1/payers/shared-ref",
	} {
		e.do("GET", p, b.Key, "", nil).expect(404, "not_found")
		e.do("GET", p, a.Key, "", nil).expect(200, "")
	}
	e.do("POST", "/v1/payment-requests/"+rid.String()+"/cancel", b.Key, "", nil).expect(404, "not_found")

	// B cannot bill A's payer by external_ref: it is B's namespace.
	e.do("POST", "/v1/payment-requests", b.Key, `{"payer":"shared-ref","reference":"X","description":"d","amount":5}`,
		hdr{"Idempotency-Key": "x"}).expect(422, "validation_failed")
	// Same external_ref in B is a different payer.
	pb := e.putPayer(b, "shared-ref")
	pa := e.do("GET", "/v1/payers/shared-ref", a.Key, "", nil).expect(200, "")
	if pb["id"] == pa["id"] || pb["account_number"] == pa["account_number"] {
		t.Fatal("payers shared across products")
	}

	for _, p := range []string{"/v1/payment-requests", "/v1/transactions", "/v1/payment-requests?payer=shared-ref", "/v1/transactions?payer=shared-ref"} {
		m := e.do("GET", p, b.Key, "", nil).expect(200, "")
		if n := len(m["data"].([]any)); n != 0 {
			t.Fatalf("%s: B sees %d of A's rows", p, n)
		}
	}
	m := e.do("GET", "/v1/events", b.Key, "", nil).expect(200, "")
	for _, ev := range m["data"].([]any) {
		if strings.Contains(fmt.Sprint(ev), rid.String()) {
			t.Fatal("B sees A's events")
		}
	}
	// A cursor issued to A is only a position: in B it reveals nothing of A.
	e.settledFeed(a.ID)
	am := e.do("GET", "/v1/events?limit=1", a.Key, "", nil).expect(200, "")
	bm := e.do("GET", "/v1/events?after="+am["next_cursor"].(string), b.Key, "", nil).expect(200, "")
	if strings.Contains(fmt.Sprint(bm), rid.String()) {
		t.Fatal("cursor leaked A's events to B")
	}
	e.assertNoLeaks()
}

func TestListPaymentRequests(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("LST")
	e.putPayer(a, "p1")
	e.putPayer(a, "p2")
	var want []string // newest first
	for i := 0; i < 7; i++ {
		ref := "p1"
		if i%3 == 0 {
			ref = "p2"
		}
		want = append([]string{e.createRequest(a, ref, int64(100+i))["id"].(string)}, want...)
	}
	cancelled := want[0]
	e.do("POST", "/v1/payment-requests/"+cancelled+"/cancel", a.Key, "", nil).expect(200, "")

	var got []string
	cursor := ""
	pages := 0
	for {
		p := "/v1/payment-requests?limit=3"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		m := e.do("GET", p, a.Key, "", nil).expect(200, "")
		pages++
		for _, d := range m["data"].([]any) {
			got = append(got, d.(map[string]any)["id"].(string))
		}
		nc, _ := m["next_cursor"].(string)
		if nc == "" {
			if m["next_cursor"] != nil {
				t.Fatal("next_cursor must be null")
			}
			break
		}
		cursor = nc
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || pages != 3 {
		t.Fatalf("pages=%d\n got %v\nwant %v", pages, got, want)
	}
	// Default limit returns all 7 with null cursor.
	m := e.do("GET", "/v1/payment-requests", a.Key, "", nil).expect(200, "")
	if len(m["data"].([]any)) != 7 || m["next_cursor"] != nil {
		t.Fatalf("default list: %v", m)
	}
	count := func(q string) int {
		return len(e.do("GET", "/v1/payment-requests?"+q, a.Key, "", nil).expect(200, "")["data"].([]any))
	}
	if n := count("status=cancelled"); n != 1 {
		t.Fatalf("status filter %d", n)
	}
	if n := count("status=open"); n != 6 {
		t.Fatalf("open filter %d", n)
	}
	if n := count("payer=p2"); n != 3 {
		t.Fatalf("payer filter %d", n)
	}
	if n := count("payer=unknown"); n != 0 {
		t.Fatalf("unknown payer %d", n)
	}
	one := e.do("GET", "/v1/payment-requests/"+want[3], a.Key, "", nil).expect(200, "")
	if n := count("reference=" + one["reference"].(string)); n != 1 {
		t.Fatalf("reference filter %d", n)
	}

	e.settledFeed(a.ID)
	txCursor := e.do("GET", "/v1/events?limit=1", a.Key, "", nil).expect(200, "")["next_cursor"].(string)
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "status=bogus", "cursor=garbage", "cursor=" + txCursor,
		"payer=" + strings.Repeat("p", 129), "reference=" + strings.Repeat("r", 129)} {
		e.do("GET", "/v1/payment-requests?"+q, a.Key, "", nil).expect(400, "validation_failed")
	}
	e.assertNoLeaks()
}

func TestTransactions(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("TXN")
	e.putPayer(a, "p1")
	e.putPayer(a, "p2")
	r1 := e.createRequest(a, "p1", 1000)
	r1id := uid(t, r1)
	t1 := e.pay(a, "p1", 1300, &r1id) // 1000 to r1, 300 credit
	t2 := e.pay(a, "p2", 50, nil)     // credit only
	start := time.Now()
	t3 := e.pay(a, "p1", 20, nil)

	m := e.do("GET", "/v1/transactions/"+t1.ID.String(), a.Key, "", nil).expect(200, "")
	if m["object"] != "transaction" || m["amount"].(float64) != 1300 || m["kind"] != "payment" || m["direction"] != "in" ||
		m["channel"] != "mpesa_stk" || m["status"] != "succeeded" || m["phone"] != "2547*****678" || m["currency"] != "KES" {
		t.Fatalf("txn: %v", m)
	}
	allocs := m["allocations"].([]any)
	if len(allocs) != 2 {
		t.Fatalf("allocations: %v", allocs)
	}
	var toReq, toCredit float64
	for _, al := range allocs {
		al := al.(map[string]any)
		if al["payment_request_id"] == r1id.String() {
			toReq += al["amount"].(float64)
		} else if al["payment_request_id"] == nil {
			toCredit += al["amount"].(float64)
		}
	}
	if toReq != 1000 || toCredit != 300 {
		t.Fatalf("allocation split %v/%v: %v", toReq, toCredit, allocs)
	}
	e.do("GET", "/v1/transactions/"+uuid.NewString(), a.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/transactions/xyz", a.Key, "", nil).expect(404, "not_found")

	// List with paging; allocations included for every row.
	var got []string
	cursor := ""
	for {
		p := "/v1/transactions?limit=2"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		m := e.do("GET", p, a.Key, "", nil).expect(200, "")
		for _, d := range m["data"].([]any) {
			d := d.(map[string]any)
			got = append(got, d["id"].(string))
			if len(d["allocations"].([]any)) == 0 {
				t.Fatalf("list row without allocations: %v", d)
			}
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
	if n := count("payer=p2"); n != 1 {
		t.Fatalf("payer filter %d", n)
	}
	if n := count("since=" + start.UTC().Format(time.RFC3339Nano)); n != 1 {
		t.Fatalf("since filter %d", n)
	}
	if n := count("since=2999-01-01T00:00:00Z"); n != 0 {
		t.Fatalf("future since %d", n)
	}
	for _, q := range []string{"since=yesterday", "limit=-1", "cursor=AAAA"} {
		e.do("GET", "/v1/transactions?"+q, a.Key, "", nil).expect(400, "validation_failed")
	}
	e.assertNoLeaks()
}

func TestEventsFeed(t *testing.T) {
	e := newEnv(t, nil)
	a := e.tenant("EVF")
	e.putPayer(a, "p1")
	r := e.createRequest(a, "p1", 1000)
	rid := uid(t, r)
	e.pay(a, "p1", 400, &rid)
	e.pay(a, "p1", 900, &rid) // paid + credit
	e.putPayer(a, "p2")       // no credit, so its requests stay open
	r2 := e.createRequest(a, "p2", 5000)
	e.do("POST", "/v1/payment-requests/"+r2["id"].(string)+"/cancel", a.Key, "", nil).expect(200, "")

	all := e.settledFeed(a.ID)
	if len(all) < 8 {
		t.Fatalf("only %d events", len(all))
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
			if !ok || len(data) == 0 {
				t.Fatalf("event %d data: %v", i, ev["data"])
			}
			if strings.HasPrefix(all[i].Type, "payment_request.") && data["object"] != "payment_request" {
				t.Fatalf("event %d data object: %v", i, data)
			}
		}
		if got[0]["type"] != events.PaymentRequestCreated {
			t.Fatalf("first event %v", got[0]["type"])
		}
	}

	// A new event after the stored cursor is delivered next.
	last := e.do("GET", "/v1/events?limit=200", a.Key, "", nil).expect(200, "")["next_cursor"].(string)
	e.do("POST", "/v1/payment-requests/"+uid(t, e.createRequest(a, "p2", 10)).String()+"/cancel", a.Key, "", nil).expect(200, "")
	e.settledFeed(a.ID)
	m := e.do("GET", "/v1/events?after="+last, a.Key, "", nil).expect(200, "")
	data := m["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["type"] != events.PaymentRequestCreated ||
		data[1].(map[string]any)["type"] != events.PaymentRequestCancelled || m["has_more"] != false {
		t.Fatalf("incremental: %v", m)
	}

	reqCursor := e.do("GET", "/v1/payment-requests?limit=1", a.Key, "", nil).expect(200, "")["next_cursor"].(string)
	for _, q := range []string{"after=bad", "after=" + reqCursor, "limit=500", "after=" + strings.Repeat("A", 300)} {
		e.do("GET", "/v1/events?"+q, a.Key, "", nil).expect(400, "validation_failed")
	}
	e.assertNoLeaks()
}

// settledFeed waits until every committed event of the product is visible in
// the gap-free feed and returns the feed. The feed hides events newer than
// the oldest running transaction, and transaction ids are cluster-wide, so
// tests running in parallel (other databases on the same server) can delay
// visibility briefly.
func (e *env) settledFeed(productID uuid.UUID) []store.Event {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var total int
		if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE product_id = $1`, productID).Scan(&total); err != nil {
			e.t.Fatal(err)
		}
		feed, err := store.New(e.db.App).ListEventsFeed(e.ctx, store.ListEventsFeedParams{ProductID: productID, Lim: 10_000})
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
