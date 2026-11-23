package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func mustLine(t *testing.T, v int) string {
	t.Helper()
	l, err := GenerateKeyLine(v)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSealOpenAndAAD(t *testing.T) {
	kr, err := ParseKeyring(mustLine(t, 1), 0)
	if err != nil {
		t.Fatal(err)
	}
	ct, v, err := kr.Seal([]byte("whsec_x"), []byte("row:1"))
	if err != nil || v != 1 {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("whsec_x")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	pt, err := kr.Open(ct, v, []byte("row:1"))
	if err != nil || string(pt) != "whsec_x" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := kr.Open(ct, v, []byte("row:2")); !errors.Is(err, ErrDecrypt) {
		t.Fatal("ciphertext moved to another row decrypted")
	}
	ct[len(ct)-1] ^= 1
	if _, err := kr.Open(ct, v, []byte("row:1")); !errors.Is(err, ErrDecrypt) {
		t.Fatal("tampered ciphertext decrypted")
	}
	if _, err := kr.Open([]byte("short"), v, nil); !errors.Is(err, ErrDecrypt) {
		t.Fatal("short ciphertext")
	}
	if _, err := kr.Open(ct, 9, nil); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatal("unknown version")
	}
}

func TestRotation(t *testing.T) {
	l1, l2 := mustLine(t, 1), mustLine(t, 2)
	old, _ := ParseKeyring(l1, 0)
	ct, v, _ := old.Seal([]byte("s"), nil)

	kr, err := ParseKeyring("# rotated\n"+l1+"\n\n"+l2+"\n", 0)
	if err != nil || kr.ActiveVersion() != 2 {
		t.Fatalf("active = %d, %v", kr.ActiveVersion(), err)
	}
	if pt, err := kr.Open(ct, v, nil); err != nil || string(pt) != "s" {
		t.Fatal("old ciphertext unreadable after rotation")
	}
	if _, nv, _ := kr.Seal([]byte("s"), nil); nv != 2 {
		t.Fatal("new data not sealed with new key")
	}
	pinned, err := ParseKeyring(l1+"\n"+l2, 1)
	if err != nil || pinned.ActiveVersion() != 1 {
		t.Fatal("pinned active version")
	}
}

func TestParseKeyringErrors(t *testing.T) {
	l1 := mustLine(t, 1)
	for _, in := range []string{"", "# only comments", "1", "x:abc", "0:" + strings.Split(l1, ":")[1], "1:bm90MzJieXRlcw==", l1 + "\n" + l1} {
		if _, err := ParseKeyring(in, 0); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	if _, err := ParseKeyring(l1, 5); err == nil {
		t.Error("missing active version accepted")
	}
}

func TestRandomToken(t *testing.T) {
	a, _ := RandomToken(32)
	b, _ := RandomToken(32)
	if a == b || len(a) != 52 || strings.ToLower(a) != a {
		t.Fatalf("tokens %q %q", a, b)
	}
}
