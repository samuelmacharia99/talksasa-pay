// Package crypto provides authenticated encryption for secrets stored in the
// database (envelope encryption with a versioned master keyring), keyed
// hashing for API keys and webhook signatures, and secure random tokens.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

var (
	// ErrUnknownKeyVersion means the ciphertext was sealed with a key that is
	// not in the keyring (e.g. it was removed before re-encryption finished).
	ErrUnknownKeyVersion = errors.New("crypto: unknown key version")
	// ErrDecrypt hides the reason for an authentication failure on purpose.
	ErrDecrypt = errors.New("crypto: decryption failed")
)

// Keyring holds one or more AES-256-GCM master keys indexed by version.
// New data is sealed with the active version; older versions stay available
// for decryption until rotation re-encrypts everything.
type Keyring struct {
	aeads  map[int]cipher.AEAD
	active int
}

// ParseKeyring parses "<version>:<base64 32-byte key>" lines. Blank lines and
// lines starting with '#' are ignored. activeVersion 0 selects the highest.
func ParseKeyring(content string, activeVersion int) (*Keyring, error) {
	kr := &Keyring{aeads: map[int]cipher.AEAD{}}
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		verStr, keyB64, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("crypto: keyring line %d: expected <version>:<base64key>", i+1)
		}
		ver, err := strconv.Atoi(strings.TrimSpace(verStr))
		if err != nil || ver < 1 {
			return nil, fmt.Errorf("crypto: keyring line %d: version must be a positive integer", i+1)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
		if err != nil || len(key) != KeySize {
			return nil, fmt.Errorf("crypto: keyring line %d: key must be %d bytes, base64-encoded", i+1, KeySize)
		}
		if _, dup := kr.aeads[ver]; dup {
			return nil, fmt.Errorf("crypto: keyring line %d: duplicate version %d", i+1, ver)
		}
		aead, err := newAEAD(key)
		if err != nil {
			return nil, err
		}
		kr.aeads[ver] = aead
	}
	if len(kr.aeads) == 0 {
		return nil, errors.New("crypto: keyring is empty")
	}
	if activeVersion == 0 {
		vs := kr.Versions()
		activeVersion = vs[len(vs)-1]
	}
	if _, ok := kr.aeads[activeVersion]; !ok {
		return nil, fmt.Errorf("crypto: active key version %d not in keyring", activeVersion)
	}
	kr.active = activeVersion
	return kr, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	return cipher.NewGCM(block)
}

// Versions returns the key versions in ascending order.
func (k *Keyring) Versions() []int {
	vs := make([]int, 0, len(k.aeads))
	for v := range k.aeads {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	return vs
}

// ActiveVersion is the version used by Seal.
func (k *Keyring) ActiveVersion() int { return k.active }

// Seal encrypts plaintext with the active key. aad (additional authenticated
// data) binds the ciphertext to its context, e.g. "apps.webhook_secret:<id>",
// so a ciphertext copied into another row fails to decrypt.
// Output layout: nonce || ciphertext+tag.
func (k *Keyring) Seal(plaintext, aad []byte) (ciphertext []byte, version int, err error) {
	aead := k.aeads[k.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("crypto: nonce: %w", err)
	}
	out := aead.Seal(nonce, nonce, plaintext, aad)
	return out, k.active, nil
}

// Open decrypts a ciphertext produced by Seal.
func (k *Keyring) Open(ciphertext []byte, version int, aad []byte) ([]byte, error) {
	aead, ok := k.aeads[version]
	if !ok {
		return nil, ErrUnknownKeyVersion
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns+aead.Overhead() {
		return nil, ErrDecrypt
	}
	pt, err := aead.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// GenerateKeyLine returns a new keyring line for the given version. Used by
// the `keygen` admin command; the output must go straight into the SOPS file.
func GenerateKeyLine(version int) (string, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%s", version, base64.StdEncoding.EncodeToString(key)), nil
}
