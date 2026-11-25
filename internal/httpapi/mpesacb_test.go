package httpapi_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const (
	cbToken     = "cbTOKEN_0123456789abcdefghijklmnopqrstuvwxyz"
	safaricomIP = "196.201.214.200"
	proxyAddr   = "10.0.0.5:5555" // Caddy, a trusted proxy
)

var safaricomNet = []netip.Prefix{netip.MustParsePrefix("196.201.214.0/24")}

type cbEnv struct {
	h    http.Handler
	stk  *fakeSTK
	logs *syncBuffer
}

func newCBEnv(t *testing.T, mutate func(*httpapi.Deps)) *cbEnv {
	t.Helper()
	logs := &syncBuffer{}
	f := newFakeSTK()
	d := httpapi.Deps{
		Logger: tlog.New(logs, tlog.Options{Level: slog.LevelDebug}), MaxBodyBytes: 1 << 20,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		STK:            f, MpesaCallbackToken: config.NewSecret(cbToken), MpesaCallbackCIDRs: safaricomNet,
	}
	if mutate != nil {
		mutate(&d)
	}
	e := &cbEnv{h: httpapi.NewRouter(d), stk: f, logs: logs}
	t.Cleanup(func() {
		if strings.Contains(logs.String(), cbToken) || strings.Contains(logs.String(), cbToken[:20]) {
			t.Errorf("logs contain the callback token:\n%s", logs.String())
		}
	})
	return e
}

// fromSafaricom sends through Caddy with Safaricom's address.
func fromSafaricom() reqOpt {
	return func(r *http.Request) {
		r.RemoteAddr = proxyAddr
		r.Header.Set(httpx.ClientIPHeader, safaricomIP)
	}
}

func (e *cbEnv) post(path, body string, opts ...reqOpt) result {
	return serve(e.h, http.MethodPost, path, strings.NewReader(body), opts...)
}

const stkBody = `{"Body":{"stkCallback":{"MerchantRequestID":"m1","CheckoutRequestID":"ws_CO_1","ResultCode":0,"ResultDesc":"ok"}}}`

func expectAck(t *testing.T, r result) {
	t.Helper()
	if r.code != 200 || r.hdr.Get("Content-Type") != "application/json" ||
		strings.TrimSpace(r.body) != `{"ResultCode":0,"ResultDesc":"Accepted"}` {
		t.Fatalf("ack: %d %s %q", r.code, r.hdr.Get("Content-Type"), r.body)
	}
}

func TestCallbacksStoreAndAck(t *testing.T) {
	e := newCBEnv(t, nil)
	expectAck(t, e.post("/cb/mpesa/"+cbToken+"/stk", stkBody, fromSafaricom()))
	c2b := `{"TransactionType":"Pay Bill","TransID":"RKT1","TransAmount":"100.00","BillRefNumber":"HOST-1","MSISDN":"2547***"}`
	expectAck(t, e.post("/cb/mpesa/"+cbToken+"/c2b/confirmation", c2b, fromSafaricom()))
	// Validation always accepts and stores nothing.
	expectAck(t, e.post("/cb/mpesa/"+cbToken+"/c2b/validation", c2b, fromSafaricom()))

	got := e.stk.storedCallbacks()
	if len(got) != 2 {
		t.Fatalf("stored %d callbacks", len(got))
	}
	if got[0].kind != store.MpesaCallbackKindStk || string(got[0].raw) != stkBody || got[0].ip != netip.MustParseAddr(safaricomIP) {
		t.Fatalf("stk callback: %+v", got[0])
	}
	if got[1].kind != store.MpesaCallbackKindC2bConfirmation || string(got[1].raw) != c2b {
		t.Fatalf("c2b callback: %+v %s", got[1].kind, got[1].raw)
	}

	// The access log shows the redacted path, never the token.
	var paths []string
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "http request" {
			paths = append(paths, l["path"].(string))
		}
	}
	want := []string{"/cb/mpesa/[redacted]/stk", "/cb/mpesa/[redacted]/c2b/confirmation", "/cb/mpesa/[redacted]/c2b/validation"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("access log paths %v", paths)
	}
}

// A wrong token or source address is indistinguishable from an unknown
// path, and raises an alarm without logging the token.
func TestCallbacksRejectBadTokenAndIP(t *testing.T) {
	e := newCBEnv(t, nil)
	unknown := e.post("/no/such/path", stkBody, fromSafaricom())
	unknown.expect(t, 404, "not_found")
	base := unknown.json(t)

	wrongToken := strings.Replace(cbToken, "0", "1", 1)
	cases := []struct {
		name, path, reason, kind string
		opts                     []reqOpt
	}{
		{"wrong token", "/cb/mpesa/" + wrongToken + "/stk", "bad_token", "stk", []reqOpt{fromSafaricom()}},
		{"token prefix", "/cb/mpesa/" + cbToken[:len(cbToken)-1] + "/c2b/confirmation", "bad_token", "c2b_confirmation", []reqOpt{fromSafaricom()}},
		{"token too long", "/cb/mpesa/" + cbToken + "x/stk", "bad_token", "stk", []reqOpt{fromSafaricom()}},
		{"wrong token unknown subpath", "/cb/mpesa/" + wrongToken + "/zzz", "bad_token", "unknown", []reqOpt{fromSafaricom()}},
		{"outside cidrs", "/cb/mpesa/" + cbToken + "/stk", "ip_not_allowed", "stk", []reqOpt{fromAddr("203.0.113.9:1234")}},
		{"spoofed header from untrusted peer", "/cb/mpesa/" + cbToken + "/stk", "ip_not_allowed", "stk",
			[]reqOpt{fromAddr("203.0.113.9:1234"), withHeader(httpx.ClientIPHeader, safaricomIP)}},
		{"validation outside cidrs", "/cb/mpesa/" + cbToken + "/c2b/validation", "ip_not_allowed", "c2b_validation", []reqOpt{fromAddr("203.0.113.9:1234")}},
	}
	for _, tc := range cases {
		before := len(e.logs.lines(t))
		r := e.post(tc.path, stkBody, tc.opts...)
		m := r.expect(t, 404, "not_found")
		if m["detail"] != base["detail"] || m["title"] != base["title"] || m["type"] != base["type"] {
			t.Errorf("%s: distinguishable from an unknown path: %s", tc.name, r.body)
		}
		var alarm map[string]any
		for _, l := range e.logs.lines(t)[before:] {
			if l["msg"] == "mpesa callback rejected" {
				alarm = l
			}
		}
		if alarm == nil || alarm["alarm"] != true || alarm["level"] != "WARN" || alarm["reason"] != tc.reason || alarm["kind"] != tc.kind || alarm["remote_ip"] == nil {
			t.Errorf("%s: alarm log %v", tc.name, alarm)
		}
	}
	if n := len(e.stk.storedCallbacks()); n != 0 {
		t.Fatalf("stored %d rejected callbacks", n)
	}
	if strings.Contains(e.logs.String(), wrongToken) {
		t.Fatal("logs contain the presented token")
	}
}

func TestCallbacksOnlyPOSTOnKnownPaths(t *testing.T) {
	e := newCBEnv(t, nil)
	serve(e.h, http.MethodGet, "/cb/mpesa/"+cbToken+"/stk", nil, fromSafaricom()).expect(t, 404, "not_found")
	e.post("/cb/mpesa/"+cbToken+"/other", stkBody, fromSafaricom()).expect(t, 404, "not_found")
	e.post("/cb/mpesa/"+cbToken, stkBody, fromSafaricom()).expect(t, 404, "not_found")
	if len(e.stk.storedCallbacks()) != 0 {
		t.Fatal("stored")
	}
}

func TestCallbacksAnySourceWhenNoCIDRs(t *testing.T) {
	e := newCBEnv(t, func(d *httpapi.Deps) { d.MpesaCallbackCIDRs = nil })
	expectAck(t, e.post("/cb/mpesa/"+cbToken+"/stk", stkBody, fromAddr("203.0.113.9:1234")))
	if got := e.stk.storedCallbacks(); len(got) != 1 || got[0].ip != netip.MustParseAddr("203.0.113.9") {
		t.Fatal(got)
	}
}

func TestCallbacksNoTokenConfiguredRejectsAll(t *testing.T) {
	e := newCBEnv(t, func(d *httpapi.Deps) { d.MpesaCallbackToken = config.Secret{} })
	e.post("/cb/mpesa//stk", stkBody, fromSafaricom()).expect(t, 404, "not_found")
	e.post("/cb/mpesa/x/stk", stkBody, fromSafaricom()).expect(t, 404, "not_found")
	if len(e.stk.storedCallbacks()) != 0 {
		t.Fatal("stored")
	}
}

func TestCallbacksNotMountedWhenMpesaOff(t *testing.T) {
	for name, mutate := range map[string]func(*httpapi.Deps){
		"nil STK":     func(d *httpapi.Deps) { d.STK = nil },
		"unavailable": func(d *httpapi.Deps) { d.STK.(*fakeSTK).available = false },
	} {
		t.Run(name, func(t *testing.T) {
			e := newCBEnv(t, mutate)
			r := e.post("/cb/mpesa/"+cbToken+"/stk", stkBody, fromSafaricom())
			r.expect(t, 404, "not_found")
			if len(e.stk.storedCallbacks()) != 0 || strings.Contains(e.logs.String(), "mpesa callback") {
				t.Fatal("callback handled while M-Pesa is off")
			}
		})
	}
}

func TestCallbacksBodyRules(t *testing.T) {
	e := newCBEnv(t, nil)
	path := "/cb/mpesa/" + cbToken + "/stk"
	big := `{"pad":"` + strings.Repeat("a", httpapi.MaxCallbackBody) + `"}`
	e.post(path, big, fromSafaricom()).expect(t, 413, "payload_too_large")
	// Without Content-Length (chunked): the reader limit still applies.
	serve(e.h, http.MethodPost, path, io.MultiReader(strings.NewReader(big)), fromSafaricom(), func(r *http.Request) { r.ContentLength = -1 }).
		expect(t, 413, "payload_too_large")
	for _, body := range []string{"", "   ", "[1,2]", `"str"`, "not json", `{"a":1`, `{"a":1} trailing`} {
		e.post(path, body, fromSafaricom()).expect(t, 400, "invalid_json")
	}
	// Exactly at the limit is fine.
	exact := `{"pad":"` + strings.Repeat("a", httpapi.MaxCallbackBody-10) + `"}`
	expectAck(t, e.post(path, exact, fromSafaricom()))
	if n := len(e.stk.storedCallbacks()); n != 1 {
		t.Fatalf("stored %d", n)
	}
}

func TestCallbacksStoreFailureIs500(t *testing.T) {
	e := newCBEnv(t, nil)
	e.stk.storeErr = errors.New("db down")
	r := e.post("/cb/mpesa/"+cbToken+"/stk", stkBody, fromSafaricom())
	r.expect(t, 500, "internal_error")
	if strings.Contains(r.body, "db down") || strings.Contains(r.body, "ResultCode") {
		t.Fatalf("body %s", r.body)
	}
	var found bool
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "mpesa callback not stored" {
			found = l["alarm"] == true && l["path"] == "/cb/mpesa/[redacted]/stk"
		}
	}
	if !found {
		t.Fatalf("no alarm for the storage failure:\n%s", e.logs.String())
	}
}

func TestCallbacksRateLimitedPerIP(t *testing.T) {
	e := newCBEnv(t, func(d *httpapi.Deps) { d.MpesaCallbackLimiter = httpx.NewRateLimiter(60, 2) })
	path := "/cb/mpesa/" + cbToken + "/stk"
	expectAck(t, e.post(path, stkBody, fromSafaricom()))
	expectAck(t, e.post(path, stkBody, fromSafaricom()))
	e.post(path, stkBody, fromSafaricom()).expect(t, 429, "rate_limited")
	// Another source has its own bucket (here rejected by the allow-list).
	e.post(path, stkBody, fromAddr("203.0.113.9:1")).expect(t, 404, "not_found")
}

// Callbacks never go through app-API authentication.
func TestCallbacksNeedNoAPIKey(t *testing.T) {
	e := newCBEnv(t, nil)
	expectAck(t, e.post("/cb/mpesa/"+cbToken+"/stk", stkBody, fromSafaricom(), withHeader("Authorization", "Bearer junk")))
}
