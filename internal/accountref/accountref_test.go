package accountref

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// The database CHECK on charges.account_number.
var schemaRe = regexp.MustCompile(`^[A-Z]{2,6}-[0-9][A-Z0-9]*$`)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"HOST-1042":    "HOST1042",
		"host 1042":    "HOST1042",
		"  Host-1042 ": "HOST1042",
		"HOST_10.42":   "HOST1042",
		"ho-st1042":    "HOST1042",
		"":             "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	ok := map[string]Parsed{
		"HOST-1042":  {"HOST", "HOST1042"},
		"host 1042":  {"HOST", "HOST1042"},
		"ab1":        {"AB", "AB1"},
		"ABCDEF-9XY": {"ABCDEF", "ABCDEF9XY"},
	}
	for in, want := range ok {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "H-1042", "ABCDEFG-1", "HOST", "HOST-XY", "1042", "HOST#1042", "HOSTÄ1"} {
		if _, err := Parse(in); !errors.Is(err, ErrNotAnAccount) {
			t.Errorf("Parse(%q) err = %v, want ErrNotAnAccount", in, err)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, acct := range []string{"HOST-1042", "HOST-1", "HOST-9ABC0DE", "AB-123456789"} {
		if err := Validate(acct[:strings.IndexByte(acct, '-')], acct); err != nil {
			t.Errorf("Validate(%q) = %v", acct, err)
		}
	}
	cases := []struct {
		prefix, acct string
		want         error
	}{
		{"HOST", "BILL-1042", ErrWrongPrefix},
		{"HOST", "HOST-A1", ErrNotAnAccount},
		{"HOST", "HOST1042", ErrNotAnAccount},
		{"HOST", "host-1042", ErrNotAnAccount},
		{"HOST", "HOST-10-42", ErrNotAnAccount},
		{"HOST", "HOST-1042 ", ErrNotAnAccount},
		{"HOST", "HOST-12345678", ErrTooLong},
		{"host", "host-1", ErrInvalidPrefix},
	}
	for _, c := range cases {
		if err := Validate(c.prefix, c.acct); !errors.Is(err, c.want) {
			t.Errorf("Validate(%q, %q) = %v, want %v", c.prefix, c.acct, err, c.want)
		}
	}
}

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for _, prefix := range []string{"AB", "HOST", "ABCDEF"} {
		for range 2000 {
			acct, err := Generate(prefix)
			if err != nil {
				t.Fatal(err)
			}
			if len(acct) != MaxLen || !schemaRe.MatchString(acct) {
				t.Fatalf("Generate(%s) = %q: wrong shape", prefix, acct)
			}
			if err := Validate(prefix, acct); err != nil {
				t.Fatalf("Generate(%s) = %q fails Validate: %v", prefix, acct, err)
			}
			p, err := Parse(acct)
			if err != nil || p.Prefix != prefix || p.Normalized != strings.ReplaceAll(acct, "-", "") {
				t.Fatalf("Parse(%q) = %+v, %v", acct, p, err)
			}
			if !ValidCheck(prefix, acct) || !ValidCheck(prefix, strings.ToLower(acct)) || !ValidCheck(prefix, strings.ReplaceAll(acct, "-", " ")) {
				t.Fatalf("ValidCheck(%q) = false", acct)
			}
			if strings.ContainsAny(acct[len(prefix)+1:], "ILOU") {
				t.Fatalf("%q uses a non-Crockford character", acct)
			}
			seen[acct] = true
		}
	}
	// AB and HOST have 8 and 6 random characters: 4000 draws never repeat
	// in practice. ABCDEF has only 3 (9*32^3 = 294,912 numbers), so a few
	// repeats are expected there; the live-charge unique index makes the
	// charge service retry.
	if len(seen) < 3990+1900 {
		t.Fatalf("only %d distinct numbers out of 6000", len(seen))
	}
	if ValidCheck("BILL", "HOST-1234567") || ValidCheck("HOST", "HOST-1") {
		t.Fatal("ValidCheck accepted a wrong prefix or a too-short body")
	}
	if _, err := Generate("host"); !errors.Is(err, ErrInvalidPrefix) {
		t.Fatalf("lowercase prefix: %v", err)
	}
}

// The check character catches every single-character substitution and every
// adjacent transposition in the generated part.
func TestCheckCatchesTypos(t *testing.T) {
	for range 200 {
		acct, err := Generate("HOST")
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(acct[5:])
		for i := range body {
			for j := 0; j < len(crockford); j++ {
				if crockford[j] == body[i] {
					continue
				}
				typo := append([]byte(nil), body...)
				typo[i] = crockford[j]
				if ValidCheck("HOST", "HOST-"+string(typo)) {
					t.Fatalf("substitution %s -> HOST-%s not detected", acct, typo)
				}
			}
			if i+1 < len(body) && body[i] != body[i+1] {
				typo := append([]byte(nil), body...)
				typo[i], typo[i+1] = typo[i+1], typo[i]
				if ValidCheck("HOST", "HOST-"+string(typo)) {
					t.Fatalf("transposition %s -> HOST-%s not detected", acct, typo)
				}
			}
		}
	}
}

func TestGFMulIsAField(t *testing.T) {
	// Every non-zero element has an inverse (the polynomial is irreducible).
	for a := uint8(1); a < 32; a++ {
		found := false
		for b := uint8(1); b < 32; b++ {
			if gfMul(a, b) == 1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("%d has no inverse", a)
		}
	}
}
