package webhook

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/talksasa/talksasa-pay/internal/config"
)

// ErrBlockedAddress means the webhook host resolved to an address we refuse
// to connect to (loopback, private, link-local, metadata, ...).
var ErrBlockedAddress = errors.New("webhook: destination address is not allowed")

// ErrRedirect is returned for any 3xx: redirects are never followed (a
// redirect could point the signed request at an internal address).
var ErrRedirect = errors.New("webhook: redirects are not followed")

// MaxResponseBytes caps how much of a receiver's response is read.
const MaxResponseBytes = 64 << 10

// ClientOptions configures the outbound HTTP client.
type ClientOptions struct {
	// Env is the deployment environment (cfg.Env).
	Env config.Environment
	// Timeout is the total per-attempt timeout (cfg.Worker.WebhookTimeout).
	// Zero means 10 s.
	Timeout time.Duration
	// AllowLoopback lets the client reach 127.0.0.0/8 and ::1 so tests and
	// local development can use httptest receivers. NewHTTPClient refuses it
	// unless Env is development. Set only when cfg.Env == development.
	AllowLoopback bool
}

// NewHTTPClient builds the SSRF-safe client every delivery uses:
//   - the dialer checks the actual IP being dialled (after DNS resolution, so
//     DNS rebinding cannot sneak past) against blockedAddr;
//   - no proxy from the environment; no redirects; TLS 1.2+;
//   - a total timeout per attempt.
func NewHTTPClient(o ClientOptions) (*http.Client, error) {
	if o.AllowLoopback && o.Env != config.EnvDevelopment {
		return nil, fmt.Errorf("webhook: AllowLoopback is only permitted in development (env %q)", o.Env)
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	allowLoopback := o.AllowLoopback
	dialer := &net.Dialer{
		Timeout:   o.Timeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			return checkDial(network, address, allowLoopback)
		},
	}
	tr := &http.Transport{
		Proxy:                  nil, // never honour HTTP(S)_PROXY: it would bypass the dial check
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           200,
		MaxIdleConnsPerHost:    10,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    o.Timeout,
		ResponseHeaderTimeout:  o.Timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirect
		},
	}, nil
}

// checkDial runs in net.Dialer.Control with the resolved "ip:port".
func checkDial(network, address string, allowLoopback bool) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return ErrBlockedAddress
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return ErrBlockedAddress
	}
	if allowLoopback && ap.Addr().Unmap().IsLoopback() {
		return nil
	}
	if blockedAddr(ap.Addr()) {
		return ErrBlockedAddress
	}
	return nil
}

// Ranges that are never valid webhook destinations, beyond what netip's
// Is* helpers cover.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // carrier-grade NAT (RFC 6598)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, incl. broadcast
		"::/96",           // IPv4-compatible (deprecated)
		"64:ff9b:1::/48",  // local-use NAT64 (RFC 8215)
		"100::/64",        // discard-only
		"2001::/32",       // Teredo
		"2001:db8::/32",   // documentation
		"fec0::/10",       // site-local (deprecated)
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96") // well-known NAT64: embeds an IPv4
	sixToFour = netip.MustParsePrefix("2002::/16")    // 6to4: embeds an IPv4 in bits 16-48
)

// blockedAddr reports whether ip is loopback, private (RFC 1918, IPv6 ULA),
// CGNAT, link-local (incl. the 169.254.169.254 metadata service),
// multicast, unspecified, reserved, or an IPv6 form embedding such an IPv4.
func blockedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.WithZone("")
	if ip.Is4In6() {
		return blockedAddr(ip.Unmap())
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case nat64.Contains(ip):
			return blockedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		case sixToFour.Contains(ip):
			return blockedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return false
}
