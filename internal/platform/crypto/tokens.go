package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"io"
	"strings"
)

// tokenEncoding is lowercase base32 without padding: URL-safe, case-insensitive
// friendly and free of characters that need escaping.
var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// RandomToken returns nBytes of CSPRNG output as lowercase base32.
func RandomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("crypto: random: %w", err)
	}
	return strings.ToLower(tokenEncoding.EncodeToString(b)), nil
}

// RandomBytes returns n bytes of CSPRNG output.
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("crypto: random: %w", err)
	}
	return b, nil
}

// HMACSHA256 computes HMAC-SHA256(key, msg).
func HMACSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// Equal compares two MACs in constant time.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }
