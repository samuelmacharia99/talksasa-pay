package appapi

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCursorRoundTripAndTamper(t *testing.T) {
	id := uuid.New()
	c := encodeCursor(cursorEvents, 123456789, id)
	n, got, err := decodeCursor(cursorEvents, c)
	if err != nil || n != 123456789 || got != id {
		t.Fatalf("round trip: %v %d %s", err, n, got)
	}
	if _, _, err := decodeCursor(cursorCharges, c); err == nil {
		t.Fatal("cursor of another kind accepted")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(c)
	badVersion := append([]byte{9}, raw[1:]...)
	negative := append([]byte{}, raw...)
	negative[2] = 0x80
	bad := []string{
		"", "!!!", strings.Repeat("A", 201), c + "A", c[:len(c)-2],
		base64.StdEncoding.EncodeToString(raw), // padded alphabet
		base64.RawURLEncoding.EncodeToString(badVersion),
		base64.RawURLEncoding.EncodeToString(raw[:10]),
		base64.RawURLEncoding.EncodeToString(negative),
	}
	for _, b := range bad {
		if _, _, err := decodeCursor(cursorEvents, b); err == nil {
			t.Errorf("accepted bad cursor %q", b)
		}
	}
}
