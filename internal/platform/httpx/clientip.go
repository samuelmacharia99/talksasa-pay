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

// ClientIP resolves the caller's IP once per request. Forwarding headers are
// honoured ONLY when the TCP peer is inside trusted; otherwise anyone could
// spoof their address to dodge rate limits, pollute the audit log or pass
// the M-Pesa callback IP check.
//
// From a trusted peer:
//   - X-Forwarded-For present: the request came through forwarding proxies
//     (e.g. Talksasa Cloud's host nginx, then its edge gateway, which pass
//     every other header through untouched). It is read right to left,
//     skipping trusted proxy hops; the first address that is not a trusted
//     proxy is the one the outermost proxy saw. Anything the caller wrote
//     into the header sits to the left of that and is never reached, and
//     X-Talksasa-Client-Ip is ignored (the caller could have set it).
//   - otherwise X-Talksasa-Client-Ip, which only our own Next.js servers
//     and Caddy (deploy/Caddyfile strips X-Forwarded-For) send.
func ClientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := peerIP(r.RemoteAddr)
			ua := r.UserAgent()
			if ip.IsValid() && inPrefixes(ip, trusted) {
				if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
					ip = forwardedClient(ip, xff, trusted)
				} else if fwd, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(ClientIPHeader))); err == nil {
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

// forwardedClient walks X-Forwarded-For (possibly split over several header
// lines) from the right, starting at the trusted peer, and returns the first
// hop that is not a trusted proxy. A malformed entry stops the walk at the
// last trusted hop, so garbage can never be chosen.
func forwardedClient(peer netip.Addr, values []string, trusted []netip.Prefix) netip.Addr {
	var hops []string
	for _, v := range values {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		a, err := netip.ParseAddr(h)
		if err != nil {
			if ap, perr := netip.ParseAddrPort(h); perr == nil {
				a = ap.Addr()
			} else {
				return client
			}
		}
		client = a.Unmap()
		if !inPrefixes(client, trusted) {
			return client
		}
	}
	return client
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
