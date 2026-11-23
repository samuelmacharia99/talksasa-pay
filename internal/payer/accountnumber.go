package payer

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Account numbers look like HOST-4471: an uppercase product prefix, a hyphen,
// a base number from a per-product counter, and one trailing Damm check digit.
//
// The check digit detects every single-digit typo and every adjacent
// transposition, so a mistyped C2B reference (HOST-4417) does not match some
// other customer; it lands in the suspense queue instead.

// FirstBase is the first counter value issued per product, giving 4-digit
// numbers (base 100 + check digit) for the first 900 payers.
const FirstBase int64 = 100

var (
	prefixRe = regexp.MustCompile(`^[A-Z]{2,6}$`)

	ErrInvalidPrefix = errors.New("payer: prefix must be 2-6 uppercase letters")
	ErrNotAnAccount  = errors.New("payer: not a valid account number")
	ErrBadCheckDigit = errors.New("payer: account number check digit does not match")
)

// ValidPrefix reports whether p is an acceptable product prefix.
func ValidPrefix(p string) bool { return prefixRe.MatchString(p) }

// FormatAccountNumber builds PREFIX-<base><check>.
func FormatAccountNumber(prefix string, base int64) (string, error) {
	if !ValidPrefix(prefix) {
		return "", ErrInvalidPrefix
	}
	if base < FirstBase {
		return "", errors.New("payer: account base below minimum")
	}
	digits := strconv.FormatInt(base, 10)
	return prefix + "-" + digits + string(rune('0'+dammCheck(digits))), nil
}

// ParsedAccount is the canonical form of a typed account reference.
type ParsedAccount struct {
	Prefix     string // HOST
	Normalized string // HOST4471: the lookup key stored in account_number_norm
}

// ParseAccountReference canonicalises what a customer typed on their phone:
// trims, uppercases, strips spaces/hyphens/underscores/dots, splits prefix
// from digits and verifies the check digit. "host 4471", "HOST4471" and
// " Host-4471 " all parse to HOST4471.
func ParseAccountReference(raw string) (ParsedAccount, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch {
		case r == ' ' || r == '-' || r == '_' || r == '.':
			continue
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			return ParsedAccount{}, ErrNotAnAccount
		}
	}
	s := b.String()
	i := strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
	if i < 2 || i > 6 || len(s)-i < 2 {
		return ParsedAccount{}, ErrNotAnAccount
	}
	prefix, digits := s[:i], s[i:]
	if !ValidPrefix(prefix) || strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return ParsedAccount{}, ErrNotAnAccount
	}
	if digits[0] == '0' {
		return ParsedAccount{}, ErrNotAnAccount
	}
	if dammCheck(digits) != 0 {
		return ParsedAccount{}, ErrBadCheckDigit
	}
	return ParsedAccount{Prefix: prefix, Normalized: prefix + digits}, nil
}

// NormalizeAccountNumber returns the lookup key for a generated account number.
func NormalizeAccountNumber(accountNumber string) string {
	return strings.ReplaceAll(strings.ToUpper(accountNumber), "-", "")
}

// dammTable is the standard order-10 weakly totally anti-symmetric quasigroup.
var dammTable = [10][10]uint8{
	{0, 3, 1, 7, 5, 9, 8, 6, 4, 2},
	{7, 0, 9, 2, 1, 5, 4, 8, 6, 3},
	{4, 2, 0, 6, 8, 7, 1, 3, 5, 9},
	{1, 7, 5, 0, 9, 8, 3, 4, 2, 6},
	{6, 1, 2, 3, 0, 4, 5, 9, 7, 8},
	{3, 6, 7, 4, 2, 0, 9, 5, 8, 1},
	{5, 8, 6, 9, 7, 2, 0, 1, 3, 4},
	{8, 9, 4, 5, 3, 6, 2, 0, 1, 7},
	{9, 4, 3, 8, 6, 1, 7, 2, 0, 5},
	{2, 5, 8, 1, 4, 3, 6, 7, 9, 0},
}

// dammCheck returns the interim digit over the string. For a number without a
// check digit it is the check digit to append; for a number including its
// check digit it is 0 when valid.
func dammCheck(digits string) uint8 {
	var interim uint8
	for i := 0; i < len(digits); i++ {
		interim = dammTable[interim][digits[i]-'0']
	}
	return interim
}
