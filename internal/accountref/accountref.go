// Package accountref parses and generates paybill account numbers.
//
// Every app owns an account prefix (2-6 uppercase letters). A charge's
// account number is the prefix, a hyphen, a digit, then letters and digits,
// at most 12 characters in total (Daraja's AccountReference limit), e.g.
// HOST-1042. Paybill (C2B) payments are routed to an app by the prefix of
// what the customer typed (money rule 7).
//
// Account numbers the gateway generates are PREFIX-, a digit, random
// Crockford base32 characters and one check character, padded to exactly 12
// characters. The check character (a Damm-style check over GF(32)) detects
// every single-character typo and every adjacent transposition.
package accountref

import (
	"crypto/rand"
	"errors"
	"math/big"
	"regexp"
	"strings"
)

// MaxLen is the longest account number (Daraja AccountReference limit).
const MaxLen = 12

// crockford is the Crockford base32 alphabet (no I, L, O or U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	prefixRe  = regexp.MustCompile(`^[A-Z]{2,6}$`)
	accountRe = regexp.MustCompile(`^([A-Z]{2,6})-[0-9][A-Z0-9]*$`)

	ErrInvalidPrefix = errors.New("accountref: prefix must be 2-6 uppercase letters")
	ErrNotAnAccount  = errors.New("accountref: not a valid account number")
	ErrWrongPrefix   = errors.New("accountref: account number does not start with the app's prefix")
	ErrTooLong       = errors.New("accountref: account number is longer than 12 characters")
)

// ValidPrefix reports whether p is an acceptable app prefix.
func ValidPrefix(p string) bool { return prefixRe.MatchString(p) }

// Normalize is the lookup key for what a customer typed (and the value of
// charges.account_number_norm): spaces, hyphens, underscores and dots are
// removed and letters uppercased. "host 1042", "HOST1042" and " Host-1042 "
// all normalize to HOST1042. Other characters are kept, so they never match.
func Normalize(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch r {
		case ' ', '-', '_', '.', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Parsed is the canonical form of a typed account reference.
type Parsed struct {
	Prefix     string // HOST
	Normalized string // HOST1042
}

// Parse normalizes raw and splits off the app prefix: the leading 2-6
// letters, which must be followed by a digit. Only letters and digits may
// remain after normalizing.
func Parse(raw string) (Parsed, error) {
	n := Normalize(raw)
	if n == "" || strings.ContainsFunc(n, func(r rune) bool { return !isUpperAlnum(r) }) {
		return Parsed{}, ErrNotAnAccount
	}
	i := strings.IndexFunc(n, func(r rune) bool { return r < 'A' || r > 'Z' })
	if i < 2 || i > 6 || n[i] < '0' || n[i] > '9' {
		return Parsed{}, ErrNotAnAccount
	}
	return Parsed{Prefix: n[:i], Normalized: n}, nil
}

// Validate checks an account number an app supplied for one of its charges:
// it must be exactly PREFIX-, a digit, then uppercase letters and digits
// (one hyphen only), at most 12 characters. Callers trim and uppercase
// first if they want to be lenient.
func Validate(prefix, accountNumber string) error {
	if !ValidPrefix(prefix) {
		return ErrInvalidPrefix
	}
	if len(accountNumber) > MaxLen {
		return ErrTooLong
	}
	m := accountRe.FindStringSubmatch(accountNumber)
	if m == nil {
		return ErrNotAnAccount
	}
	if m[1] != prefix {
		return ErrWrongPrefix
	}
	return nil
}

// Generate returns a fresh random account number for prefix, exactly 12
// characters long, e.g. HOST-4K7QZ9M (prefix, hyphen, digit, random
// Crockford characters, check character).
func Generate(prefix string) (string, error) {
	if !ValidPrefix(prefix) {
		return "", ErrInvalidPrefix
	}
	bodyLen := MaxLen - len(prefix) - 1 // >= 5
	body := make([]byte, 0, bodyLen)
	d, err := randInt(9)
	if err != nil {
		return "", err
	}
	body = append(body, byte('1'+d))
	for len(body) < bodyLen-1 {
		v, err := randInt(32)
		if err != nil {
			return "", err
		}
		body = append(body, crockford[v])
	}
	body = append(body, crockford[checkValue(string(body))])
	return prefix + "-" + string(body), nil
}

// ValidCheck reports whether accountNumber (in any typed form) is a
// generated account number of prefix with a correct check character.
// App-supplied numbers have no check character, so this is only meaningful
// for generated ones. The prefix is given, not parsed: a typo that moves a
// letter next to the prefix would otherwise change the parsed prefix.
func ValidCheck(prefix, accountNumber string) bool {
	n := Normalize(accountNumber)
	body, ok := strings.CutPrefix(n, prefix)
	if !ok || !ValidPrefix(prefix) || len(body) < 2 {
		return false
	}
	interim := uint8(0)
	for i := 0; i < len(body); i++ {
		v := strings.IndexByte(crockford, body[i])
		if v < 0 {
			return false
		}
		interim = damm(interim, uint8(v))
	}
	return interim == 0
}

// checkValue is the check character's value for body (without it).
func checkValue(body string) uint8 {
	interim := uint8(0)
	for i := 0; i < len(body); i++ {
		interim = damm(interim, uint8(strings.IndexByte(crockford, body[i])))
	}
	// damm(interim, c) = 2*interim + c = 0 in GF(32)  =>  c = 2*interim.
	return gfMul(2, interim)
}

// damm is the quasigroup x∘y = 2·x + y over GF(32). Because 2 ≠ 1 it is
// weakly totally anti-symmetric, which is what catches transpositions.
func damm(x, y uint8) uint8 { return gfMul(2, x) ^ y }

// gfMul multiplies in GF(2^5) modulo x^5 + x^2 + 1.
func gfMul(a, b uint8) uint8 {
	var p uint8
	for b > 0 {
		if b&1 != 0 {
			p ^= a
		}
		b >>= 1
		a <<= 1
		if a&0x20 != 0 {
			a ^= 0x25
		}
	}
	return p & 0x1f
}

func isUpperAlnum(r rune) bool { return (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') }

func randInt(n int64) (int64, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, err
	}
	return v.Int64(), nil
}
