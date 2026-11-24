package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/pkg/talksasapay"
)

func TestHistogramBuckets(t *testing.T) {
	// Every value maps into a bucket whose upper bound is >= it and within
	// 1/64 relative error.
	for _, v := range []uint64{0, 1, 63, 64, 65, 127, 128, 129, 1000, 1 << 20, 123456789, 1<<62 + 12345, 1<<64 - 1} {
		i := bucketIndex(v)
		if i < 0 || i >= numBuckets {
			t.Fatalf("v=%d idx=%d", v, i)
		}
		u := bucketUpper(i)
		if u < v || float64(u-v) > float64(v)/64+1 {
			t.Fatalf("v=%d upper=%d", v, u)
		}
		if i > 0 && bucketUpper(i-1) >= v {
			t.Fatalf("v=%d also fits bucket %d", v, i-1)
		}
	}
	// Buckets are contiguous.
	for i := 1; i < 2000; i++ {
		if bucketUpper(i) <= bucketUpper(i-1) || bucketIndex(bucketUpper(i-1)+1) != i {
			t.Fatalf("gap at %d", i)
		}
	}
}

func TestHistogramQuantiles(t *testing.T) {
	h := NewHistogram()
	if h.Quantile(.99) != 0 || h.Max() != 0 || h.Mean() != 0 {
		t.Fatal("empty histogram")
	}
	// 1..10000 ms, shuffled.
	r := rand.New(rand.NewPCG(1, 2))
	for _, i := range r.Perm(10000) {
		h.Record(time.Duration(i+1) * time.Millisecond)
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{.5, 5000 * time.Millisecond}, {.9, 9000 * time.Millisecond}, {.99, 9900 * time.Millisecond}, {1, 10000 * time.Millisecond}, {0, time.Millisecond}} {
		got := h.Quantile(c.q)
		if got < c.want || float64(got-c.want) > float64(c.want)*0.016 {
			t.Errorf("q%.2f = %v, want ~%v", c.q, got, c.want)
		}
	}
	if h.Max() != 10*time.Second || h.Count() != 10000 || h.Mean() != 5000500*time.Microsecond {
		t.Fatalf("max %v count %d mean %v", h.Max(), h.Count(), h.Mean())
	}
	// Quantiles never exceed the exact max, and merge adds up.
	h2 := NewHistogram()
	h2.Record(3 * time.Millisecond)
	if h2.Quantile(.99) != 3*time.Millisecond {
		t.Fatalf("single value: %v", h2.Quantile(.99))
	}
	h2.Record(-time.Second)
	h2.Merge(h)
	h2.Merge(NewHistogram())
	if h2.Count() != 10002 || h2.Max() != 10*time.Second || h2.Quantile(0) != 0 {
		t.Fatalf("merge: count %d max %v min %v", h2.Count(), h2.Max(), h2.Quantile(0))
	}
}

func TestParseMix(t *testing.T) {
	m, err := ParseMix(defaultMix)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Ops(), ",") != "upsert,create,get,list,events" || m.Weight("create") != 30 || m.Weight("upsert") != 20 || m.Weight("cancel") != 0 {
		t.Fatalf("%+v", m)
	}
	m, err = ParseMix(" get = 3 , cancel=0,whoami=1,")
	if err != nil || strings.Join(m.Ops(), ",") != "get,whoami" || m.Weight("whoami") != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{"", "get", "get=x", "get=-1", "nope=1", "get=1,get=2", "get=0", "get=10000000"} {
		if _, err := ParseMix(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Picks follow the weights.
	m, _ = ParseMix("get=1,list=3")
	r := rand.New(rand.NewPCG(3, 4))
	counts := map[string]int{}
	for range 40000 {
		counts[m.Pick(r)]++
	}
	if counts["get"] < 9000 || counts["get"] > 11000 || counts["list"] < 29000 || counts["list"] > 31000 {
		t.Fatalf("%v", counts)
	}
}

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(200, 1)
	start := time.Now()
	for range 41 {
		if err := b.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 180*time.Millisecond || el > 2*time.Second {
		t.Fatalf("41 tokens at 200/s took %v", el)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if newTokenBucket(1, 1).Wait(ctx) == nil {
		t.Fatal("cancelled wait succeeded")
	}
	if newTokenBucket(0, 1).Wait(context.Background()) != nil {
		t.Fatal("unlimited bucket blocked")
	}
}

// fakeAPI is an in-memory /v1 good enough for the generator.
type fakeAPI struct {
	t        *testing.T
	key      string
	secret   []byte
	fail     map[string]bool // "METHOD /pattern" -> respond 500
	slow     time.Duration
	mu       sync.Mutex
	payers   map[string]bool
	prs      map[string]map[string]any
	events   []map[string]any
	hookURL  string
	dupEvery int // deliver every n-th event twice (0 = never)
	wg       sync.WaitGroup
	srv      *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, key: "tsp_test_k1_supersecretvalue", secret: []byte("whsec_fake"), fail: map[string]bool{},
		payers: map[string]bool{}, prs: map[string]map[string]any{}}
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+f.key {
				f.problem(w, 401, "unauthorized")
				return
			}
			if f.slow > 0 {
				time.Sleep(f.slow)
			}
			if f.fail[pattern] {
				f.problem(w, 500, "internal_error")
				return
			}
			h(w, r)
		})
	}
	route("GET /v1/whoami", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, 200, map[string]any{"product_id": "p", "product_name": "Fake", "account_prefix": "FAKE", "key_id": "k1"})
	})
	route("PUT /v1/payers/{ref}", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["display_name"] == "" {
			f.problem(w, 422, "validation_failed")
			return
		}
		f.mu.Lock()
		existed := f.payers[r.PathValue("ref")]
		f.payers[r.PathValue("ref")] = true
		f.mu.Unlock()
		status := 200
		if !existed {
			status = 201
		}
		f.json(w, status, f.payer(r.PathValue("ref")))
	})
	route("GET /v1/payers/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ok := f.payers[r.PathValue("ref")]
		f.mu.Unlock()
		if !ok {
			f.problem(w, 404, "not_found")
			return
		}
		f.json(w, 200, f.payer(r.PathValue("ref")))
	})
	route("POST /v1/payment-requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			f.problem(w, 400, "idempotency_key_required")
			return
		}
		var in struct {
			Payer, Reference, Description string
			Amount                        int64
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Amount < 1 {
			f.problem(w, 422, "validation_failed")
			return
		}
		f.mu.Lock()
		if !f.payers[in.Payer] {
			f.mu.Unlock()
			f.problem(w, 422, "validation_failed")
			return
		}
		id := fmt.Sprintf("pr-%d", len(f.prs))
		pr := map[string]any{"object": "payment_request", "id": id, "reference": in.Reference, "amount": in.Amount,
			"amount_paid": 0, "outstanding": in.Amount, "status": "open", "currency": "KES",
			"payer":      map[string]any{"external_ref": in.Payer, "account_number": "FAKE-1"},
			"created_at": time.Now().UTC(), "updated_at": time.Now().UTC()}
		f.prs[id] = pr
		f.emitLocked("payment_request.created", pr)
		f.mu.Unlock()
		f.json(w, 201, pr)
	})
	route("GET /v1/payment-requests", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, 200, map[string]any{"data": []any{}, "next_cursor": nil})
	})
	route("GET /v1/payment-requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		pr, ok := f.prs[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			f.problem(w, 404, "not_found")
			return
		}
		f.json(w, 200, pr)
	})
	route("POST /v1/payment-requests/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		pr, ok := f.prs[r.PathValue("id")]
		if !ok {
			f.problem(w, 404, "not_found")
			return
		}
		if pr["status"] != "cancelled" {
			pr = maps.Clone(pr) // copy on write: other handlers encode the old map unlocked
			pr["status"] = "cancelled"
			f.prs[r.PathValue("id")] = pr
			f.emitLocked("payment_request.cancelled", pr)
		}
		f.json(w, 200, pr)
	})
	route("GET /v1/transactions", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, 200, map[string]any{"data": []any{}, "next_cursor": nil})
	})
	route("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if a := r.URL.Query().Get("after"); a != "" {
			start, _ = strconv.Atoi(strings.TrimPrefix(a, "e"))
		}
		limit := 50
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
			limit = l
		}
		f.mu.Lock()
		end := min(start+limit, len(f.events))
		data := append([]map[string]any{}, f.events[min(start, end):end]...)
		more := end < len(f.events)
		f.mu.Unlock()
		var next any
		if len(data) > 0 {
			next = fmt.Sprintf("e%d", end)
		}
		f.json(w, 200, map[string]any{"data": data, "next_cursor": next, "has_more": more})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(func() { f.wg.Wait(); f.srv.Close() })
	return f
}

func (f *fakeAPI) payer(ref string) map[string]any {
	return map[string]any{"object": "payer", "id": "id-" + ref, "external_ref": ref, "account_number": "FAKE-1",
		"display_name": "x", "email": nil, "phones": []string{}, "status": "active", "credit_balance": 0,
		"created_at": time.Now().UTC(), "updated_at": time.Now().UTC()}
}

func (f *fakeAPI) emitLocked(typ string, data map[string]any) {
	ev := map[string]any{"object": "event", "id": fmt.Sprintf("ev-%d", len(f.events)), "type": typ,
		"api_version": "2026-11-24", "created_at": time.Now().UTC(), "data": data}
	body, _ := json.Marshal(ev)
	f.events = append(f.events, ev)
	if f.hookURL == "" {
		return
	}
	times := 1
	if f.dupEvery > 0 && len(f.events)%f.dupEvery == 0 {
		times = 2
	}
	url := f.hookURL
	for range times {
		f.wg.Go(func() {
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(talksasapay.HeaderSignature, talksasapay.SignWebhook(time.Now(), body, f.secret))
			req.Header.Set(talksasapay.HeaderEventID, ev["id"].(string))
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		})
	}
}

func (f *fakeAPI) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"about:blank","title":"x","status":%d,"code":%q}`, status, code)
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runLoad(t *testing.T, f *fakeAPI, extra ...string) (int, string, string) {
	t.Helper()
	var out, errOut syncBuffer
	args := append([]string{"-base-url", f.srv.URL, "-key-file", writeFile(t, f.key+"\n"),
		"-duration", "400ms", "-concurrency", "4", "-payers", "5", "-max-p99", "5s"}, extra...)
	code := run(context.Background(), args, &out, &errOut)
	if strings.Contains(out.String()+errOut.String(), "supersecretvalue") {
		t.Fatal("API key printed")
	}
	return code, out.String(), errOut.String()
}

// syncBuffer is a goroutine-safe bytes.Buffer with an optional line hook.
type syncBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	onLine func(string)
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.onLine != nil {
		for _, l := range strings.Split(string(p), "\n") {
			b.onLine(l)
		}
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunSmoke(t *testing.T) {
	f := newFakeAPI(t)
	code, out, errOut := runLoad(t, f, "-mix", "upsert=2,create=3,get=3,list=1,events=1,cancel=1,txns=1,whoami=1", "-rps", "400")
	if code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	for _, op := range []string{"upsert", "create", "get", "list", "events", "cancel", "txns", "whoami", "all", "OK"} {
		if !strings.Contains(out, op) {
			t.Errorf("report lacks %q:\n%s", op, out)
		}
	}
	f.mu.Lock()
	nPR := len(f.prs)
	f.mu.Unlock()
	if nPR == 0 {
		t.Fatal("no payment requests created")
	}
	// -rps 400 for 0.4 s allows roughly 160 requests (+burst); be generous.
	var total int
	for _, l := range strings.Split(out, "\n") {
		fields := strings.Fields(l)
		if len(fields) > 2 && fields[0] == "all" {
			total, _ = strconv.Atoi(fields[1])
		}
	}
	if total == 0 || total > 260 {
		t.Fatalf("total %d requests\n%s", total, out)
	}
}

func TestRunFailsOn5xx(t *testing.T) {
	f := newFakeAPI(t)
	f.fail["GET /v1/payment-requests"] = true
	code, out, _ := runLoad(t, f, "-mix", "list=1,get=1")
	if code != 1 || !strings.Contains(out, "returned 5xx") || !strings.Contains(out, "500 internal_error") {
		t.Fatalf("code %d\n%s", code, out)
	}
	// Retried 5xx still count.
	f2 := newFakeAPI(t)
	f2.fail["GET /v1/payment-requests"] = true
	code, out, _ = runLoad(t, f2, "-mix", "list=1", "-retries", "1", "-concurrency", "1", "-duration", "50ms")
	if code != 1 || !strings.Contains(out, "returned 5xx") {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestRunFailsOnP99(t *testing.T) {
	f := newFakeAPI(t)
	f.slow = 5 * time.Millisecond
	code, out, _ := runLoad(t, f, "-mix", "whoami=1", "-max-p99", "1ms")
	if code != 1 || !strings.Contains(out, "above -max-p99") {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestRunFlagErrors(t *testing.T) {
	f := newFakeAPI(t)
	key := writeFile(t, f.key)
	for _, args := range [][]string{
		{"-key-file", key},
		{"-base-url", f.srv.URL},
		{"-base-url", f.srv.URL, "-key-file", key, "-mix", "bogus=1"},
		{"-base-url", f.srv.URL, "-key-file", key, "-duration", "0s"},
		{"-base-url", f.srv.URL, "-key-file", key, "-webhook-listen", "127.0.0.1:0"},
		{"-base-url", f.srv.URL, "-key-file", key, "-concurrency", "0"},
		{"-base-url", f.srv.URL, "-key-file", writeFile(t, "")},
		{"-base-url", f.srv.URL, "-key-file", writeFile(t, "a\nb")},
		{"-base-url", f.srv.URL, "-key-file", filepath.Join(t.TempDir(), "missing")},
		{"-base-url", f.srv.URL, "-key-file", key, "extra"},
		{"-base-url", f.srv.URL, "-key-file", key, "-mix", "stk=1,c2b=1"}, // c2b without -fake-daraja
	} {
		var out, errOut syncBuffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("%v: code %d (%s)", args, code, errOut.String())
		}
	}
	// Wrong key: whoami fails, exit 1, key not echoed.
	var out, errOut syncBuffer
	code := run(context.Background(), []string{"-base-url", f.srv.URL, "-key-file", writeFile(t, "tsp_test_k1_wrongsecret")}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "unauthorized") || strings.Contains(errOut.String(), "wrongsecret") {
		t.Fatalf("code %d %s", code, errOut.String())
	}
}

func TestWebhookReceiverReconcile(t *testing.T) {
	secret := []byte("whsec_recv")
	rc := newReceiver()
	srv := httptest.NewServer(rc.handler(secret))
	defer srv.Close()
	send := func(id string, key []byte) int {
		body := fmt.Appendf(nil, `{"object":"event","id":%q,"type":"payment_request.paid","api_version":"x","created_at":"2026-11-24T10:00:00Z","data":{}}`, id)
		req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader(body))
		req.Header.Set(talksasapay.HeaderSignature, talksasapay.SignWebhook(time.Now(), body, key))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		id   string
		key  []byte
		want int
	}{{"ev-1", secret, 200}, {"ev-1", secret, 200}, {"ev-2", secret, 200}, {"ev-x", secret, 200}, {"ev-3", []byte("wrong"), 400}} {
		if got := send(c.id, c.key); got != c.want {
			t.Fatalf("%s: %d", c.id, got)
		}
	}
	var out bytes.Buffer
	res, err := rc.reconcile(context.Background(), &out, func(context.Context) ([]string, error) {
		return []string{"ev-1", "ev-2", "ev-3"}, nil
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := webhookResult{feed: 3, delivered: 4, unique: 3, duplicates: 1, missing: 1, unexpected: 1, rejected: 1}
	if res != want {
		t.Fatalf("got %+v want %+v\n%s", res, want, out.String())
	}
	for _, s := range []string{"missing ev-3", "ev-1 payment_request.paid x2", "rejected signature_mismatch: 1"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, out.String())
		}
	}
}

func TestRunWithWebhookListener(t *testing.T) {
	f := newFakeAPI(t)
	f.dupEvery = 5
	var out, errOut syncBuffer
	out.onLine = func(l string) {
		if addr, ok := strings.CutPrefix(l, "webhook receiver on "); ok {
			f.mu.Lock()
			f.hookURL = "http://" + strings.TrimSpace(addr) + "/"
			f.mu.Unlock()
		}
	}
	args := []string{"-base-url", f.srv.URL, "-key-file", writeFile(t, f.key), "-duration", "300ms", "-concurrency", "3",
		"-payers", "3", "-mix", "create=3,cancel=1", "-webhook-listen", "127.0.0.1:0",
		"-webhook-secret-file", writeFile(t, string(f.secret)), "-webhook-grace", "5s"}
	code := run(context.Background(), args, &out, &errOut)
	s := out.String()
	if code != 0 || !strings.Contains(s, "webhooks: feed events") || !strings.Contains(s, "missing 0") {
		t.Fatalf("code %d\n%s\n%s", code, s, errOut.String())
	}
	if !strings.Contains(s, "x2") {
		t.Fatalf("duplicates not reported:\n%s", s)
	}
	if strings.Contains(s, "whsec_fake") {
		t.Fatal("webhook secret printed")
	}
}

func TestSTKPhoneEndings(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, d := range stkEndings {
		p := stkPhone(r, d)
		if len(p) != 10 || p[:2] != "07" || int(p[9]-'0') != d {
			t.Fatalf("stkPhone(%d) = %q", d, p)
		}
	}
	var pp promptPool
	pp.add(waitingPrompt{payer: 1, amount: 5})
	if w, ok := pp.take(r); !ok || w.amount != 5 {
		t.Fatal("prompt pool")
	}
	if _, ok := pp.take(r); ok {
		t.Fatal("prompt taken twice")
	}
}
