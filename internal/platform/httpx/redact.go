package httpx

import (
	"net/netip"
	"strings"
)

// InPrefixes reports whether ip is inside any of prefixes (false for an
// invalid ip or an empty list). Use it for source allow-lists, e.g.
// Safaricom's callback networks; callers decide what an empty list means.
func InPrefixes(ip netip.Addr, prefixes []netip.Prefix) bool {
	return ip.IsValid() && inPrefixes(ip.Unmap(), prefixes)
}

// Redacted replaces secret path segments in logs.
const Redacted = "[redacted]"

// PathRedactor turns a request path into one that is safe to log.
type PathRedactor func(path string) string

// RedactSegments builds a PathRedactor from patterns such as
// "/cb/mpesa/*" or "/cb/**":
//
//   - literal segments match case-insensitively;
//   - "*" matches one segment and replaces it with [redacted];
//   - a trailing "**" matches the rest of the path (one or more segments)
//     and replaces it with a single [redacted].
//
// Empty segments are dropped before matching, so "/cb/mpesa//TOKEN" is
// still redacted. The first matching pattern wins; a path no pattern
// matches is returned unchanged. The query string is never part of the
// input (pass URL.Path).
func RedactSegments(patterns ...string) PathRedactor {
	compiled := make([][]string, 0, len(patterns))
	for _, p := range patterns {
		compiled = append(compiled, strings.Split(strings.TrimPrefix(p, "/"), "/"))
	}
	return func(path string) string {
		segs := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
		if len(segs) == 0 {
			return path
		}
		for _, pat := range compiled {
			if out, ok := redactMatch(pat, segs); ok {
				return "/" + strings.Join(out, "/")
			}
		}
		return path
	}
}

func redactMatch(pat, segs []string) ([]string, bool) {
	out := make([]string, 0, len(segs))
	for i, p := range pat {
		if p == "**" && i == len(pat)-1 {
			if i >= len(segs) {
				return nil, false
			}
			return append(out, Redacted), true
		}
		if i >= len(segs) {
			// The path ended inside the pattern: nothing secret follows.
			return out, true
		}
		switch {
		case p == "*":
			out = append(out, Redacted)
		case strings.EqualFold(p, segs[i]):
			out = append(out, segs[i])
		default:
			return nil, false
		}
	}
	// Pattern consumed; keep the remaining (non-secret) segments.
	return append(out, segs[len(pat):]...), true
}
