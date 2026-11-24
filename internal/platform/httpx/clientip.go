package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
)

// ClientIPHeader carries the end user's IP from a trusted front end (the
// Next.js servers or the reverse proxy) to the Go service.
const ClientIPHeader = "X-Talksasa-Client-Ip"

// ClientUAHeader carries the end user's browser user agent the same way.
const ClientUAHeader = "X-Talksasa-Client-Ua"

type clientIPKey struct{}
type clientUAKey struct{}

// ClientIP resolves the caller's IP once per request. The header is honoured
// ONLY when the TCP peer is inside trusted; otherwise anyone could spoof
// their address to dodge rate limits or pollute the audit log.
func ClientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := peerIP(r.RemoteAddr)
			ua := r.UserAgent()
			if ip.IsValid() && inPrefixes(ip, trusted) {
				if fwd, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(ClientIPHeader))); err == nil {
					ip = fwd.Unmap()
				}
				if fwd := r.Header.Get(ClientUAHeader); fwd != "" {
					ua = fwd
				}
			}
			if len(ua) > 300 {
				ua = ua[:300]
			}
			ctx := context.WithValue(r.Context(), clientIPKey{}, ip)
			ctx = context.WithValue(ctx, clientUAKey{}, ua)
			if ip.IsValid() {
				ctx = tlog.WithAttrs(ctx, slog.String("client_ip", ip.String()))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIPFrom returns the resolved client IP (invalid if unknown).
func ClientIPFrom(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(clientIPKey{}).(netip.Addr)
	return ip
}

// ClientUAFrom returns the end user's user agent resolved by ClientIP.
func ClientUAFrom(ctx context.Context) string {
	ua, _ := ctx.Value(clientUAKey{}).(string)
	return ua
}

func peerIP(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

func inPrefixes(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
