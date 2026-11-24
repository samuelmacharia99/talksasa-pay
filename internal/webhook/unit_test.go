package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/config"
)

// The vectors are shared with the SDKs (PHP etc.): an independent
// implementation must reproduce expected_v1 byte for byte.
func TestSignatureVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/webhook_signature_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vectors []struct {
			Secret     string `json:"secret"`
			T          int64  `json:"t"`
			Body       string `json:"body"`
			ExpectedV1 string `json:"expected_v1"`
			Header     string `json:"header"`
		} `json:"vectors"`
		Rotation struct {
			Current  string `json:"secret_current"`
			Previous string `json:"secret_previous"`
			T        int64  `json:"t"`
			Body     string `json:"body"`
			Header   string `json:"header"`
		} `json:"rotation"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Vectors) < 3 {
		t.Fatal("expected at least 3 vectors")
	}
	for _, v := range f.Vectors {
		if got := Sign([]byte(v.Secret), v.T, []byte(v.Body)); got != v.ExpectedV1 {
			t.Errorf("Sign(%q): got %s want %s", v.Secret, got, v.ExpectedV1)
		}
		if got := SignatureHeader(v.T, []byte(v.Body), []byte(v.Secret)); got != v.Header {
			t.Errorf("header: got %s want %s", got, v.Header)
		}
		at := time.Unix(v.T, 0)
		if !Verify([]byte(v.Secret), v.Header, []byte(v.Body), at.Add(299*time.Second), SignatureTolerance) {
			t.Error("Verify rejected a valid signature")
		}
		if Verify([]byte(v.Secret), v.Header, []byte(v.Body), at.Add(301*time.Second), SignatureTolerance) {
			t.Error("Verify accepted a stale timestamp")
		}
		if Verify([]byte(v.Secret), v.Header, []byte(v.Body+" "), at, SignatureTolerance) {
			t.Error("Verify accepted a modified body")
		}
		if Verify([]byte(v.Secret+"x"), v.Header, []byte(v.Body), at, SignatureTolerance) {
			t.Error("Verify accepted the wrong secret")
		}
	}
	r := f.Rotation
	if got := SignatureHeader(r.T, []byte(r.Body), []byte(r.Current), []byte(r.Previous)); got != r.Header {
		t.Errorf("rotation header: got %s want %s", got, r.Header)
	}
	for _, s := range []string{r.Current, r.Previous} {
		if !Verify([]byte(s), r.Header, []byte(r.Body), time.Unix(r.T, 0), SignatureTolerance) {
			t.Errorf("rotation header must verify with either secret")
		}
	}
	if got := SignatureHeader(r.T, []byte(r.Body), []byte(r.Current), nil); strings.Count(got, "v1=") != 1 {
		t.Errorf("nil previous secret must not add a signature: %s", got)
	}
}

func TestBlockedAddr(t *testing.T) {
	cases := map[string]bool{
		// blocked
		"127.0.0.1": true, "127.255.0.1": true, "10.0.0.1": true, "172.16.5.4": true, "172.31.255.255": true,
		"192.168.1.1": true, "100.64.0.1": true, "100.127.255.254": true, "169.254.169.254": true, "169.254.0.1": true,
		"0.0.0.0": true, "0.1.2.3": true, "224.0.0.1": true, "239.255.255.250": true, "255.255.255.255": true,
		"240.0.0.1": true, "198.18.0.1": true, "192.0.0.8": true, "192.0.2.1": true,
		"::": true, "::1": true, "fe80::1": true, "fc00::1": true, "fd12:3456::1": true, "ff02::1": true,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "::ffff:169.254.169.254": true,
		"64:ff9b::a00:1":     true, // NAT64 of 10.0.0.1
		"64:ff9b::7f00:1":    true, // NAT64 of 127.0.0.1
		"64:ff9b::a9fe:a9fe": true, // NAT64 of 169.254.169.254
		"64:ff9b:1::1":       true, // local-use NAT64
		"2002:a00:1::1":      true, // 6to4 of 10.0.0.1
		"2001:db8::1":        true, "2001:0:4136:e378::1": true, "::127.0.0.1": true, "fec0::1": true,
		// allowed
		"8.8.8.8": false, "1.1.1.1": false, "41.90.64.1": false, "196.201.214.200": false, "100.63.255.255": false,
		"100.128.0.1": false, "172.32.0.1": false, "2a00:1450:4001::1": false, "2606:4700::1111": false,
		"64:ff9b::808:808": false, // NAT64 of 8.8.8.8
		"2002:808:808::1":  false, // 6to4 of 8.8.8.8
		"::ffff:8.8.8.8":   false,
	}
	for s, want := range cases {
		if got := blockedAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("blockedAddr(%s) = %v, want %v", s, got, want)
		}
	}
	if !blockedAddr(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("zoned link-local must be blocked")
	}
}

func TestCheckDial(t *testing.T) {
	if err := checkDial("tcp", "127.0.0.1:80", false); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("loopback allowed without hook: %v", err)
	}
	if err := checkDial("tcp", "127.0.0.1:80", true); err != nil {
		t.Errorf("loopback hook: %v", err)
	}
	if err := checkDial("tcp", "10.0.0.1:80", true); !errors.Is(err, ErrBlockedAddress) {
		t.Error("loopback hook must not open private ranges")
	}
	if err := checkDial("tcp", "[::ffff:169.254.169.254]:80", true); !errors.Is(err, ErrBlockedAddress) {
		t.Error("metadata via mapped address allowed")
	}
	if err := checkDial("udp", "8.8.8.8:53", false); !errors.Is(err, ErrBlockedAddress) {
		t.Error("non-TCP allowed")
	}
	if err := checkDial("tcp", "8.8.8.8:443", false); err != nil {
		t.Errorf("public address blocked: %v", err)
	}
}

func TestNewHTTPClientLoopbackHookOnlyInDevelopment(t *testing.T) {
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProduction, ""} {
		if _, err := NewHTTPClient(ClientOptions{Env: env, AllowLoopback: true}); err == nil {
			t.Errorf("AllowLoopback accepted in %q", env)
		}
	}
	if _, err := NewHTTPClient(ClientOptions{Env: config.EnvDevelopment, AllowLoopback: true}); err != nil {
		t.Fatal(err)
	}
}

// The real client must refuse a loopback receiver (what an attacker-set
// webhook URL pointing at localhost would hit), and with the dev hook must
// still never follow redirects.
func TestHTTPClientBlocksLoopbackAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	prod, err := NewHTTPClient(ClientOptions{Env: config.EnvProduction, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = prod.Post(srv.URL+"/x?token=supersecret", "application/json", strings.NewReader("{}"))
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback reachable in production client: %v", err)
	}
	if msg := describeErr(err); strings.Contains(msg, "supersecret") || strings.Contains(msg, "/x") {
		t.Fatalf("error message leaks the URL: %s", msg)
	}
	// "localhost" resolves after DNS; the dial check still blocks it.
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	if _, err := prod.Get("http://localhost:" + port + "/"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("localhost reachable: %v", err)
	}

	dev, err := NewHTTPClient(ClientOptions{Env: config.EnvDevelopment, AllowLoopback: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := dev.Get(srv.URL + "/redirect")
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("redirect followed: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("want the 302 response alongside the error, got %+v", resp)
	}
	if describeErr(err) != "redirect not followed" {
		t.Fatalf("describeErr: %s", describeErr(err))
	}
}

func TestBackoffSchedule(t *testing.T) {
	var ceilSum time.Duration
	for n := 1; n < 25; n++ {
		c := backoffCeiling(n)
		ceilSum += c
		for range 50 {
			d := Backoff(n)
			if d < time.Second || d > c+time.Second {
				t.Fatalf("Backoff(%d) = %s outside [1s, %s]", n, d, c+time.Second)
			}
		}
	}
	if backoffCeiling(1) != 30*time.Second || backoffCeiling(2) != time.Minute || backoffCeiling(24) != RetryCap || backoffCeiling(1000) != RetryCap {
		t.Fatal("unexpected ceilings")
	}
	// Expected span (mean of full jitter = half the ceiling) is about 3 days.
	mean := ceilSum / 2
	if mean < 60*time.Hour || mean > 84*time.Hour {
		t.Fatalf("expected retry span %s, want ~72h", mean)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	h := func(v string) http.Header { return http.Header{"Retry-After": []string{v}} }
	if d, ok := parseRetryAfter(429, h("120"), now); !ok || d != 2*time.Minute {
		t.Fatalf("seconds: %s %v", d, ok)
	}
	if d, ok := parseRetryAfter(503, h(now.Add(time.Hour).Format(http.TimeFormat)), now); !ok || d != time.Hour {
		t.Fatalf("date: %s %v", d, ok)
	}
	if d, ok := parseRetryAfter(429, h("999999999"), now); !ok || d != MaxRetryAfter {
		t.Fatalf("clamp: %s %v", d, ok)
	}
	if _, ok := parseRetryAfter(500, h("120"), now); ok {
		t.Fatal("Retry-After honoured on 500")
	}
	if _, ok := parseRetryAfter(429, h("soon"), now); ok {
		t.Fatal("garbage accepted")
	}
	if _, ok := parseRetryAfter(429, h("-5"), now); ok {
		t.Fatal("negative accepted")
	}
}

func TestBreaker(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	ep := newEndpoints(3, time.Minute, 2, func() time.Time { return now })
	id := uuid.New()

	// Concurrency limit.
	a1, _, t1 := ep.acquire(id)
	a2, _, t2 := ep.acquire(id)
	a3, _, _ := ep.acquire(id)
	if a1 != admitted || a2 != admitted || a3 != rejectedBusy {
		t.Fatalf("limit: %v %v %v", a1, a2, a3)
	}
	ep.release(t1, failed)
	ep.release(t2, failed)
	if ep.open(id) {
		t.Fatal("opened before threshold")
	}
	_, _, t3 := ep.acquire(id)
	ep.release(t3, failed)
	if !ep.open(id) {
		t.Fatal("not open after 3 consecutive failures")
	}
	if adm, wait, _ := ep.acquire(id); adm != rejectedOpen || wait != time.Minute {
		t.Fatalf("open: %v %s", adm, wait)
	}
	// Half-open: one probe only.
	now = now.Add(time.Minute)
	pa, _, probe := ep.acquire(id)
	pb, _, _ := ep.acquire(id)
	if pa != admitted || !probe.probe || pb != rejectedOpen {
		t.Fatalf("half-open: %v %v %v", pa, probe.probe, pb)
	}
	ep.release(probe, failed)
	if !ep.open(id) {
		t.Fatal("failed probe must re-open")
	}
	now = now.Add(time.Minute)
	_, _, probe = ep.acquire(id)
	ep.release(probe, succeeded)
	if ep.open(id) {
		t.Fatal("successful probe must close")
	}
	if a, _, tk := ep.acquire(id); a != admitted || tk.probe {
		t.Fatal("closed breaker should admit normally")
	} else {
		ep.release(tk, noVerdict)
	}
	if len(ep.m) != 0 {
		t.Fatalf("idle healthy endpoints should be forgotten: %d", len(ep.m))
	}
}

func TestSanitizeBody(t *testing.T) {
	in := []byte("ok\x00\x1b[31m\r\nline2\t\xff" + strings.Repeat("é", 2000))
	s := sanitizeBody(in)
	if s == nil || !utf8.ValidString(*s) || strings.ContainsAny(*s, "\x00\x1b\r") {
		t.Fatalf("not sanitized: %q", *s)
	}
	if n := utf8.RuneCountInString(*s); n != maxStoredBody {
		t.Fatalf("length %d", n)
	}
	if sanitizeBody(nil) != nil || sanitizeBody([]byte("\x00")) != nil {
		t.Fatal("empty bodies must be nil")
	}
}

func TestDescribeErr(t *testing.T) {
	ue := &url.Error{Op: "Post", URL: "https://hooks.example.com/p?key=abc", Err: context.DeadlineExceeded}
	if got := describeErr(ue); got != "timeout" {
		t.Fatalf("got %q", got)
	}
}
