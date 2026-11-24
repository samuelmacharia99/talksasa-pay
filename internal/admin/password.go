package admin

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
)

// Passwords are hashed with argon2id and stored as a PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt b64>$<hash b64>
//
// The parameters travel with the hash, so they can be raised later; a hash
// made with weaker parameters is transparently upgraded on the next login.
type argonParams struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	keyLen    uint32
	saltLen   int
}

// defaultArgon is above the OWASP 2024 minimum (m=19 MiB, t=2, p=1).
var defaultArgon = argonParams{memoryKiB: 64 * 1024, time: 3, threads: 4, keyLen: 32, saltLen: 16}

const (
	minPasswordLen = 12
	maxPasswordLen = 128
)

// ErrWeakPassword wraps a human-readable reason the password was rejected.
var ErrWeakPassword = errors.New("password does not meet the policy")

type weakPasswordError struct{ reason string }

func (e weakPasswordError) Error() string { return e.reason }
func (e weakPasswordError) Unwrap() error { return ErrWeakPassword }

// PasswordProblem returns the policy message for a weak-password error.
func PasswordProblem(err error) (string, bool) {
	var w weakPasswordError
	if errors.As(err, &w) {
		return w.reason, true
	}
	return "", false
}

// CheckPasswordPolicy enforces length (NIST 800-63B style: length over
// composition rules) and rejects passwords built from the user's own details
// or a single repeated character.
func CheckPasswordPolicy(password, email, name string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case !utf8.ValidString(password):
		return weakPasswordError{"Password contains invalid characters."}
	case n < minPasswordLen:
		return weakPasswordError{fmt.Sprintf("Password must be at least %d characters.", minPasswordLen)}
	case n > maxPasswordLen:
		return weakPasswordError{fmt.Sprintf("Password must be at most %d characters.", maxPasswordLen)}
	case strings.Count(password, string([]rune(password)[0])) == n:
		return weakPasswordError{"Password cannot be a single repeated character."}
	}
	lower := strings.ToLower(password)
	if local, _, ok := strings.Cut(strings.ToLower(email), "@"); ok && len(local) >= 4 && strings.Contains(lower, local) {
		return weakPasswordError{"Password must not contain your email address."}
	}
	for _, part := range strings.Fields(strings.ToLower(name)) {
		if len(part) >= 4 && strings.Contains(lower, part) {
			return weakPasswordError{"Password must not contain your name."}
		}
	}
	return nil
}

// hasher bounds concurrent argon2 work: each hash allocates 64 MiB, so an
// unbounded burst of login attempts could exhaust memory.
type hasher struct {
	params argonParams
	sem    chan struct{}

	dummyOnce sync.Once
	dummy     string
}

func newHasher(p argonParams, concurrency int) *hasher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &hasher{params: p, sem: make(chan struct{}, concurrency)}
}

func (h *hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hasher) release() { <-h.sem }

// Hash returns the PHC string for password.
func (h *hasher) Hash(ctx context.Context, password string) (string, error) {
	salt, err := crypto.RandomBytes(h.params.saltLen)
	if err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	p := h.params
	key := argon2.IDKey([]byte(password), salt, p.time, p.memoryKiB, p.threads, p.keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memoryKiB, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify checks password against a PHC hash in constant time. needsRehash is
// true when the hash was made with parameters other than the current ones.
func (h *hasher) Verify(ctx context.Context, password, phc string) (ok, needsRehash bool, err error) {
	p, salt, want, err := parsePHC(phc)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.time, p.memoryKiB, p.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	cur := h.params
	return true, p.memoryKiB != cur.memoryKiB || p.time != cur.time || p.threads != cur.threads || uint32(len(want)) != cur.keyLen, nil
}

// VerifyDummy spends the same time as a real verification. It is used when
// the email is unknown so response timing does not reveal which accounts
// exist.
func (h *hasher) VerifyDummy(ctx context.Context, password string) {
	h.dummyOnce.Do(func() {
		d, err := h.Hash(ctx, "dummy-password-for-timing-equalisation")
		if err == nil {
			h.dummy = d
		}
	})
	if h.dummy != "" {
		_, _, _ = h.Verify(ctx, password, h.dummy)
	}
}

func parsePHC(phc string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(phc, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("admin: unsupported password hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errors.New("admin: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.time, &p.threads); err != nil {
		return p, nil, nil, errors.New("admin: malformed argon2 parameters")
	}
	// Refuse absurd parameters from a tampered row (memory DoS).
	if p.memoryKiB < 8*1024 || p.memoryKiB > 1024*1024 || p.time < 1 || p.time > 20 || p.threads < 1 {
		return p, nil, nil, errors.New("admin: argon2 parameters out of range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, errors.New("admin: malformed salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, errors.New("admin: malformed hash")
	}
	return p, salt, key, nil
}
