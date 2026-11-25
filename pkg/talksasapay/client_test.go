package talksasapay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "tsp_test_abc123_s3cr3t"

// recorded is one request the fake server saw.
type recorded struct {
	Method, RawPath, RawQuery string
	Header                    http.Header
	Body                      string
}

// fake is a scripted server: each call pops the next responder (the last one
// repeats) and records the request.
type fake struct {
	t     *testing.T
	mu    sync.Mutex
	reqs  []recorded
	resps []func(w http.ResponseWriter, r *http.Request)
	srv   *httptest.Server
}

func newFake(t *testing.T, resps ...func(w http.ResponseWriter, r *http.Request)) *fake {
	f := &fake{t: t, resps: resps}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{Method: r.Method, RawPath: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(b)})
		i := len(f.reqs) - 1
		if i >= len(f.resps) {
			i = len(f.resps) - 1
		}
		h := f.resps[i]
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func jsonResp(status int, body any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func problem(status int, code string, hdr map[string]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("X-Request-Id", "req-hdr")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"about:blank","title":"t","status":%d,"code":%q,"detail":"d","request_id":"req-1"}`, status, code)
	}
}

// newTestClient builds a client whose sleeps are recorded instead of slept.
func newTestClient(t *testing.T, f *fake, opts ...Option) (*Client, *[]time.Duration) {
	t.Helper()
	c, err := New(f.srv.URL, testKey, opts...)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	slept := &[]time.Duration{}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*slept = append(*slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	return c, slept
}

const testChargeID = "0193a5f2-0000-7000-8000-000000000001"

var sampleCharge = map[string]any{
	"object": "charge", "id": testChargeID, "reference": "INV-1", "amount": 1500, "currency": "KES",
	"description": "Hosting", "metadata": map[string]any{"invoice_id": 42}, "account_number": "HOST-4K7QZ9M",
	"status": "pending", "checkout_url": "https://pay.example/c/abc",
	"paybill":    map[string]any{"business_number": "123456", "account_number": "HOST-4K7QZ9M"},
	"expires_at": "2026-11-25T10:00:00Z", "succeeded_at": nil, "transaction_id": nil, "latest_stk_push": nil,
	"created_at": "2026-11-24T10:00:00Z", "updated_at": "2026-11-24T10:00:00Z", "future_field": map[string]any{"x": 1},
}

var sampleTxn = map[string]any{
	"object": "transaction", "id": "0193a5f2-0000-7000-8000-00000000000a", "kind": "payment", "direction": "in",
	"channel": "mpesa_stk", "status": "succeeded", "amount": 1500, "currency": "KES", "receipt": "TKA1B2C3D4",
	"phone": "0712***678", "account_number": "HOST-4K7QZ9M", "charge_id": testChargeID, "reverses_transaction_id": nil,
	"occurred_at": "2026-11-24T10:01:00Z", "created_at": "2026-11-24T10:01:01Z", "completed_at": "2026-11-24T10:01:02Z",
}

// with returns a copy of m with the given keys replaced.
func with(m map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func checkCommon(t *testing.T, r recorded, method, path string) {
	t.Helper()
	if r.Method != method || r.RawPath != path {
		t.Fatalf("got %s %s, want %s %s", r.Method, r.RawPath, method, path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "talksasa-pay-go/"+Version) {
		t.Fatalf("User-Agent = %q", ua)
	}
	if r.Body != "" && r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q for a body", r.Header.Get("Content-Type"))
	}
	if r.Body == "" && r.Header.Get("Content-Type") != "" {
		t.Fatalf("Content-Type set without a body")
	}
}

func TestNew(t *testing.T) {
	for _, tc := range []struct {
		base, want string
		ok         bool
	}{
		{"https://api.example", "https://api.example/v1", true},
		{"https://api.example/", "https://api.example/v1", true},
		{"https://api.example/v1", "https://api.example/v1", true},
		{"https://api.example/v1/", "https://api.example/v1", true},
		{"http://127.0.0.1:8088/pay", "http://127.0.0.1:8088/pay/v1", true},
		{"api.example", "", false},
		{"ftp://api.example", "", false},
		{"https://api.example?x=1", "", false},
	} {
		c, err := New(tc.base, testKey)
		if (err == nil) != tc.ok {
			t.Fatalf("New(%q) err=%v", tc.base, err)
		}
		if tc.ok && c.base.String() != tc.want {
			t.Fatalf("New(%q) base=%s want %s", tc.base, c.base, tc.want)
		}
	}
	for _, k := range []string{"", "tsp_test_x\n", "a b"} {
		if _, err := New("https://api.example", k); err == nil {
			t.Fatalf("key %q accepted", k)
		}
	}
	c, _ := New("https://api.example", testKey)
	if strings.Contains(c.String(), testKey) || strings.Contains(fmt.Sprintf("%v %+v", c, c), "s3cr3t") {
		t.Fatal("client formatting leaks the API key")
	}
}

func TestWhoami(t *testing.T) {
	f := newFake(t, jsonResp(200, map[string]any{"app_id": "a1", "app_name": "Cloud Hosting", "account_prefix": "HOST", "key_id": "abc123", "extra": true}))
	c, _ := newTestClient(t, f, WithUserAgent("hosting/1.0"))
	w, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w.AppID != "a1" || w.AppName != "Cloud Hosting" || w.AccountPrefix != "HOST" || w.KeyID != "abc123" {
		t.Fatalf("%+v", w)
	}
	r := f.requests()[0]
	checkCommon(t, r, "GET", "/v1/whoami")
	if !strings.HasSuffix(r.Header.Get("User-Agent"), " hosting/1.0") {
		t.Fatalf("UA = %q", r.Header.Get("User-Agent"))
	}
}

func TestCreateCharge(t *testing.T) {
	replay := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Idempotent-Replayed", "true")
		jsonResp(200, sampleCharge)(w, r)
	}
	withPush := with(sampleCharge, "latest_stk_push", sampleSTK)
	f := newFake(t, jsonResp(201, withPush), replay, jsonResp(201, sampleCharge))
	c, _ := newTestClient(t, f)
	ctx := context.Background()
	in := ChargeInput{
		Amount: 1500, Reference: "INV-1", Description: "Hosting", Phone: " 0712345678 ", AccountNumber: "HOST-1042",
		Metadata: map[string]any{"invoice_id": 42}, ExpiresIn: time.Hour, IdempotencyKey: "INV-1-v1",
	}
	ch, replayed, err := c.CreateCharge(ctx, in)
	if err != nil || replayed {
		t.Fatalf("replayed=%v err=%v", replayed, err)
	}
	if ch.Amount != 1500 || !ch.Pending() || ch.Succeeded() || ch.Paybill == nil || ch.AccountNumber != "HOST-4K7QZ9M" ||
		ch.CheckoutURL != "https://pay.example/c/abc" || ch.Metadata["invoice_id"] != float64(42) || *ch.Description != "Hosting" ||
		ch.TransactionID != nil || ch.SucceededAt != nil || ch.ExpiresAt.IsZero() {
		t.Fatalf("%+v", ch)
	}
	if ch.LatestSTKPush == nil || ch.LatestSTKPush.ChargeID != testChargeID || !ch.LatestSTKPush.Pending() {
		t.Fatalf("latest push %+v", ch.LatestSTKPush)
	}
	if _, replayed, err = c.CreateCharge(ctx, in); err != nil || !replayed {
		t.Fatalf("replayed=%v err=%v", replayed, err)
	}
	if _, _, err := c.CreateCharge(ctx, ChargeInput{Amount: 10, Reference: "r"}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	checkCommon(t, reqs[0], "POST", "/v1/charges")
	if reqs[0].Header.Get("Idempotency-Key") != "INV-1-v1" {
		t.Fatalf("key %q", reqs[0].Header.Get("Idempotency-Key"))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["amount"] != float64(1500) || body["reference"] != "INV-1" || body["phone"] != "0712345678" ||
		body["account_number"] != "HOST-1042" || body["expires_in"] != float64(3600) || body["description"] != "Hosting" ||
		body["metadata"].(map[string]any)["invoice_id"] != float64(42) || len(body) != 7 {
		t.Fatalf("body %v", body)
	}
	if strings.Contains(reqs[0].Body, "INV-1-v1") {
		t.Fatal("idempotency key leaked into the body")
	}
	if reqs[2].Body != `{"amount":10,"reference":"r"}` {
		t.Fatalf("minimal body %s", reqs[2].Body)
	}
	auto := reqs[2].Header.Get("Idempotency-Key")
	if !strings.HasPrefix(auto, "tspgo_") || len(auto) != 6+32 {
		t.Fatalf("auto key %q", auto)
	}
	if k1, k2 := NewIdempotencyKey(), NewIdempotencyKey(); k1 == k2 {
		t.Fatal("keys repeat")
	}
}

func TestCreateChargeValidatesLocally(t *testing.T) {
	f := newFake(t, jsonResp(201, sampleCharge))
	c, _ := newTestClient(t, f)
	for _, in := range []ChargeInput{
		{Amount: 1, Reference: "r", IdempotencyKey: strings.Repeat("k", 256)},
		{Amount: 1, Reference: "r", IdempotencyKey: "bad\nkey"},
		{Amount: 1, Reference: "r", Phone: strings.Repeat("7", 21)},
		{Amount: 1, Reference: "r", ExpiresIn: 1500 * time.Millisecond},
	} {
		if _, _, err := c.CreateCharge(context.Background(), in); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestGetAndCancelCharge(t *testing.T) {
	succeeded := with(sampleCharge, "status", "succeeded", "paybill", nil, "transaction_id", sampleTxn["id"],
		"succeeded_at", "2026-11-24T10:01:02Z", "latest_stk_push", with(sampleSTK, "status", "succeeded"))
	f := newFake(t, jsonResp(200, succeeded), jsonResp(200, with(sampleCharge, "status", "cancelled", "paybill", nil)))
	c, _ := newTestClient(t, f)
	ctx := context.Background()
	ch, err := c.GetCharge(ctx, testChargeID)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Succeeded() || ch.Paybill != nil || *ch.TransactionID != sampleTxn["id"] || ch.SucceededAt == nil || ch.LatestSTKPush.Status != STKSucceeded {
		t.Fatalf("%+v", ch)
	}
	ch, err = c.CancelCharge(ctx, "a/b")
	if err != nil || ch.Status != ChargeCancelled || ch.Pending() {
		t.Fatalf("%+v %v", ch, err)
	}
	if _, err := c.GetCharge(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, err := c.CancelCharge(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	checkCommon(t, reqs[0], "GET", "/v1/charges/"+testChargeID)
	checkCommon(t, reqs[1], "POST", "/v1/charges/a%2Fb/cancel")
	if reqs[1].Body != "" || reqs[1].Header.Get("Idempotency-Key") != "" {
		t.Fatalf("cancel sent body %q key %q", reqs[1].Body, reqs[1].Header.Get("Idempotency-Key"))
	}
}

func TestPathSegmentEscaping(t *testing.T) {
	f := newFake(t, jsonResp(200, sampleCharge))
	c, _ := newTestClient(t, f)
	ctx := context.Background()
	for _, id := range []string{"cust/1 ?#%", "..", "ü"} {
		if _, err := c.GetCharge(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.requests()
	for i, want := range []string{"/v1/charges/cust%2F1%20%3F%23%25", "/v1/charges/%2E%2E", "/v1/charges/%C3%BC"} {
		checkCommon(t, reqs[i], "GET", want)
	}
}

func TestTransactions(t *testing.T) {
	reversal := with(sampleTxn, "id", "rev-1", "kind", "reversal", "direction", "out", "channel", "manual", "receipt", nil,
		"reverses_transaction_id", sampleTxn["id"])
	f := newFake(t, jsonResp(200, sampleTxn), jsonResp(200, map[string]any{"data": []any{sampleTxn, reversal}, "next_cursor": nil}))
	c, _ := newTestClient(t, f)
	ctx := context.Background()
	tx, err := c.GetTransaction(ctx, "0193a5f2-0000-7000-8000-00000000000a")
	if err != nil {
		t.Fatal(err)
	}
	if tx.Amount != 1500 || *tx.Receipt != "TKA1B2C3D4" || *tx.ChargeID != testChargeID || *tx.AccountNumber != "HOST-4K7QZ9M" || tx.Kind != KindPayment {
		t.Fatalf("%+v", tx)
	}
	since := time.Date(2026, 11, 1, 3, 0, 0, 0, time.FixedZone("EAT", 3*3600))
	var kinds []string
	for tx, err := range c.ListTransactions(ctx, ListTransactionsParams{ChargeID: testChargeID, Since: since, Limit: 10}) {
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, tx.Kind)
	}
	if strings.Join(kinds, ",") != "payment,reversal" {
		t.Fatalf("kinds %v", kinds)
	}
	reqs := f.requests()
	checkCommon(t, reqs[0], "GET", "/v1/transactions/0193a5f2-0000-7000-8000-00000000000a")
	checkCommon(t, reqs[1], "GET", "/v1/transactions")
	if reqs[1].RawQuery != "charge_id="+testChargeID+"&limit=10&since=2026-11-01T00%3A00%3A00Z" {
		t.Fatalf("query %s", reqs[1].RawQuery)
	}
}

func TestListChargesIterator(t *testing.T) {
	page := func(cursor any, ids ...string) func(http.ResponseWriter, *http.Request) {
		var data []map[string]any
		for _, id := range ids {
			data = append(data, with(sampleCharge, "id", id))
		}
		return jsonResp(200, map[string]any{"data": data, "next_cursor": cursor})
	}
	f := newFake(t, page("c1", "a", "b"), page("c2", "c"), page(nil, "d"))
	c, _ := newTestClient(t, f)
	var ids []string
	for ch, err := range c.ListCharges(context.Background(), ListChargesParams{Status: ChargePending, Reference: "INV 1", Limit: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ch.ID)
	}
	if strings.Join(ids, ",") != "a,b,c,d" {
		t.Fatalf("ids %v", ids)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	want := []string{"limit=2&reference=INV+1&status=pending", "cursor=c1&limit=2&reference=INV+1&status=pending", "cursor=c2&limit=2&reference=INV+1&status=pending"}
	for i, r := range reqs {
		checkCommon(t, r, "GET", "/v1/charges")
		if r.RawQuery != want[i] {
			t.Fatalf("page %d query %s", i, r.RawQuery)
		}
	}

	// Breaking early fetches no further pages.
	f2 := newFake(t, page("c1", "a", "b"))
	c2, _ := newTestClient(t, f2)
	for range c2.ListCharges(context.Background(), ListChargesParams{}) {
		break
	}
	if len(f2.requests()) != 1 {
		t.Fatal("iterator fetched after break")
	}

	// A repeating cursor ends with an error instead of looping forever.
	f3 := newFake(t, page("same", "a"))
	c3, _ := newTestClient(t, f3)
	var lastErr error
	for _, err := range c3.ListCharges(context.Background(), ListChargesParams{}) {
		lastErr = err
	}
	if lastErr == nil || len(f3.requests()) != 2 {
		t.Fatalf("err=%v requests=%d", lastErr, len(f3.requests()))
	}

	// Errors are yielded once and end iteration.
	f4 := newFake(t, page("c1", "a"), problem(404, CodeNotFound, nil))
	c4, _ := newTestClient(t, f4)
	var got []error
	for _, err := range c4.ListCharges(context.Background(), ListChargesParams{}) {
		got = append(got, err)
	}
	if len(got) != 2 || got[0] != nil || !IsNotFound(got[1]) {
		t.Fatalf("%v", got)
	}

	// The single-page call.
	f5 := newFake(t, page("next", "x"))
	c5, _ := newTestClient(t, f5)
	p, err := c5.ListChargesPage(context.Background(), ListChargesParams{Cursor: "c9"})
	if err != nil || len(p.Data) != 1 || *p.NextCursor != "next" || f5.requests()[0].RawQuery != "cursor=c9" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestErrorDecoding(t *testing.T) {
	validation := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("X-Request-Id", "req-v")
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"type":"https://docs/validation","title":"Invalid request","status":422,"code":"validation_failed","detail":"2 fields are invalid","errors":[{"field":"amount","message":"must be at least 1"},{"field":"reference","message":"required"}],"new":1}`)
	}
	html := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("X-Request-Id", "req-h")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "<html>"+strings.Repeat("x", 500)+"</html>")
	}
	f := newFake(t, problem(409, CodeDuplicateReference, nil), validation, html)
	c, _ := newTestClient(t, f)
	ctx := context.Background()

	_, _, err := c.CreateCharge(ctx, ChargeInput{Reference: "r", Amount: 1})
	if !IsCode(err, CodeDuplicateReference) || IsCode(err, CodeNotFound) || StatusCode(err) != 409 {
		t.Fatalf("%v", err)
	}
	ae, _ := AsAPIError(err)
	if ae.RequestID != "req-1" || ae.Detail != "d" || ae.Title != "t" || ae.Temporary() {
		t.Fatalf("%+v", ae)
	}
	if !strings.Contains(err.Error(), "409 duplicate_reference: d (request_id req-1)") {
		t.Fatalf("%q", err.Error())
	}
	_, _, err = c.CreateCharge(ctx, ChargeInput{})
	ae, ok := AsAPIError(err)
	if !ok || ae.Code != CodeValidationFailed || len(ae.FieldErrors) != 2 || ae.FieldErrors[0].Field != "amount" || ae.RequestID != "req-v" {
		t.Fatalf("%+v", ae)
	}
	if !strings.Contains(err.Error(), "[amount: must be at least 1; reference: required]") {
		t.Fatalf("%q", err.Error())
	}
	_, err = c.Whoami(ctx)
	ae, ok = AsAPIError(err)
	if !ok || ae.Status != 500 || ae.Code != "" || ae.RequestID != "req-h" || len(ae.Detail) > 210 || ae.Title != "Internal Server Error" {
		t.Fatalf("%+v", ae)
	}
	if IsCode(errors.New("x"), "x") || StatusCode(nil) != 0 {
		t.Fatal("helpers on non-API errors")
	}
	if strings.Contains(fmt.Sprint(err), testKey) {
		t.Fatal("error leaks key")
	}
}

func TestRetryGETOnTransientStatuses(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		f := newFake(t, problem(status, "x", nil), problem(status, "x", nil), jsonResp(200, map[string]any{"app_id": "a"}))
		var events []LogEvent
		c, slept := newTestClient(t, f, WithRetryBackoff(100*time.Millisecond, time.Second), WithLogger(func(e LogEvent) { events = append(events, e) }))
		if _, err := c.Whoami(context.Background()); err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if len(f.requests()) != 3 || len(*slept) != 2 {
			t.Fatalf("%d: %d requests, %d sleeps", status, len(f.requests()), len(*slept))
		}
		// equal jitter: attempt 1 in [50,100]ms, attempt 2 in [100,200]ms
		if d := (*slept)[0]; d < 50*time.Millisecond || d > 100*time.Millisecond {
			t.Fatalf("first backoff %v", d)
		}
		if d := (*slept)[1]; d < 100*time.Millisecond || d > 200*time.Millisecond {
			t.Fatalf("second backoff %v", d)
		}
		if len(events) != 3 || events[0].RetryIn == 0 || events[0].Status != status || events[0].RequestID != "req-hdr" || events[2].Err != nil || events[2].Attempt != 3 {
			t.Fatalf("log events %+v", events)
		}
	}
}

func TestNoRetryOnPermanentErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 422, 500} {
		f := newFake(t, problem(status, "x", nil), jsonResp(200, map[string]any{}))
		c, _ := newTestClient(t, f)
		if _, err := c.Whoami(context.Background()); StatusCode(err) != status {
			t.Fatalf("%d: %v", status, err)
		}
		if len(f.requests()) != 1 {
			t.Fatalf("%d retried", status)
		}
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	f := newFake(t, problem(429, CodeRateLimited, map[string]string{"Retry-After": "7"}), jsonResp(200, map[string]any{}))
	c, slept := newTestClient(t, f)
	if _, err := c.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Fatalf("slept %v", *slept)
	}
	// A Retry-After beyond the cap is returned to the caller instead.
	f2 := newFake(t, problem(503, CodeOverloaded, map[string]string{"Retry-After": "3600"}))
	c2, slept2 := newTestClient(t, f2)
	_, err := c2.Whoami(context.Background())
	ae, ok := AsAPIError(err)
	if !ok || ae.RetryAfter != time.Hour || len(*slept2) != 0 || len(f2.requests()) != 1 {
		t.Fatalf("err=%v slept=%v", err, *slept2)
	}
	if !ae.Temporary() {
		t.Fatal("503 should be temporary")
	}
	// HTTP-date form.
	if d := parseRetryAfter(time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat), time.Now()); d < 85*time.Second || d > 91*time.Second {
		t.Fatalf("date form %v", d)
	}
	if parseRetryAfter("-3", time.Now()) != 0 || parseRetryAfter("soon", time.Now()) != 0 {
		t.Fatal("junk Retry-After")
	}
}

func TestRetriesExhausted(t *testing.T) {
	f := newFake(t, problem(503, CodeOverloaded, nil))
	c, slept := newTestClient(t, f, WithMaxRetries(2))
	_, err := c.Whoami(context.Background())
	if !IsCode(err, CodeOverloaded) || len(f.requests()) != 3 || len(*slept) != 2 {
		t.Fatalf("err=%v requests=%d", err, len(f.requests()))
	}
	f0 := newFake(t, problem(503, CodeOverloaded, nil))
	c0, _ := newTestClient(t, f0, WithMaxRetries(0))
	_, _ = c0.Whoami(context.Background())
	if len(f0.requests()) != 1 {
		t.Fatal("WithMaxRetries(0) retried")
	}
}

func TestBackoffCapped(t *testing.T) {
	c, _ := New("https://x", testKey, WithRetryBackoff(time.Second, 4*time.Second))
	for i := 0; i < 50; i++ {
		if d := c.backoff(10); d < 2*time.Second || d > 4*time.Second {
			t.Fatalf("backoff(10)=%v", d)
		}
	}
}

func TestPOSTRetrySafety(t *testing.T) {
	ctx := context.Background()
	// Create always carries a key, so it is retried, with the SAME key.
	f := newFake(t, problem(502, "", nil), jsonResp(201, sampleCharge))
	c, _ := newTestClient(t, f)
	if _, _, err := c.CreateCharge(ctx, ChargeInput{Reference: "r", Amount: 1}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].Header.Get("Idempotency-Key") == "" || reqs[0].Header.Get("Idempotency-Key") != reqs[1].Header.Get("Idempotency-Key") || reqs[0].Body != reqs[1].Body {
		t.Fatalf("create retry: %d requests, keys %q %q", len(reqs), reqs[0].Header.Get("Idempotency-Key"), reqs[len(reqs)-1].Header.Get("Idempotency-Key"))
	}
	// Cancel is idempotent by contract.
	f2 := newFake(t, problem(503, "", nil), jsonResp(200, sampleCharge))
	c2, _ := newTestClient(t, f2)
	if _, err := c2.CancelCharge(ctx, "id"); err != nil || len(f2.requests()) != 2 {
		t.Fatalf("cancel: %v, %d requests", err, len(f2.requests()))
	}
	// A POST that is neither keyed nor idempotent is never retried.
	f3 := newFake(t, problem(503, "", nil), jsonResp(200, map[string]any{}))
	c3, _ := newTestClient(t, f3)
	_, err := c3.do(ctx, request{method: http.MethodPost, path: "/something", body: map[string]int{"a": 1}}, nil)
	if StatusCode(err) != 503 || len(f3.requests()) != 1 {
		t.Fatalf("unsafe POST: %v, %d requests", err, len(f3.requests()))
	}
}

// dropConn closes the connection without a response (a network error).
func dropConn(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("no hijacker")
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func TestNetworkErrors(t *testing.T) {
	ctx := context.Background()
	f := newFake(t, dropConn, jsonResp(200, sampleCharge))
	c, _ := newTestClient(t, f)
	if _, err := c.GetCharge(ctx, "x"); err != nil || len(f.requests()) != 2 {
		t.Fatalf("GET after drop: %v, %d", err, len(f.requests()))
	}
	f2 := newFake(t, dropConn, jsonResp(200, sampleCharge))
	c2, _ := newTestClient(t, f2)
	if _, _, err := c2.CreateCharge(ctx, ChargeInput{Reference: "x", Amount: 1}); err != nil || len(f2.requests()) != 2 {
		t.Fatalf("keyed POST after drop: %v, %d", err, len(f2.requests()))
	}
	f3 := newFake(t, dropConn, jsonResp(200, map[string]any{}))
	c3, _ := newTestClient(t, f3)
	_, err := c3.do(ctx, request{method: http.MethodPost, path: "/x", body: 1}, nil)
	if err == nil || StatusCode(err) != 0 || len(f3.requests()) != 1 {
		t.Fatalf("unsafe POST after drop: %v, %d", err, len(f3.requests()))
	}
	if strings.Contains(err.Error(), f3.srv.URL) {
		t.Fatalf("error repeats the URL: %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	// The real sleeper stops waiting when the context ends.
	f := newFake(t, problem(503, CodeOverloaded, map[string]string{"Retry-After": "30"}))
	c, err := New(f.srv.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = c.Whoami(ctx)
	if !IsCode(err, CodeOverloaded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	// A deadline shorter than the wait returns immediately without sleeping.
	c2, slept := newTestClient(t, f)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := c2.Whoami(ctx2); !IsCode(err, CodeOverloaded) || len(*slept) != 0 {
		t.Fatalf("err=%v slept=%v", err, *slept)
	}
	// Per-attempt timeout.
	block := make(chan struct{})
	defer close(block)
	f3 := newFake(t, func(http.ResponseWriter, *http.Request) { <-block })
	c3, _ := newTestClient(t, f3, WithTimeout(50*time.Millisecond), WithMaxRetries(1))
	if _, err := c3.Whoami(context.Background()); !errors.Is(err, context.DeadlineExceeded) || len(f3.requests()) != 2 {
		t.Fatalf("timeout: %v, %d requests", err, len(f3.requests()))
	}
}

func TestLoggerNeverSeesKey(t *testing.T) {
	f := newFake(t, problem(401, CodeUnauthorized, nil))
	var buf strings.Builder
	c, _ := newTestClient(t, f, WithLogger(func(e LogEvent) { fmt.Fprintf(&buf, "%+v %v\n", e, e.Err) }))
	_, _ = c.Whoami(context.Background())
	if buf.Len() == 0 || strings.Contains(buf.String(), "s3cr3t") {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestBadSuccessBody(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not json") })
	c, _ := newTestClient(t, f)
	if _, err := c.Whoami(context.Background()); err == nil || len(f.requests()) != 1 {
		t.Fatalf("err=%v", err)
	}
}
