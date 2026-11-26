package mpesa_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
)

const (
	testKey      = "CONSUMERKEY-aaaaaaaaaaaa"
	testSecret   = "CONSUMERSECRET-bbbbbbbbbbbb"
	testPasskey  = "PASSKEY-bfb279f9aa9bdbcf158e97dd71a467cd2e0c893059b10f78e6b72ada1ed2c919"
	testCBToken  = "CBTOKEN_cccccccccccccccccccccccccccccccccccccc"
	testToken    = "ACCESSTOKEN-dddddddddddd"
	testShort    = "174379"
	testCallback = "https://api.example.test/"
	testPhone    = "254708374149"
)

// darajaStub is a scriptable Daraja: oauth plus one handler per API path.
type darajaStub struct {
	t          *testing.T
	srv        *httptest.Server
	tokenCalls atomic.Int64
	tokenDelay time.Duration
	tokenTTL   string
	tokenFn    func(n int64) (int, string) // optional override
	mu         sync.Mutex
	api        map[string]http.HandlerFunc
	apiCalls   map[string]int
	bodies     map[string][]byte
}

func newStub(t *testing.T) *darajaStub {
	s := &darajaStub{t: t, tokenTTL: "3599", api: map[string]http.HandlerFunc{}, apiCalls: map[string]int{}, bodies: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *darajaStub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth/v1/generate" {
		n := s.tokenCalls.Add(1)
		if s.tokenDelay > 0 {
			time.Sleep(s.tokenDelay)
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != testKey || p != testSecret || r.URL.Query().Get("grant_type") != "client_credentials" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"errorCode":"400.008.01","errorMessage":"Invalid Authentication passed"}`)
			return
		}
		if s.tokenFn != nil {
			code, body := s.tokenFn(n)
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"`+testToken+`","expires_in":"`+s.tokenTTL+`"}`)
		return
	}
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	h := s.api[r.URL.Path]
	s.apiCalls[r.URL.Path]++
	s.bodies[r.URL.Path] = b
	s.mu.Unlock()
	if h == nil {
		w.WriteHeader(404)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	h(w, r)
}

func (s *darajaStub) handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.api["/"+path] = h
}

func (s *darajaStub) calls(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiCalls["/"+path]
}

func (s *darajaStub) body(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies["/"+path]
}

func reply(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func opts(base string) mpesa.ClientOptions {
	return mpesa.ClientOptions{
		Env: config.MpesaSandbox, BaseURL: base, ShortCode: testShort,
		ConsumerKey: config.NewSecret(testKey), ConsumerSecret: config.NewSecret(testSecret),
		Passkey: config.NewSecret(testPasskey), CallbackToken: config.NewSecret(testCBToken),
		CallbackBaseURL: testCallback, Timeout: 2 * time.Second,
	}
}

func newClient(t *testing.T, s *darajaStub, mod ...func(*mpesa.ClientOptions)) *mpesa.HTTPClient {
	t.Helper()
	o := opts(s.srv.URL + "/")
	for _, m := range mod {
		m(&o)
	}
	c, err := mpesa.NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const (
	okPush  = `{"MerchantRequestID":"29115-34620561-1","CheckoutRequestID":"ws_CO_191220191020363925","ResponseCode":"0","ResponseDescription":"Success. Request accepted for processing","CustomerMessage":"Success. Request accepted for processing"}`
	okQuery = `{"ResponseCode":"0","ResponseDescription":"accepted","MerchantRequestID":"m","CheckoutRequestID":"c","ResultCode":"0","ResultDesc":"The service request is processed successfully."}`
)

func pushReq() mpesa.STKPushRequest {
	return mpesa.STKPushRequest{Phone: testPhone, Amount: 100, AccountReference: "HOST-1042", TransactionDesc: "PR-1"}
}

func TestNewClientValidation(t *testing.T) {
	o := opts("")
	o.Env = "live"
	o.ShortCode = "12"
	o.Passkey = config.Secret{}
	o.CallbackToken = config.NewSecret("short")
	o.CallbackBaseURL = "https://x.test/no-slash"
	_, err := mpesa.NewClient(o)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"Env", "ShortCode", "Passkey", "CallbackToken", "CallbackBaseURL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "short") {
		t.Fatalf("error leaks secrets: %v", err)
	}
	if _, err := mpesa.NewClient(opts("")); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

func TestCallbackURLs(t *testing.T) {
	c, err := mpesa.NewClient(opts(""))
	if err != nil {
		t.Fatal(err)
	}
	stk, conf, val := c.CallbackURLs()
	p := testCallback + "cb/hooks/" + testCBToken
	if stk != p+"/stk" || conf != p+"/c2b/confirmation" || val != p+"/c2b/validation" {
		t.Fatalf("%s %s %s", stk, conf, val)
	}
}

func TestTokenSingleflight(t *testing.T) {
	s := newStub(t)
	s.tokenDelay = 50 * time.Millisecond
	s.handle(mpesa.PathSTKQuery, reply(200, okQuery))
	c := newClient(t, s)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.STKQuery(context.Background(), "ws_CO_1"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := s.tokenCalls.Load(); n != 1 {
		t.Fatalf("token requests = %d, want 1", n)
	}
	if n := s.calls(mpesa.PathSTKQuery); n != 100 {
		t.Fatalf("queries = %d", n)
	}
}

func TestTokenWaiterContextCancel(t *testing.T) {
	s := newStub(t)
	s.tokenDelay = 300 * time.Millisecond
	s.handle(mpesa.PathSTKQuery, reply(200, okQuery))
	c := newClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.STKPush(ctx, pushReq())
	if !mpesa.IsRejected(err) { // no prompt was sent
		t.Fatalf("push with cancelled token wait: %v", err)
	}
	// The shared fetch was not cancelled with the first caller.
	if _, err := c.STKQuery(context.Background(), "ws_CO_1"); err != nil {
		t.Fatal(err)
	}
	if n := s.tokenCalls.Load(); n != 1 {
		t.Fatalf("token requests = %d, want 1", n)
	}
}

func TestTokenRefreshNearExpiry(t *testing.T) {
	s := newStub(t)
	s.tokenTTL = "3600"
	s.handle(mpesa.PathSTKQuery, reply(200, okQuery))
	var mu sync.Mutex
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	c := newClient(t, s, func(o *mpesa.ClientOptions) {
		o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	q := func() {
		t.Helper()
		if _, err := c.STKQuery(context.Background(), "c"); err != nil {
			t.Fatal(err)
		}
	}
	q()
	advance(54 * time.Minute)
	q()
	if n := s.tokenCalls.Load(); n != 1 {
		t.Fatalf("token requests after 54m = %d, want 1", n)
	}
	advance(time.Minute + time.Second) // 55m01s: within 5 minutes of expiry
	q()
	if n := s.tokenCalls.Load(); n != 2 {
		t.Fatalf("token requests after 55m = %d, want 2", n)
	}
}

func TestUnauthorizedRefetchesAndRetriesOnce(t *testing.T) {
	s := newStub(t)
	var n atomic.Int64
	s.handle(mpesa.PathSTKPush, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		if n.Add(1) == 1 {
			reply(401, `{"errorCode":"404.001.04","errorMessage":"Invalid Access Token"}`)(w, r)
			return
		}
		reply(200, okPush)(w, r)
	})
	c := newClient(t, s)
	if _, err := c.STKPush(context.Background(), pushReq()); err != nil {
		t.Fatal(err)
	}
	if s.tokenCalls.Load() != 2 || s.calls(mpesa.PathSTKPush) != 2 {
		t.Fatalf("token=%d push=%d", s.tokenCalls.Load(), s.calls(mpesa.PathSTKPush))
	}

	// Persistent 401: one retry only, then rejected.
	s.handle(mpesa.PathSTKPush, reply(401, `{"errorCode":"404.001.04","errorMessage":"Invalid Access Token"}`))
	_, err := c.STKPush(context.Background(), pushReq())
	var e *mpesa.Error
	if !errors.As(err, &e) || e.Kind != mpesa.Rejected || e.HTTPStatus != 401 || e.Code != "404.001.04" {
		t.Fatalf("%#v", err)
	}
	if s.calls(mpesa.PathSTKPush) != 4 {
		t.Fatalf("push calls = %d, want 4", s.calls(mpesa.PathSTKPush))
	}
}

func TestSTKPushRequestBody(t *testing.T) {
	s := newStub(t)
	s.handle(mpesa.PathSTKPush, reply(200, okPush))
	now := time.Date(2026, 11, 24, 21, 30, 5, 0, time.UTC) // 00:30:05 next day in Nairobi
	c := newClient(t, s, func(o *mpesa.ClientOptions) { o.Now = func() time.Time { return now } })
	res, err := c.STKPush(context.Background(), mpesa.STKPushRequest{
		Phone: testPhone, Amount: 1500, AccountReference: "HOST-1042-EXTRA-LONG", TransactionDesc: "Invoice INV-2026-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckoutRequestID != "ws_CO_191220191020363925" || res.MerchantRequestID != "29115-34620561-1" || res.ResponseCode != "0" {
		t.Fatalf("%+v", res)
	}
	var b map[string]any
	if err := json.Unmarshal(s.body(mpesa.PathSTKPush), &b); err != nil {
		t.Fatal(err)
	}
	if b["Timestamp"] != "20261125003005" {
		t.Fatalf("timestamp %v", b["Timestamp"])
	}
	pw, err := base64.StdEncoding.DecodeString(b["Password"].(string))
	if err != nil || string(pw) != testShort+testPasskey+"20261125003005" {
		t.Fatalf("password does not decode to shortcode+passkey+timestamp (err %v)", err)
	}
	want := map[string]any{
		"BusinessShortCode": testShort, "TransactionType": "CustomerPayBillOnline", "Amount": 1500.0,
		"PartyA": testPhone, "PartyB": testShort, "PhoneNumber": testPhone,
		"CallBackURL":      testCallback + "cb/hooks/" + testCBToken + "/stk",
		"AccountReference": "HOST-1042-EX", "TransactionDesc": "Invoice INV-2",
	}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("%s = %v, want %v", k, b[k], v)
		}
	}
}

func TestSTKPushValidatesLocally(t *testing.T) {
	s := newStub(t)
	c := newClient(t, s)
	for _, r := range []mpesa.STKPushRequest{
		{Phone: "0708374149", Amount: 10}, {Phone: testPhone, Amount: 0}, {Phone: testPhone, Amount: mpesa.MaxAmount + 1},
	} {
		if _, err := c.STKPush(context.Background(), r); !mpesa.IsRejected(err) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if s.tokenCalls.Load() != 0 {
		t.Fatal("invalid requests must not reach Daraja")
	}
}

func TestSTKPushClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   mpesa.ErrorKind // 0 = success
		code   string
	}{
		{"accepted", 200, okPush, 0, ""},
		{"response code non-zero", 200, `{"ResponseCode":"1","ResponseDescription":"Rejected"}`, mpesa.Rejected, "1"},
		{"accepted without checkout id", 200, `{"ResponseCode":"0"}`, mpesa.Transient, ""},
		{"unreadable 200", 200, `<html>`, mpesa.Transient, ""},
		{"bad request", 400, `{"requestId":"r","errorCode":"400.002.02","errorMessage":"Bad Request - Invalid PhoneNumber"}`, mpesa.Rejected, "400.002.02"},
		{"forbidden", 403, `{}`, mpesa.Rejected, ""},
		{"request timeout", 408, ``, mpesa.Transient, ""},
		{"spike arrest", 429, `{"fault":{"faultstring":"Spike arrest violation","detail":{"errorcode":"policies.ratelimit.SpikeArrestViolation"}}}`, mpesa.Transient, "policies.ratelimit.SpikeArrestViolation"},
		{"server error", 500, `{"requestId":"r","errorCode":"500.003.1001","errorMessage":"Internal Server Error"}`, mpesa.Transient, "500.003.1001"},
		{"bad gateway", 502, `not json`, mpesa.Transient, ""},
		{"redirect", 302, ``, mpesa.Transient, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.handle(mpesa.PathSTKPush, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 302 {
					w.Header().Set("Location", "/elsewhere")
				}
				reply(tc.status, tc.body)(w, r)
			})
			_, err := newClient(t, s).STKPush(context.Background(), pushReq())
			if tc.kind == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var e *mpesa.Error
			if !errors.As(err, &e) || e.Kind != tc.kind || e.Code != tc.code {
				t.Fatalf("got %#v (%v), want kind %d code %q", err, err, tc.kind, tc.code)
			}
			if tc.status != 302 && e.HTTPStatus != tc.status {
				t.Fatalf("status %d", e.HTTPStatus)
			}
		})
	}
}

func TestSTKPushNetworkErrors(t *testing.T) {
	// Connection dropped after the request was sent: outcome unknown.
	s := newStub(t)
	s.handle(mpesa.PathSTKPush, func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	})
	if _, err := newClient(t, s).STKPush(context.Background(), pushReq()); !mpesa.IsTransient(err) {
		t.Fatalf("dropped connection: %v", err)
	}

	// Timeout after sending: outcome unknown.
	s2 := newStub(t)
	s2.handle(mpesa.PathSTKPush, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		reply(200, okPush)(w, r)
	})
	c := newClient(t, s2, func(o *mpesa.ClientOptions) { o.Timeout = 200 * time.Millisecond })
	if _, err := c.STKPush(context.Background(), pushReq()); !mpesa.IsTransient(err) {
		t.Fatalf("timeout: %v", err)
	}

	// Nothing listening: the request never left, so no prompt was sent.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	o := opts("http://" + addr + "/")
	dead, err := mpesa.NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.STKPush(context.Background(), pushReq()); !mpesa.IsRejected(err) {
		t.Fatalf("connection refused push: %v", err)
	}
	if _, err := dead.STKQuery(context.Background(), "c"); !mpesa.IsTransient(err) {
		t.Fatalf("connection refused query: %v", err)
	}

	// Token endpoint failing: for a push nothing was sent.
	s3 := newStub(t)
	s3.tokenFn = func(int64) (int, string) {
		return 503, `{"errorCode":"503.001.01","errorMessage":"Service Unavailable"}`
	}
	c3 := newClient(t, s3)
	if _, err := c3.STKPush(context.Background(), pushReq()); !mpesa.IsRejected(err) {
		t.Fatalf("token 503 push: %v", err)
	}
	if _, err := c3.STKQuery(context.Background(), "c"); !mpesa.IsTransient(err) {
		t.Fatalf("token 503 query: %v", err)
	}
	if s3.calls(mpesa.PathSTKPush) != 0 {
		t.Fatal("push sent without a token")
	}
}

func TestSTKQueryClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		state  mpesa.QueryState
		code   string
		kind   mpesa.ErrorKind // non-zero: want an error
	}{
		{"succeeded", 200, okQuery, mpesa.QuerySucceeded, "0", 0},
		{"succeeded numeric", 200, `{"ResponseCode":"0","ResultCode":0,"ResultDesc":"ok"}`, mpesa.QuerySucceeded, "0", 0},
		{"cancelled", 200, `{"ResponseCode":"0","ResultCode":"1032","ResultDesc":"Request cancelled by user"}`, mpesa.QueryFailed, "1032", 0},
		{"unreachable", 200, `{"ResponseCode":"0","ResultCode":"1037","ResultDesc":"DS timeout user cannot be reached"}`, mpesa.QueryFailed, "1037", 0},
		{"4999", 200, `{"ResponseCode":"0","ResultCode":"4999","ResultDesc":"The transaction is still under processing"}`, mpesa.QueryPending, "4999", 0},
		{"being processed", 500, `{"requestId":"r","errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`, mpesa.QueryPending, "500.001.1001", 0},
		{"server error", 500, `{"requestId":"r","errorCode":"500.003.1001","errorMessage":"Internal Server Error"}`, 0, "", mpesa.Transient},
		{"spike arrest", 429, `{}`, 0, "", mpesa.Transient},
		{"bad checkout id", 400, `{"requestId":"r","errorCode":"400.002.02","errorMessage":"Bad Request - Invalid CheckoutRequestID"}`, 0, "", mpesa.Rejected},
		{"no result code", 200, `{"ResponseCode":"1","ResponseDescription":"x"}`, 0, "", mpesa.Transient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.handle(mpesa.PathSTKQuery, reply(tc.status, tc.body))
			r, err := newClient(t, s).STKQuery(context.Background(), "ws_CO_1")
			if tc.kind != 0 {
				var e *mpesa.Error
				if !errors.As(err, &e) || e.Kind != tc.kind {
					t.Fatalf("got %v, want kind %d", err, tc.kind)
				}
				return
			}
			if err != nil || r.State != tc.state || r.ResultCode != tc.code {
				t.Fatalf("got %+v %v", r, err)
			}
		})
	}
}

func TestSTKQueryRequestBody(t *testing.T) {
	s := newStub(t)
	s.handle(mpesa.PathSTKQuery, reply(200, okQuery))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, mpesa.Nairobi)
	c := newClient(t, s, func(o *mpesa.ClientOptions) { o.Now = func() time.Time { return now } })
	if _, err := c.STKQuery(context.Background(), "ws_CO_42"); err != nil {
		t.Fatal(err)
	}
	var b map[string]string
	if err := json.Unmarshal(s.body(mpesa.PathSTKQuery), &b); err != nil {
		t.Fatal(err)
	}
	pw, _ := base64.StdEncoding.DecodeString(b["Password"])
	if b["Timestamp"] != "20260102030405" || string(pw) != testShort+testPasskey+"20260102030405" ||
		b["CheckoutRequestID"] != "ws_CO_42" || b["BusinessShortCode"] != testShort {
		t.Fatalf("%v", b["CheckoutRequestID"])
	}
}

func TestRegisterC2BURLs(t *testing.T) {
	for _, env := range []string{config.MpesaSandbox, config.MpesaProduction} {
		s := newStub(t)
		ok := reply(200, `{"OriginatorCoversationID":"1","ResponseCode":"0","ResponseDescription":"Success"}`)
		s.handle(mpesa.PathRegisterV1, ok)
		s.handle(mpesa.PathRegisterV2, ok)
		c := newClient(t, s, func(o *mpesa.ClientOptions) { o.Env = env })
		if err := c.RegisterC2BURLs(context.Background(), mpesa.RegisterURLsRequest{}); err != nil {
			t.Fatal(err)
		}
		path := mpesa.PathRegisterV1
		if env == config.MpesaProduction {
			path = mpesa.PathRegisterV2
		}
		var b map[string]string
		if err := json.Unmarshal(s.body(path), &b); err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		_, conf, val := c.CallbackURLs()
		if b["ShortCode"] != testShort || b["ResponseType"] != "Completed" || b["ConfirmationURL"] != conf || b["ValidationURL"] != val {
			t.Fatalf("%s: %v", env, b["ShortCode"])
		}
		s.handle(path, reply(200, `{"ResponseCode":"500.003.1001","ResponseDescription":"URLs are already registered"}`))
		if err := c.RegisterC2BURLs(context.Background(), mpesa.RegisterURLsRequest{}); !mpesa.IsRejected(err) {
			t.Fatalf("%s: %v", env, err)
		}
	}
}

// Secrets (consumer key/secret, passkey, callback token, access token, STK
// password) must never reach logs or error strings.
func TestSecretsNeverLeak(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newStub(t)
	n := 0
	s.handle(mpesa.PathSTKPush, func(w http.ResponseWriter, r *http.Request) {
		n++
		switch n {
		case 1:
			reply(200, okPush)(w, r)
		case 2:
			reply(400, `{"errorCode":"400.002.02","errorMessage":"Bad Request - Invalid Amount"}`)(w, r)
		default:
			reply(500, `{"errorCode":"500.003.1001","errorMessage":"Internal Server Error"}`)(w, r)
		}
	})
	s.handle(mpesa.PathSTKQuery, reply(500, `{"errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`))
	s.handle(mpesa.PathRegisterV1, reply(401, `{}`))
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	c := newClient(t, s, func(o *mpesa.ClientOptions) {
		o.Logger = logger
		o.Now = func() time.Time { return now }
	})
	var errs []string
	for range 3 {
		if _, err := c.STKPush(context.Background(), pushReq()); err != nil {
			errs = append(errs, err.Error(), fmt.Sprintf("%+v %#v", err, err))
		}
	}
	if _, err := c.STKQuery(context.Background(), "c"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := c.RegisterC2BURLs(context.Background(), mpesa.RegisterURLsRequest{}); err != nil {
		errs = append(errs, err.Error())
	}
	// Bad credentials path.
	bad, _ := mpesa.NewClient(func() mpesa.ClientOptions {
		o := opts(s.srv.URL + "/")
		o.ConsumerSecret = config.NewSecret("WRONG-" + testSecret)
		o.Logger = logger
		return o
	}())
	if _, err := bad.STKQuery(context.Background(), "c"); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) < 4 {
		t.Fatalf("expected errors, got %v", errs)
	}
	password := base64.StdEncoding.EncodeToString([]byte(testShort + testPasskey + mpesa.Timestamp(now)))
	basic := base64.StdEncoding.EncodeToString([]byte(testKey + ":" + testSecret))
	haystack := logs.String() + "\n" + strings.Join(errs, "\n")
	for name, secret := range map[string]string{
		"consumer key": testKey, "consumer secret": testSecret, "passkey": testPasskey, "callback token": testCBToken,
		"access token": testToken, "stk password": password, "basic auth": basic, "full phone": testPhone,
	} {
		if strings.Contains(haystack, secret) {
			t.Errorf("%s leaked:\n%s", name, haystack)
		}
	}
	if !strings.Contains(logs.String(), `"phone":"2547*****149"`) || !strings.Contains(logs.String(), `"op":"stk_push"`) {
		t.Errorf("expected masked phone and op in logs:\n%s", logs.String())
	}
}

func TestResponseSizeCapped(t *testing.T) {
	s := newStub(t)
	huge := `{"ResponseCode":"0","ResultCode":"0","ResultDesc":"` + strings.Repeat("x", 200<<10) + `"}`
	s.handle(mpesa.PathSTKQuery, reply(200, huge))
	_, err := newClient(t, s).STKQuery(context.Background(), "c")
	if !mpesa.IsTransient(err) { // truncated at 64 KiB: unreadable
		t.Fatalf("%v", err)
	}
}

func TestErrorMessageTrimmed(t *testing.T) {
	s := newStub(t)
	s.handle(mpesa.PathSTKPush, reply(400, `{"errorCode":"400.002.02","errorMessage":"`+strings.Repeat("y", 1000)+`"}`))
	_, err := newClient(t, s).STKPush(context.Background(), pushReq())
	var e *mpesa.Error
	if !errors.As(err, &e) || len(e.Message) > 200 {
		t.Fatalf("%v", err)
	}
}

func TestForbiddenURLWord(t *testing.T) {
	for u, want := range map[string]string{
		"https://pay.talksasa.com/api/cb/hooks/abc/c2b/confirmation": "",
		"https://pay.talksasa.com/api/cb/mpesa/abc/c2b/validation":   "mpesa",
		"https://x/cb/hooks/aBcXdZ/stk":                              "",
		"https://x/cb/hooks/xxCmDyy/stk":                             "cmd",
		"https://SAFARICOM.example/cb/hooks/t/stk":                   "safaricom",
		"https://x/cb/hooks/t/stk?QUERY=1":                           "query",
	} {
		if got := mpesa.ForbiddenURLWord(u); got != want {
			t.Errorf("%s: %q, want %q", u, got, want)
		}
	}
	if w := mpesa.ForbiddenURLWord(mpesa.CallbackPath); w != "" {
		t.Fatalf("CallbackPath contains %q", w)
	}
}
