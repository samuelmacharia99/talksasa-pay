// Package money defines the integer Kenya Shilling amount type used across the
// service. Floats are never used for money: M-Pesa settles in whole shillings
// and every amount is stored as a bigint of KES.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// KES is an amount in whole Kenya Shillings.
type KES int64

// MaxKES bounds any single amount we accept. It is far above any M-Pesa
// transaction limit and keeps sums of many amounts far from int64 overflow.
const MaxKES KES = 1_000_000_000_000

var (
	ErrInvalidAmount  = errors.New("money: invalid amount")
	ErrFractional     = errors.New("money: fractional shillings are not supported")
	ErrNonPositive    = errors.New("money: amount must be greater than zero")
	ErrAmountTooLarge = errors.New("money: amount exceeds the maximum")
)

// Parse parses a decimal string strictly. "100", "100.0" and "100.00" are
// accepted; "100.50", "1e3", "-5", " 100", "100,000" and "" are rejected.
func Parse(s string) (KES, error) {
	if s == "" {
		return 0, ErrInvalidAmount
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) {
		return 0, ErrInvalidAmount
	}
	if hasFrac {
		if frac == "" || len(frac) > 2 || !allDigits(frac) {
			return 0, ErrInvalidAmount
		}
		if strings.Trim(frac, "0") != "" {
			return 0, ErrFractional
		}
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrAmountTooLarge
	}
	return checkRange(KES(n))
}

// FromFloat64 converts a JSON number decoded as float64 (as some providers
// send). It rejects anything that is not an exact whole number.
func FromFloat64(f float64) (KES, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, ErrInvalidAmount
	}
	if f != math.Trunc(f) {
		return 0, ErrFractional
	}
	if f < 0 || f > float64(MaxKES) {
		return 0, ErrAmountTooLarge
	}
	return checkRange(KES(int64(f)))
}

// Positive validates an amount supplied by a caller.
func Positive(n int64) (KES, error) {
	if n <= 0 {
		return 0, ErrNonPositive
	}
	return checkRange(KES(n))
}

func checkRange(k KES) (KES, error) {
	if k > MaxKES {
		return 0, ErrAmountTooLarge
	}
	if k < 0 {
		return 0, ErrInvalidAmount
	}
	return k, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Int64 returns the raw value for storage.
func (k KES) Int64() int64 { return int64(k) }

// String renders "KES 1,234" (negative: "KES -1,234").
func (k KES) String() string {
	n := int64(k)
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return fmt.Sprintf("KES %s%s", sign, b.String())
}

// Min returns the smaller of a and b.
func Min(a, b KES) KES {
	if a < b {
		return a
	}
	return b
}
