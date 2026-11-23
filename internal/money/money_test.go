package money

import (
	"errors"
	"math"
	"testing"
)

func TestParse(t *testing.T) {
	ok := map[string]KES{"0": 0, "1": 1, "100": 100, "100.0": 100, "100.00": 100, "1000000": 1_000_000}
	for in, want := range ok {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	bad := map[string]error{
		"": ErrInvalidAmount, "-5": ErrInvalidAmount, " 100": ErrInvalidAmount, "100,000": ErrInvalidAmount,
		"1e3": ErrInvalidAmount, "100.": ErrInvalidAmount, ".5": ErrInvalidAmount, "100.000": ErrInvalidAmount,
		"100.50": ErrFractional, "0.01": ErrFractional, "99999999999999999999": ErrAmountTooLarge,
		"1000000000001": ErrAmountTooLarge,
	}
	for in, want := range bad {
		if _, err := Parse(in); !errors.Is(err, want) {
			t.Errorf("Parse(%q) err = %v, want %v", in, err, want)
		}
	}
}

func TestFromFloat64(t *testing.T) {
	if k, err := FromFloat64(1500); err != nil || k != 1500 {
		t.Fatalf("1500: %d %v", k, err)
	}
	for _, f := range []float64{1.5, 0.1, -1, math.NaN(), math.Inf(1), 1e13} {
		if _, err := FromFloat64(f); err == nil {
			t.Errorf("FromFloat64(%v) accepted", f)
		}
	}
}

func TestPositive(t *testing.T) {
	if _, err := Positive(0); !errors.Is(err, ErrNonPositive) {
		t.Fatal("zero accepted")
	}
	if _, err := Positive(-1); !errors.Is(err, ErrNonPositive) {
		t.Fatal("negative accepted")
	}
	if k, err := Positive(10); err != nil || k != 10 {
		t.Fatal(err)
	}
}

func TestString(t *testing.T) {
	cases := map[KES]string{0: "KES 0", 999: "KES 999", 1000: "KES 1,000", 1234567: "KES 1,234,567", -2500: "KES -2,500"}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(k), got, want)
		}
	}
}
