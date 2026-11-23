// Package product manages Talksasa products (ISP Billing, Cloud Hosting, ...)
// and their API keys. Adding a product is data, not code: create a row with
// its account prefix, issue an API key, configure its webhook URL.
package product

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	ErrNotFound       = errors.New("product: not found")
	ErrPrefixTaken    = errors.New("product: account prefix already in use")
	ErrInvalidName    = errors.New("product: name must be 1-100 characters")
	ErrInvalidWebhook = errors.New("product: webhook URL must be an absolute https URL (http allowed only in development)")
)

// webhookSecretBytes is the size of each product's HMAC signing secret.
const webhookSecretBytes = 32

// Product is the public view of a product. It never carries secrets.
type Product struct {
	ID            uuid.UUID
	Name          string
	AccountPrefix string
	WebhookURL    string
	Status        store.ProductStatus
	CreatedAt     time.Time
}

// Service manages products and API keys.
type Service struct {
	pool        *db.Pool
	keys        *crypto.Keyring
	pepper      []byte
	allowHTTP   bool   // development only: allow http:// webhook URLs
	keyEnvLabel string // "live" in production, "test" elsewhere
	logger      *slog.Logger
	now         func() time.Time
}

// Options configures the Service.
type Options struct {
	Pool           *db.Pool
	Keyring        *crypto.Keyring
	APIKeyPepper   []byte
	AllowHTTPHooks bool
	LiveKeys       bool
	Logger         *slog.Logger
}

// NewService builds a product service.
func NewService(o Options) *Service {
	label := "test"
	if o.LiveKeys {
		label = "live"
	}
	return &Service{
		pool: o.Pool, keys: o.Keyring, pepper: o.APIKeyPepper, allowHTTP: o.AllowHTTPHooks,
		keyEnvLabel: label, logger: o.Logger, now: time.Now,
	}
}

// CreateParams describes a new product.
type CreateParams struct {
	Name          string
	AccountPrefix string
	WebhookURL    string // optional at creation
}

// Created is returned once on creation. WebhookSecret must be handed to the
// product team over a secure channel; it can be rotated but never re-read
// through the API.
type Created struct {
	Product       Product
	WebhookSecret string
}

// Create registers a product, its account-number counter and its webhook
// signing secret, and audits the action.
func (s *Service) Create(ctx context.Context, actor audit.Actor, p CreateParams) (Created, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 100 {
		return Created{}, ErrInvalidName
	}
	prefix := strings.ToUpper(strings.TrimSpace(p.AccountPrefix))
	if !payer.ValidPrefix(prefix) {
		return Created{}, payer.ErrInvalidPrefix
	}
	hook, err := s.validateWebhookURL(p.WebhookURL)
	if err != nil {
		return Created{}, err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Created{}, err
	}
	secretRaw, err := crypto.RandomBytes(webhookSecretBytes)
	if err != nil {
		return Created{}, err
	}
	secret := "whsec_" + encodeSecret(secretRaw)
	ct, ver, err := s.keys.Seal([]byte(secret), webhookSecretAAD(id))
	if err != nil {
		return Created{}, err
	}

	var row store.Product
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		row, err = q.CreateProduct(ctx, store.CreateProductParams{
			ID: id, Name: name, AccountPrefix: prefix, WebhookUrl: hook,
			WebhookSecretCiphertext: ct, WebhookSecretKeyVersion: int32(ver),
		})
		if err != nil {
			if db.IsUniqueViolation(err, "products_account_prefix_key") {
				return ErrPrefixTaken
			}
			return fmt.Errorf("product: insert: %w", err)
		}
		if err := q.CreateAccountCounter(ctx, store.CreateAccountCounterParams{ProductID: id, FirstBase: payer.FirstBase}); err != nil {
			return fmt.Errorf("product: counter: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "product.created", EntityType: "product", EntityID: id.String(), ProductID: &id,
			Details: map[string]any{"name": name, "account_prefix": prefix, "webhook_url": derefStr(hook)},
		})
	})
	if err != nil {
		return Created{}, err
	}
	s.logger.InfoContext(ctx, "product created", slog.String("product_id", id.String()), slog.String("account_prefix", prefix))
	return Created{Product: fromRow(row), WebhookSecret: secret}, nil
}

// Get returns a product by id.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Product, error) {
	row, err := store.New(s.pool).GetProduct(ctx, id)
	if db.IsNotFound(err) {
		return Product{}, ErrNotFound
	}
	if err != nil {
		return Product{}, err
	}
	return fromRow(row), nil
}

// List returns all products.
func (s *Service) List(ctx context.Context) ([]Product, error) {
	rows, err := store.New(s.pool).ListProducts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Product, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// SetWebhookURL changes where events are delivered.
func (s *Service) SetWebhookURL(ctx context.Context, actor audit.Actor, id uuid.UUID, rawURL string) (Product, error) {
	hook, err := s.validateWebhookURL(rawURL)
	if err != nil {
		return Product{}, err
	}
	var row store.Product
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		row, err = q.UpdateProductWebhookURL(ctx, store.UpdateProductWebhookURLParams{ID: id, WebhookUrl: hook})
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "product.webhook_url_changed", EntityType: "product", EntityID: id.String(), ProductID: &id,
			Details: map[string]any{"webhook_url": derefStr(hook)},
		})
	})
	if err != nil {
		return Product{}, err
	}
	return fromRow(row), nil
}

// RotateWebhookSecret replaces the signing secret and returns the new one once.
func (s *Service) RotateWebhookSecret(ctx context.Context, actor audit.Actor, id uuid.UUID) (string, error) {
	secretRaw, err := crypto.RandomBytes(webhookSecretBytes)
	if err != nil {
		return "", err
	}
	secret := "whsec_" + encodeSecret(secretRaw)
	ct, ver, err := s.keys.Seal([]byte(secret), webhookSecretAAD(id))
	if err != nil {
		return "", err
	}
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		_, err := q.UpdateProductWebhookSecret(ctx, store.UpdateProductWebhookSecretParams{
			ID: id, WebhookSecretCiphertext: ct, WebhookSecretKeyVersion: int32(ver),
		})
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "product.webhook_secret_rotated", EntityType: "product", EntityID: id.String(), ProductID: &id,
		})
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// WebhookSecret decrypts a product's signing secret for the webhook signer.
// The value must never be logged or returned over the API.
func (s *Service) WebhookSecret(ctx context.Context, id uuid.UUID) ([]byte, error) {
	row, err := store.New(s.pool).GetProduct(ctx, id)
	if db.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.keys.Open(row.WebhookSecretCiphertext, int(row.WebhookSecretKeyVersion), webhookSecretAAD(id))
}

func (s *Service) validateWebhookURL(raw string) (*string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || len(raw) > 2048 {
		return nil, ErrInvalidWebhook
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !s.allowHTTP {
			return nil, ErrInvalidWebhook
		}
	default:
		return nil, ErrInvalidWebhook
	}
	return &raw, nil
}

// webhookSecretAAD binds a ciphertext to its product row.
func webhookSecretAAD(id uuid.UUID) []byte { return []byte("products.webhook_secret:" + id.String()) }

func fromRow(r store.Product) Product {
	return Product{
		ID: r.ID, Name: r.Name, AccountPrefix: r.AccountPrefix, WebhookURL: derefStr(r.WebhookUrl),
		Status: r.Status, CreatedAt: r.CreatedAt,
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Resolve finds a product by id or by account prefix (case-insensitive).
func (s *Service) Resolve(ctx context.Context, ref string) (Product, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Product{}, ErrNotFound
	}
	if id, err := uuid.Parse(ref); err == nil {
		return s.Get(ctx, id)
	}
	row, err := store.New(s.pool).GetProductByPrefix(ctx, strings.ToUpper(ref))
	if db.IsNotFound(err) {
		return Product{}, ErrNotFound
	}
	if err != nil {
		return Product{}, err
	}
	return fromRow(row), nil
}
