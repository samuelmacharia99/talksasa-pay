// Package app manages apps (any system collecting money through the gateway)
// and their API keys. Adding an app is data, not code: create a row with
// its account prefix, issue an API key, configure its webhook URL.
package app

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

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	ErrNotFound       = errors.New("app: not found")
	ErrPrefixTaken    = errors.New("app: account prefix already in use")
	ErrInvalidName    = errors.New("app: name must be 1-100 characters")
	ErrInvalidWebhook = errors.New("app: webhook URL must be an absolute https URL (http allowed only in development)")
)

// webhookSecretBytes is the size of each app's HMAC signing secret.
const webhookSecretBytes = 32

// App is the public view of an app. It never carries secrets.
type App struct {
	ID            uuid.UUID
	Name          string
	AccountPrefix string
	WebhookURL    string
	Status        store.AppStatus
	CreatedAt     time.Time
}

// Service manages apps and API keys.
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

// NewService builds an app service.
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

// CreateParams describes a new app.
type CreateParams struct {
	Name          string
	AccountPrefix string
	WebhookURL    string // optional at creation
}

// Created is returned once on creation. WebhookSecret must be handed to the
// app team over a secure channel; it can be rotated but never re-read
// through the API.
type Created struct {
	App           App
	WebhookSecret string
}

// Create registers an app and its webhook
// signing secret, and audits the action.
func (s *Service) Create(ctx context.Context, actor audit.Actor, p CreateParams) (Created, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 100 {
		return Created{}, ErrInvalidName
	}
	prefix := strings.ToUpper(strings.TrimSpace(p.AccountPrefix))
	if !accountref.ValidPrefix(prefix) {
		return Created{}, accountref.ErrInvalidPrefix
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

	var row store.App
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		row, err = q.CreateApp(ctx, store.CreateAppParams{
			ID: id, Name: name, AccountPrefix: prefix, WebhookUrl: hook,
			WebhookSecretCiphertext: ct, WebhookSecretKeyVersion: int32(ver),
		})
		if err != nil {
			if db.IsUniqueViolation(err, "apps_account_prefix_key") {
				return ErrPrefixTaken
			}
			return fmt.Errorf("app: insert: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "app.created", EntityType: "app", EntityID: id.String(), AppID: &id,
			Details: map[string]any{"name": name, "account_prefix": prefix, "webhook_url": derefStr(hook)},
		})
	})
	if err != nil {
		return Created{}, err
	}
	s.logger.InfoContext(ctx, "app created", slog.String("app_id", id.String()), slog.String("account_prefix", prefix))
	return Created{App: fromRow(row), WebhookSecret: secret}, nil
}

// Get returns an app by id.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (App, error) {
	row, err := store.New(s.pool).GetApp(ctx, id)
	if db.IsNotFound(err) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	return fromRow(row), nil
}

// List returns all apps.
func (s *Service) List(ctx context.Context) ([]App, error) {
	rows, err := store.New(s.pool).ListApps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]App, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// SetWebhookURL changes where events are delivered.
func (s *Service) SetWebhookURL(ctx context.Context, actor audit.Actor, id uuid.UUID, rawURL string) (App, error) {
	hook, err := s.validateWebhookURL(rawURL)
	if err != nil {
		return App{}, err
	}
	var row store.App
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		row, err = q.UpdateAppWebhookURL(ctx, store.UpdateAppWebhookURLParams{ID: id, WebhookUrl: hook})
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "app.webhook_url_changed", EntityType: "app", EntityID: id.String(), AppID: &id,
			Details: map[string]any{"webhook_url": derefStr(hook)},
		})
	})
	if err != nil {
		return App{}, err
	}
	return fromRow(row), nil
}

// SetStatus suspends or re-activates an app. A suspended app's API
// keys stop working immediately and its pay pages stop resolving; money that
// still arrives on its account numbers is recorded as usual.
func (s *Service) SetStatus(ctx context.Context, actor audit.Actor, id uuid.UUID, status store.AppStatus) (App, error) {
	if !status.Valid() {
		return App{}, fmt.Errorf("app: unknown status %q", status)
	}
	var row store.App
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		before, err := q.GetApp(ctx, id)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if before.Status == status {
			row = before
			return nil
		}
		row, err = q.SetAppStatus(ctx, store.SetAppStatusParams{ID: id, Status: status})
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "app.status_changed", EntityType: "app", EntityID: id.String(), AppID: &id,
			Details: map[string]any{"from": string(before.Status), "to": string(status)},
		})
	})
	if err != nil {
		return App{}, err
	}
	s.logger.InfoContext(ctx, "app status changed", slog.String("app_id", id.String()), slog.String("status", string(status)))
	return fromRow(row), nil
}

// WebhookSecretGrace is how long the previous webhook secret keeps signing
// after a rotation, so receivers can switch over without dropping events.
const WebhookSecretGrace = 24 * time.Hour

// RotateWebhookSecret replaces the signing secret and returns the new one once.
// The old secret keeps signing (as a second v1 signature) for WebhookSecretGrace.
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
		_, err := q.UpdateAppWebhookSecret(ctx, store.UpdateAppWebhookSecretParams{
			ID: id, WebhookSecretCiphertext: ct, WebhookSecretKeyVersion: int32(ver),
			PrevExpiresAt: s.now().Add(WebhookSecretGrace),
		})
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "app.webhook_secret_rotated", EntityType: "app", EntityID: id.String(), AppID: &id,
		})
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// WebhookSecret decrypts an app's signing secret for the webhook signer.
// The value must never be logged or returned over the API.
func (s *Service) WebhookSecret(ctx context.Context, id uuid.UUID) ([]byte, error) {
	row, err := store.New(s.pool).GetApp(ctx, id)
	if db.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.keys.Open(row.WebhookSecretCiphertext, int(row.WebhookSecretKeyVersion), webhookSecretAAD(id))
}

// WebhookSecrets decrypts the current signing secret and, during the
// rotation grace period (WebhookSecretGrace), the previous one. previous is
// nil when there is none or it has expired. Both values are secrets: never
// log or return them over the API.
func (s *Service) WebhookSecrets(ctx context.Context, id uuid.UUID) (current, previous []byte, err error) {
	row, err := store.New(s.pool).GetApp(ctx, id)
	if db.IsNotFound(err) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	current, err = s.keys.Open(row.WebhookSecretCiphertext, int(row.WebhookSecretKeyVersion), webhookSecretAAD(id))
	if err != nil {
		return nil, nil, fmt.Errorf("app: open webhook secret: %w", err)
	}
	if row.WebhookSecretPrevCiphertext == nil || row.WebhookSecretPrevKeyVersion == nil ||
		row.WebhookSecretPrevExpiresAt == nil || !row.WebhookSecretPrevExpiresAt.After(s.now()) {
		return current, nil, nil
	}
	// Sealed by RotateWebhookSecret's predecessor with the same AAD (the
	// ciphertext was moved, not re-encrypted).
	previous, err = s.keys.Open(row.WebhookSecretPrevCiphertext, int(*row.WebhookSecretPrevKeyVersion), webhookSecretAAD(id))
	if err != nil {
		return nil, nil, fmt.Errorf("app: open previous webhook secret: %w", err)
	}
	return current, previous, nil
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

// webhookSecretAAD binds a ciphertext to its app row.
func webhookSecretAAD(id uuid.UUID) []byte { return []byte("apps.webhook_secret:" + id.String()) }

func fromRow(r store.App) App {
	return App{
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

// Resolve finds an app by id or by account prefix (case-insensitive).
func (s *Service) Resolve(ctx context.Context, ref string) (App, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return App{}, ErrNotFound
	}
	if id, err := uuid.Parse(ref); err == nil {
		return s.Get(ctx, id)
	}
	row, err := store.New(s.pool).GetAppByPrefix(ctx, strings.ToUpper(ref))
	if db.IsNotFound(err) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	return fromRow(row), nil
}
