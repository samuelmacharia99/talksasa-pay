package phone

import "testing"

func TestNormalize(t *testing.T) {
	ok := map[string]string{
		"0712345678": "254712345678", "712345678": "254712345678", "+254712345678": "254712345678",
		"254712345678": "254712345678", "0712 345 678": "254712345678", "(0712)-345-678": "254712345678",
		"0110123456": "254110123456", "110123456": "254110123456", "+254 110 123 456": "254110123456",
		" 0722000000 ": "254722000000",
	}
	for in, want := range ok {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "07123", "07123456789", "0812345678", "254812345678", "+1 202 555 0100",
		"07123x5678", "25471234567", "2547123456789", "0712+345678", "++254712345678", "0012345678"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want error", in, got)
		}
	}
}

func TestIsNormalized(t *testing.T) {
	if !IsNormalized("254712345678") || IsNormalized("0712345678") {
		t.Fatal("IsNormalized wrong")
	}
}
