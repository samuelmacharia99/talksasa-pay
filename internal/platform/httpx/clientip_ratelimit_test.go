package httpx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientIPTrustsHeaderOnlyFromTrustedPeers(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	var got netip.Addr
	h := ClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIPFrom(r.Context()) }))
	cases := []struct {
		peer, header, want string
	}{
		{"10.1.2.3:5000", "203.0.113.9", "203.0.113.9"},      // trusted proxy: header used
		{"198.51.100.7:5000", "203.0.113.9", "198.51.100.7"}, // untrusted: header ignored (spoof attempt)
		{"10.1.2.3:5000", "not-an-ip", "10.1.2.3"},           // garbage header: peer used
		{"10.1.2.3:5000", "", "10.1.2.3"},
		{"[::ffff:10.1.2.3]:5000", "2001:db8::1", "2001:db8::1"}, // v4-mapped peer is unmapped
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.peer
		if c.header != "" {
			r.Header.Set(ClientIPHeader, c.header)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got.String() != c.want {
			t.Errorf("peer=%s header=%q: got %s want %s", c.peer, c.header, got, c.want)
		}
	}
}

func TestRateLimiterBurstRefillAndIsolation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewRateLimiter(60, 3) // 1 token/s, burst 3
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("burst request %d denied", i)
		}
	}
	ok, retry := l.Allow("a")
	if ok || retry <= 0 || retry > time.Second {
		t.Fatalf("4th request: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("other key affected")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("token not refilled")
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	l := NewRateLimiter(60, 1)
	l.maxKeys = 10
	for i := 0; i < 1000; i++ {
		l.Allow(fmt.Sprint("k", i))
	}
	if len(l.buckets) > 10 {
		t.Fatalf("buckets grew to %d", len(l.buckets))
	}
}

func TestLimitByIPWrites429(t *testing.T) {
	l := NewRateLimiter(60, 1)
	h := ClientIP(nil)(LimitByIP(l, "t")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})))
	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "198.51.100.7:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := do()
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d retry-after=%q", w.Code, w.Header().Get("Retry-After"))
	}
}
