package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// The Talksasa Cloud chain: Safaricom -> host nginx (appends $remote_addr
// to X-Forwarded-For) -> edge gateway in the service's Docker network
// (appends its peer, the bridge) -> Go. The edge passes every other header
// through, so a caller-set X-Talksasa-Client-Ip must not count.
func TestClientIPFromForwardingProxies(t *testing.T) {
	private := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	}
	const edge = "10.210.27.5:51234"
	cases := []struct {
		name, peer string
		xff        []string
		talksasa   string
		want       string
	}{
		{"cloud chain", edge, []string{"196.201.214.200, 10.210.27.1"}, "", "196.201.214.200"},
		{"caller-forged left entries are skipped", edge, []string{"196.201.214.206, 1.2.3.4, 10.210.27.1"}, "", "1.2.3.4"},
		{"caller-set talksasa header ignored behind proxies", edge, []string{"1.2.3.4, 10.210.27.1"}, "196.201.214.200", "1.2.3.4"},
		{"several header lines", edge, []string{"9.9.9.9", "1.2.3.4, 10.210.27.1"}, "", "1.2.3.4"},
		{"port on an entry", edge, []string{"1.2.3.4:5555, 10.210.27.1"}, "", "1.2.3.4"},
		{"garbage stops at the last trusted hop", edge, []string{"196.201.214.200, not-an-ip, 10.210.27.1"}, "", "10.210.27.1"},
		{"all hops trusted", edge, []string{"10.0.0.9, 10.210.27.1"}, "", "10.0.0.9"},
		{"untrusted peer: headers ignored", "203.0.113.7:4000", []string{"196.201.214.200"}, "196.201.214.200", "203.0.113.7"},
		{"our Next.js server (no XFF)", "10.210.27.3:4000", nil, "41.90.1.2", "41.90.1.2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got netip.Addr
			h := ClientIP(private)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIPFrom(r.Context()) }))
			r := httptest.NewRequest(http.MethodPost, "/cb/hooks/x/stk", nil)
			r.RemoteAddr = c.peer
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if c.talksasa != "" {
				r.Header.Set(ClientIPHeader, c.talksasa)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got.String() != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}
