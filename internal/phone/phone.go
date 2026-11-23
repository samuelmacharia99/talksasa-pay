// Package phone normalises Kenyan mobile numbers to the 12-digit
// international form M-Pesa uses (2547XXXXXXXX or 2541XXXXXXXX).
package phone

import (
	"errors"
	"strings"
)

// ErrInvalid is returned for anything that is not a Kenyan mobile number.
var ErrInvalid = errors.New("phone: not a valid Kenyan mobile number (expected e.g. 0712345678 or 254712345678)")

// Normalize accepts 07XXXXXXXX, 01XXXXXXXX, 7XXXXXXXX, 1XXXXXXXX,
// +2547XXXXXXXX, 2547XXXXXXXX (and the 2541 equivalents) with optional
// spaces, hyphens, dots or parentheses, and returns 2547XXXXXXXX/2541XXXXXXXX.
func Normalize(raw string) (string, error) {
	var b strings.Builder
	b.Grow(len(raw))
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			// leading plus only
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
			// separators
		default:
			return "", ErrInvalid
		}
	}
	d := b.String()

	var national string // 9 digits starting with 7 or 1
	switch {
	case len(d) == 12 && strings.HasPrefix(d, "254"):
		national = d[3:]
	case len(d) == 10 && d[0] == '0':
		national = d[1:]
	case len(d) == 9:
		national = d
	default:
		return "", ErrInvalid
	}
	if national[0] != '7' && national[0] != '1' {
		return "", ErrInvalid
	}
	return "254" + national, nil
}

// IsNormalized reports whether s is already in canonical form.
func IsNormalized(s string) bool {
	n, err := Normalize(s)
	return err == nil && n == s
}
