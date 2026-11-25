package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// fakeSTK stands in for *stk.Service.
type fakeSTK struct {
	mu        sync.Mutex
	available bool

	sends  []sendCall
	result func(p stk.SendParams) (stk.Push, bool, error)

	latest      *stk.Push
	latestErr   error
	latestCalls []latestCall

	stored   []storedCallback
	storeErr error
}

type sendCall struct {
	actor audit.Actor
	p     stk.SendParams
}

type latestCall struct {
	chargeID uuid.UUID
	since    time.Time
}

type storedCallback struct {
	kind stk.CallbackKind
	ip   netip.Addr
	raw  []byte
}

func newFakeSTK() *fakeSTK { return &fakeSTK{available: true} }

func (f *fakeSTK) Available() bool { return f.available }

func (f *fakeSTK) Send(_ context.Context, actor audit.Actor, p stk.SendParams) (stk.Push, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sendCall{actor, p})
	if f.result != nil {
		return f.result(p)
	}
	return testPush(p.AppID, p.ChargeID, store.StkPushStatusSent, nil), false, nil
}

func (f *fakeSTK) Get(context.Context, uuid.UUID, uuid.UUID) (stk.Push, error) {
	return stk.Push{}, stk.ErrNotFound
}

func (f *fakeSTK) LatestForCharge(_ context.Context, chargeID uuid.UUID, since time.Time) (*stk.Push, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestCalls = append(f.latestCalls, latestCall{chargeID, since})
	return f.latest, f.latestErr
}

func (f *fakeSTK) StoreCallback(_ context.Context, kind stk.CallbackKind, ip netip.Addr, raw []byte) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return uuid.Nil, f.storeErr
	}
	f.stored = append(f.stored, storedCallback{kind, ip, append([]byte(nil), raw...)})
	return uuid.New(), nil
}

func (f *fakeSTK) sendCalls() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.sends...)
}

func (f *fakeSTK) storedCallbacks() []storedCallback {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storedCallback(nil), f.stored...)
}

const fullPhone = "254712345678"

func testPush(appID, chargeID uuid.UUID, status store.StkPushStatus, resultCode *string) stk.Push {
	now := time.Now().UTC()
	co := "ws_CO_CHECKOUT_" + uuid.NewString()[:8]
	mr := "MERCHANT_" + uuid.NewString()[:8]
	return stk.Push{
		ID: uuid.New(), AppID: appID, ChargeID: chargeID, TransactionID: uuid.New(),
		Source: "pay_page", Phone: fullPhone, Amount: 1500, Status: status, CheckoutRequestID: &co,
		MerchantRequestID: &mr, ResultCode: resultCode, CreatedAt: now, UpdatedAt: now,
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

// lines decodes every JSON log line.
func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

type result struct {
	code int
	hdr  http.Header
	body string
}

func (r result) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return m
}

// expect checks status and problem code ("" = not a problem).
func (r result) expect(t *testing.T, status int, code string) map[string]any {
	t.Helper()
	if r.code != status {
		t.Fatalf("status %d, want %d: %s", r.code, status, r.body)
	}
	m := r.json(t)
	if code != "" && m["code"] != code {
		t.Fatalf("code %v, want %s: %s", m["code"], code, r.body)
	}
	return m
}

type reqOpt func(*http.Request)

func fromAddr(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func serve(h http.Handler, method, path string, body io.Reader, opts ...reqOpt) result {
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "198.51.100.7:40000"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return result{code: w.Code, hdr: w.Header(), body: w.Body.String()}
}
