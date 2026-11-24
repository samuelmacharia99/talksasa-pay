package product

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// API keys look like tsp_live_<key id>_<secret>.
//
//   - key id: 20 base32 chars, stored in clear, used to find the row;
//   - secret: 52 base32 chars (256 bits), stored only as
//     HMAC-SHA256(pepper, key id || secret).
//
// Because the secret has 256 bits of entropy a slow password hash adds
// nothing; the server-side pepper means a database dump alone cannot be used
// to test candidate keys. Several keys may be active per product, so keys can
// be rotated without downtime: issue new, deploy, revoke old.

var (
	// ErrUnauthorized is deliberately uninformative: callers must not learn
	// whether a key exists, is revoked or belongs to a suspended product.
	ErrUnauthorized  = errors.New("product: invalid API key")
	ErrKeyNotFound   = errors.New("product: API key not found or already revoked")
	ErrInvalidLabel  = errors.New("product: key label must be 1-100 characters")
	ErrInvalidExpiry = errors.New("product: key expiry must be in the future")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeSecret(b []byte) string { return strings.ToLower(b32.EncodeToString(b)) }

// Principal is the authenticated caller of the product API.
type Principal struct {
	ProductID     uuid.UUID
	ProductName   string
	AccountPrefix string
	KeyID         string
	// AllowedCIDRs, when non-empty, restricts which client IPs may use the
	// key. Enforced by the /v1 auth middleware (403 ip_not_allowed).
	AllowedCIDRs []netip.Prefix
}

// Allows reports whether ip may use this key.
func (p Principal) Allows(ip netip.Addr) bool {
	if len(p.AllowedCIDRs) == 0 {
		return true
	}
	ip = ip.Unmap()
	for _, c := range p.AllowedCIDRs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// APIKey is the non-secret view of a key.
type APIKey struct {
	ID         uuid.UUID
	KeyID      string
	Label      string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	// AllowedCIDRs nil = any address.
	AllowedCIDRs []netip.Prefix
}

// IssuedKey is returned exactly once when a key is created.
type IssuedKey struct {
	Key       APIKey
	Plaintext string
}

// IssueAPIKey creates a new key for a product.
func (s *Service) IssueAPIKey(ctx context.Context, actor audit.Actor, productID uuid.UUID, label string, expiresAt *time.Time) (IssuedKey, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 100 {
		return IssuedKey{}, ErrInvalidLabel
	}
	if expiresAt != nil && !expiresAt.After(s.now()) {
		return IssuedKey{}, ErrInvalidExpiry
	}
	keyIDRaw, err := crypto.RandomBytes(12)
	if err != nil {
		return IssuedKey{}, err
	}
	secretRaw, err := crypto.RandomBytes(32)
	if err != nil {
		return IssuedKey{}, err
	}
	keyID, secret := encodeSecret(keyIDRaw), encodeSecret(secretRaw)
	id, err := uuid.NewV7()
	if err != nil {
		return IssuedKey{}, err
	}

	var row store.ProductApiKey
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if _, err := q.GetProduct(ctx, productID); err != nil {
			if db.IsNotFound(err) {
				return ErrNotFound
			}
			return err
		}
		var err error
		row, err = q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			ID: id, ProductID: productID, KeyID: keyID, SecretHash: s.hashSecret(keyID, secret),
			Label: label, CreatedBy: actor.String(), ExpiresAt: expiresAt,
		})
		if err != nil {
			return fmt.Errorf("product: insert api key: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "api_key.issued", EntityType: "api_key", EntityID: keyID, ProductID: &productID,
			Details: map[string]any{"label": label, "expires_at": expiresAt},
		})
	})
	if err != nil {
		return IssuedKey{}, err
	}
	s.logger.InfoContext(ctx, "api key issued", slog.String("product_id", productID.String()), slog.String("key_id", keyID))
	return IssuedKey{
		Key:       keyFromRow(row),
		Plaintext: fmt.Sprintf("tsp_%s_%s_%s", s.keyEnvLabel, keyID, secret),
	}, nil
}

// RevokeAPIKey revokes a key immediately.
func (s *Service) RevokeAPIKey(ctx context.Context, actor audit.Actor, productID uuid.UUID, keyID string) error {
	return db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		by := actor.String()
		_, err := q.RevokeAPIKey(ctx, store.RevokeAPIKeyParams{ProductID: productID, KeyID: keyID, RevokedBy: &by})
		if db.IsNotFound(err) {
			return ErrKeyNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "api_key.revoked", EntityType: "api_key", EntityID: keyID, ProductID: &productID,
		})
	})
}

// ListAPIKeys returns a product's keys (never their secrets).
func (s *Service) ListAPIKeys(ctx context.Context, productID uuid.UUID) ([]APIKey, error) {
	rows, err := store.New(s.pool).ListAPIKeys(ctx, productID)
	if err != nil {
		return nil, err
	}
	out := make([]APIKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, keyFromRow(r))
	}
	return out, nil
}

// Authenticate resolves a presented API key to its product. Every failure
// returns ErrUnauthorized; the reason is logged server-side only.
func (s *Service) Authenticate(ctx context.Context, presented string) (Principal, error) {
	keyID, secret, ok := s.parseKey(presented)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	q := store.New(s.pool)
	row, err := q.GetAPIKeyForAuth(ctx, keyID)
	if err != nil {
		if db.IsNotFound(err) {
			// Spend the same work as a real comparison.
			_ = crypto.Equal(s.hashSecret(keyID, secret), make([]byte, 32))
			s.logger.WarnContext(ctx, "api key rejected", slog.String("reason", "unknown key id"))
			return Principal{}, ErrUnauthorized
		}
		return Principal{}, fmt.Errorf("product: lookup api key: %w", err)
	}
	if !crypto.Equal(s.hashSecret(keyID, secret), row.SecretHash) {
		s.logger.WarnContext(ctx, "api key rejected", slog.String("reason", "bad secret"), slog.String("key_id", keyID))
		return Principal{}, ErrUnauthorized
	}
	now := s.now()
	switch {
	case row.RevokedAt != nil:
		s.logger.WarnContext(ctx, "api key rejected", slog.String("reason", "revoked"), slog.String("key_id", keyID))
		return Principal{}, ErrUnauthorized
	case row.ExpiresAt != nil && !row.ExpiresAt.After(now):
		s.logger.WarnContext(ctx, "api key rejected", slog.String("reason", "expired"), slog.String("key_id", keyID))
		return Principal{}, ErrUnauthorized
	case row.ProductStatus != store.ProductStatusActive:
		s.logger.WarnContext(ctx, "api key rejected", slog.String("reason", "product not active"), slog.String("key_id", keyID))
		return Principal{}, ErrUnauthorized
	}
	if err := q.TouchAPIKey(ctx, row.ID); err != nil {
		// Non-fatal: last_used_at is informational.
		s.logger.WarnContext(ctx, "could not update api key last_used_at", slog.String("key_id", keyID), slog.String("error", err.Error()))
	}
	return Principal{ProductID: row.ProductID, ProductName: row.ProductName, AccountPrefix: row.AccountPrefix, KeyID: keyID, AllowedCIDRs: row.AllowedCidrs}, nil
}

func (s *Service) parseKey(presented string) (keyID, secret string, ok bool) {
	parts := strings.Split(presented, "_")
	if len(parts) != 4 || parts[0] != "tsp" || parts[1] != s.keyEnvLabel {
		return "", "", false
	}
	keyID, secret = parts[2], parts[3]
	if len(keyID) != 20 || len(secret) != 52 || !isB32Lower(keyID) || !isB32Lower(secret) {
		return "", "", false
	}
	return keyID, secret, true
}

func (s *Service) hashSecret(keyID, secret string) []byte {
	return crypto.HMACSHA256(s.pepper, []byte("tsp-api-key:"+keyID+":"+secret))
}

func isB32Lower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func keyFromRow(r store.ProductApiKey) APIKey {
	return APIKey{
		ID: r.ID, KeyID: r.KeyID, Label: r.Label, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
		ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt, LastUsedAt: r.LastUsedAt, AllowedCIDRs: r.AllowedCidrs,
	}
}

// MaxAllowedCIDRs bounds a key's allow-list.
const MaxAllowedCIDRs = 50

// ErrInvalidCIDRs is returned for an empty, oversized or malformed list.
var ErrInvalidCIDRs = errors.New("product: allowed IPs must be 1-50 valid addresses or CIDR ranges (or none to allow any)")

// ParseCIDRs parses "203.0.113.4", "203.0.113.0/24", "2001:db8::/32" style
// entries (a bare address means a single host). Nil/empty input means "any".
func ParseCIDRs(in []string) ([]netip.Prefix, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAllowedCIDRs {
		return nil, ErrInvalidCIDRs
	}
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		var p netip.Prefix
		if strings.Contains(s, "/") {
			pp, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, ErrInvalidCIDRs
			}
			p = pp.Masked()
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, ErrInvalidCIDRs
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		// A /0 would silently mean "any": make the operator say none instead.
		if p.Bits() == 0 {
			return nil, ErrInvalidCIDRs
		}
		out = append(out, p)
	}
	return out, nil
}

// SetAllowedCIDRs replaces a key's IP allow-list (nil = any address). Takes
// effect on the key's next request. Audited.
func (s *Service) SetAllowedCIDRs(ctx context.Context, actor audit.Actor, productID uuid.UUID, keyID string, cidrs []netip.Prefix) (APIKey, error) {
	if len(cidrs) > MaxAllowedCIDRs {
		return APIKey{}, ErrInvalidCIDRs
	}
	var out APIKey
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var arg []netip.Prefix
		if len(cidrs) > 0 {
			arg = cidrs
		}
		row, err := q.SetAPIKeyAllowedCIDRs(ctx, store.SetAPIKeyAllowedCIDRsParams{ProductID: productID, KeyID: keyID, AllowedCidrs: arg})
		if db.IsNotFound(err) {
			return ErrKeyNotFound
		}
		if err != nil {
			return err
		}
		out = keyFromRow(row)
		list := make([]string, 0, len(cidrs))
		for _, c := range cidrs {
			list = append(list, c.String())
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "api_key.allowed_ips_changed", EntityType: "api_key", EntityID: keyID, ProductID: &productID,
			Details: map[string]any{"allowed_ips": list},
		})
	})
	return out, err
}
