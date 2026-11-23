package payer

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestFormatAndParseRoundTrip(t *testing.T) {
	for base := FirstBase; base < FirstBase+5000; base++ {
		acct, err := FormatAccountNumber("HOST", base)
		if err != nil {
			t.Fatal(err)
		}
		p, err := ParseAccountReference(acct)
		if err != nil {
			t.Fatalf("parse %s: %v", acct, err)
		}
		if p.Prefix != "HOST" || p.Normalized != NormalizeAccountNumber(acct) {
			t.Fatalf("parse %s = %+v", acct, p)
		}
	}
}

func TestFirstAccountsAreShort(t *testing.T) {
	acct, _ := FormatAccountNumber("BILL", FirstBase)
	if len(strings.TrimPrefix(acct, "BILL-")) != 4 {
		t.Fatalf("first account %s is not 4 digits", acct)
	}
}

func TestTypedVariantsNormalise(t *testing.T) {
	acct, _ := FormatAccountNumber("HOST", 447)
	d := strings.TrimPrefix(acct, "HOST-")
	for _, typed := range []string{acct, strings.ToLower(acct), "HOST" + d, "host " + d, "  Host-" + d + "  ", "HOST_" + d, "HOST." + d} {
		p, err := ParseAccountReference(typed)
		if err != nil || p.Normalized != "HOST"+d {
			t.Errorf("%q -> %+v, %v", typed, p, err)
		}
	}
}

// The Damm check digit must catch every single-digit substitution and every
// adjacent transposition.
func TestCheckDigitCatchesTypos(t *testing.T) {
	for base := FirstBase; base < FirstBase+3000; base++ {
		acct, _ := FormatAccountNumber("HOST", base)
		d := []byte(strings.TrimPrefix(acct, "HOST-"))
		for i := range d {
			for r := byte('0'); r <= '9'; r++ {
				if r == d[i] {
					continue
				}
				m := append([]byte(nil), d...)
				m[i] = r
				if _, err := ParseAccountReference("HOST-" + string(m)); err == nil {
					t.Fatalf("substitution %s -> %s not detected", d, m)
				}
			}
			if i+1 < len(d) && d[i] != d[i+1] {
				m := append([]byte(nil), d...)
				m[i], m[i+1] = m[i+1], m[i]
				if _, err := ParseAccountReference("HOST-" + string(m)); err == nil {
					t.Fatalf("transposition %s -> %s not detected", d, m)
				}
			}
		}
	}
}

func TestRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "HOST", "4471", "H-4471", "TOOLONGX-4471", "HOST-0", "HOST-01234", "HOST-44A1", "HOST-4471!", "rent"} {
		if _, err := ParseAccountReference(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	acct, _ := FormatAccountNumber("HOST", 447)
	last := acct[len(acct)-1]
	wrong := acct[:len(acct)-1] + strconv.Itoa(int(last-'0'+1)%10)
	if _, err := ParseAccountReference(wrong); !errors.Is(err, ErrBadCheckDigit) {
		t.Fatalf("bad check digit: %v", err)
	}
}

func TestFormatValidation(t *testing.T) {
	if _, err := FormatAccountNumber("host", 100); !errors.Is(err, ErrInvalidPrefix) {
		t.Fatal("lowercase prefix accepted")
	}
	if _, err := FormatAccountNumber("HOST", 5); err == nil {
		t.Fatal("small base accepted")
	}
}
