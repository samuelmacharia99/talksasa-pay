// Package mpesasettings holds the gateway's one M-Pesa paybill (Safaricom
// Daraja) configuration, which admins edit in the panel's Settings page.
//
// Rules (user decisions, 2026-11-25):
//   - One paybill for the whole gateway, sandbox or production.
//   - The consumer key, consumer secret and passkey are stored encrypted
//     in the database (master keyring, AES-256-GCM, as for app webhook
//     secrets). They are write-only: nothing here returns them, only
//     "set" and, for long values, the last 4 characters.
//   - Every change is audited (which fields changed, never their values).
//   - The TSP_MPESA_* configuration (SOPS) is the fallback while nothing is
//     saved; once saved, the database wins. The callback token, Safaricom's
//     callback networks and the development base URL always come from the
//     configuration.
//
// Provider gives the api and worker the current Daraja client and paybill,
// rebuilding the client when the saved settings change.
package mpesasettings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Source says where the effective settings come from.
type Source string

const (
	SourceDatabase    Source = "database"    // saved in the admin panel
	SourceEnvironment Source = "environment" // TSP_MPESA_* (SOPS) fallback
	SourceNone        Source = "none"        // M-Pesa is off
)

// ConfirmProduction is the word an admin types to change or use production
// settings.
const ConfirmProduction = "production"

// Errors (mapped to API problems by internal/adminapi).
var (
	ErrNotConfigured   = errors.New("mpesasettings: M-Pesa is not configured")
	ErrNoCallbackToken = errors.New("mpesasettings: TSP_MPESA_CALLBACK_TOKEN is not set")
	ErrConfirm         = errors.New(`mpesasettings: production needs confirm: "production"`)
)

// ValidationError is a field the admin must fix.
type ValidationError struct{ Field, Message string }

func (e *ValidationError) Error() string { return "mpesasettings: " + e.Field + ": " + e.Message }

// DarajaError: Safaricom refused or could not be reached. Message never
// contains secrets or the callback token.
type DarajaError struct{ Message string }

func (e *DarajaError) Error() string { return "mpesasettings: safaricom: " + e.Message }

// RedactedToken replaces the callback token in URLs that are shown, logged,
// audited or stored.
const RedactedToken = "[TOKEN]"

// RedactToken removes the secret callback token from s (raw and
// path-escaped forms).
func RedactToken(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, RedactedToken)
	if esc := url.PathEscape(token); esc != token {
		s = strings.ReplaceAll(s, esc, RedactedToken)
	}
	return s
}

// Options configure the service.
type Options struct {
	Pool    *db.Pool
	Keyring *crypto.Keyring
	// Env is the deployment environment (TSP_ENV).
	Env config.Environment
	// Mpesa is the TSP_MPESA_* configuration: the fallback settings, and
	// always the callback token, callback networks, base URL and timeout.
	Mpesa config.MpesaConfig
	// Paybill is TSP_MPESA_PAYBILL (fallback).
	Paybill string
	Logger  *slog.Logger
	// HTTPClient and Now are for tests.
	HTTPClient *http.Client
	Now        func() time.Time
}

// Service reads and writes the settings. Safe for concurrent use.
type Service struct {
	o Options

	mu       sync.Mutex
	onChange []func()
}

// NewService builds the service.
func NewService(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}
}

// OnChange registers f to run after settings are saved by this process
// (the Provider uses it to rebuild at once instead of within its cache
// period).
func (s *Service) OnChange(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, f)
}

func (s *Service) changed() {
	s.mu.Lock()
	fs := slices.Clone(s.onChange)
	s.mu.Unlock()
	for _, f := range fs {
		f()
	}
}

// Settings are the effective, decrypted settings. Internal use only: they
// are never logged or returned over the API (the secrets are config.Secret).
type Settings struct {
	Source      Source
	Environment string
	// Paybill is the C2B shortcode (pay page, URL registration, the
	// foreign-shortcode check). STKShortCode is the BusinessShortCode for
	// prompts (the paybill unless set).
	Paybill         string
	STKShortCode    string
	ConsumerKey     config.Secret
	ConsumerSecret  config.Secret
	Passkey         config.Secret
	CallbackBaseURL string
	// Version of the saved row (0 when not from the database).
	Version int64

	row *store.MpesaSetting
}

func aad(field string) []byte { return []byte("mpesa_settings." + field + ":1") }

// Get returns the effective settings: the saved row when there is one,
// else the TSP_MPESA_* configuration, else Source none.
func (s *Service) Get(ctx context.Context) (Settings, error) {
	row, err := store.New(s.o.Pool).GetMpesaSettings(ctx)
	if db.IsNotFound(err) {
		return s.fallback(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	return s.open(row)
}

func (s *Service) fallback() Settings {
	m := s.o.Mpesa
	if !m.Enabled() {
		return Settings{Source: SourceNone, Paybill: s.o.Paybill}
	}
	return Settings{
		Source: SourceEnvironment, Environment: m.Env, Paybill: s.o.Paybill, STKShortCode: m.ShortCode,
		ConsumerKey: m.ConsumerKey, ConsumerSecret: m.ConsumerSecret, Passkey: m.Passkey,
		CallbackBaseURL: m.CallbackBaseURL,
	}
}

func (s *Service) open(row store.MpesaSetting) (Settings, error) {
	var plain [3]config.Secret
	for i, f := range []struct {
		name string
		ct   []byte
	}{{"consumer_key", row.ConsumerKeyCiphertext}, {"consumer_secret", row.ConsumerSecretCiphertext}, {"passkey", row.PasskeyCiphertext}} {
		pt, err := s.o.Keyring.Open(f.ct, int(row.KeyVersion), aad(f.name))
		if err != nil {
			return Settings{}, fmt.Errorf("mpesasettings: open %s: %w", f.name, err)
		}
		plain[i] = config.NewSecret(string(pt))
	}
	stk := row.Shortcode
	if row.StkShortcode != nil {
		stk = *row.StkShortcode
	}
	return Settings{
		Source: SourceDatabase, Environment: row.Environment, Paybill: row.Shortcode, STKShortCode: stk,
		ConsumerKey: plain[0], ConsumerSecret: plain[1], Passkey: plain[2],
		CallbackBaseURL: row.CallbackBaseUrl, Version: row.Version, row: &row,
	}, nil
}

// client builds a Daraja client for st. Errors never contain secrets.
func (s *Service) client(st Settings) (*mpesa.HTTPClient, error) {
	if st.Source == SourceNone {
		return nil, ErrNotConfigured
	}
	if s.o.Mpesa.CallbackToken.IsZero() {
		return nil, ErrNoCallbackToken
	}
	return mpesa.NewClient(mpesa.ClientOptions{
		Env: st.Environment, BaseURL: s.o.Mpesa.BaseURL, ShortCode: st.STKShortCode,
		ConsumerKey: st.ConsumerKey, ConsumerSecret: st.ConsumerSecret, Passkey: st.Passkey,
		CallbackToken: s.o.Mpesa.CallbackToken, CallbackBaseURL: st.CallbackBaseURL,
		Timeout: s.o.Mpesa.RequestTimeout, Logger: s.o.Logger, HTTPClient: s.o.HTTPClient,
	})
}

// ---------------------------------------------------------------- status

// Credential is what may be shown about a stored secret.
type Credential struct {
	Set bool
	// Last4 is the last 4 characters, only for values of 12 or more
	// characters ("" otherwise).
	Last4 string
}

func credential(s config.Secret) Credential {
	v := s.Reveal()
	c := Credential{Set: v != ""}
	if len(v) >= 12 {
		c.Last4 = v[len(v)-4:]
	}
	return c
}

// CallbackURLs are the URLs Daraja is given, with the token masked.
type CallbackURLs struct{ STK, Confirmation, Validation string }

func maskedURLs(base string) CallbackURLs {
	if base == "" {
		return CallbackURLs{}
	}
	p := base + "cb/mpesa/" + RedactedToken
	return CallbackURLs{STK: p + "/stk", Confirmation: p + "/c2b/confirmation", Validation: p + "/c2b/validation"}
}

// Registration is the last C2B URL registration recorded.
type Registration struct {
	At              time.Time
	By              string
	ConfirmationURL string // token masked
	ShortCode       string
	// Current: it was for today's paybill and confirmation URL.
	Current bool
}

// TestResult is a credential check.
type TestResult struct {
	At      time.Time
	OK      bool
	Message string // why it failed ("" when OK); never secret
}

// Status is the safe view of the settings: no secret values.
type Status struct {
	Source          Source
	Environment     string
	Paybill         string
	STKShortCode    string // "" when it is the paybill
	CallbackBaseURL string
	ConsumerKey     Credential
	ConsumerSecret  Credential
	Passkey         Credential
	// Configuration-only settings.
	CallbackTokenSet bool
	CallbackCIDRs    int
	CallbackURLs     CallbackURLs
	// Saved-row metadata (zero for the fallback).
	Version      int64
	UpdatedBy    string
	UpdatedAt    *time.Time
	Registration *Registration
	LastTest     *TestResult
	// Ready: a Daraja client can be built (prompts can be sent). Problem
	// says why not.
	Ready   bool
	Problem string
	// What Save accepts in this deployment.
	Environments []string
	HTTPSOnly    bool
}

// Status returns the safe view of the effective settings.
func (s *Service) Status(ctx context.Context) (Status, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return Status{}, err
	}
	return s.status(st), nil
}

func (s *Service) status(st Settings) Status {
	out := Status{
		Source: st.Source, Environment: st.Environment, Paybill: st.Paybill, CallbackBaseURL: st.CallbackBaseURL,
		ConsumerKey: credential(st.ConsumerKey), ConsumerSecret: credential(st.ConsumerSecret), Passkey: credential(st.Passkey),
		CallbackTokenSet: !s.o.Mpesa.CallbackToken.IsZero(), CallbackCIDRs: len(s.o.Mpesa.CallbackCIDRs),
		CallbackURLs: maskedURLs(st.CallbackBaseURL), Version: st.Version,
		Environments: s.environments(), HTTPSOnly: s.o.Env != config.EnvDevelopment,
	}
	if st.STKShortCode != st.Paybill {
		out.STKShortCode = st.STKShortCode
	}
	if r := st.row; r != nil {
		out.UpdatedBy, out.UpdatedAt = r.UpdatedBy, &r.UpdatedAt
		if r.UrlsRegisteredAt != nil {
			reg := &Registration{At: *r.UrlsRegisteredAt, By: deref(r.UrlsRegisteredBy),
				ConfirmationURL: deref(r.UrlsRegisteredUrl), ShortCode: deref(r.UrlsRegisteredShortcode)}
			reg.Current = reg.ConfirmationURL == out.CallbackURLs.Confirmation && reg.ShortCode == st.Paybill
			out.Registration = reg
		}
		if r.LastTestAt != nil && r.LastTestOk != nil {
			out.LastTest = &TestResult{At: *r.LastTestAt, OK: *r.LastTestOk}
		}
	}
	if _, err := s.client(st); err != nil {
		out.Problem = problem(err)
	} else {
		out.Ready = true
	}
	return out
}

func problem(err error) string {
	switch {
	case errors.Is(err, ErrNotConfigured):
		return "M-Pesa is off: no paybill is saved and TSP_MPESA_ENV is not set."
	case errors.Is(err, ErrNoCallbackToken):
		return "TSP_MPESA_CALLBACK_TOKEN is not set in the deployment configuration, so Safaricom has nowhere to call back. M-Pesa stays off until it is."
	}
	return "The settings cannot be used: " + strings.TrimPrefix(err.Error(), "mpesa: ")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// environments lists the Daraja environments this deployment may use
// (the same rules as TSP_MPESA_ENV).
func (s *Service) environments() []string {
	switch {
	case s.o.Env == config.EnvProduction:
		return []string{config.MpesaProduction}
	case s.o.Env.IsProductionLike():
		return []string{config.MpesaSandbox, config.MpesaProduction}
	}
	return []string{config.MpesaSandbox}
}

// ---------------------------------------------------------------- save

// SaveParams is an admin's update. A zero credential keeps the stored one
// (all three are required the first time).
type SaveParams struct {
	Environment     string
	Paybill         string
	STKShortCode    string // "" = the paybill
	CallbackBaseURL string
	ConsumerKey     config.Secret
	ConsumerSecret  config.Secret
	Passkey         config.Secret
	// Confirm must be "production" when the saved or new environment is
	// production.
	Confirm string
}

var shortCodeRe = regexp.MustCompile(`^[0-9]{5,7}$`)

const maxCredentialLen = 512

func (s *Service) validate(p *SaveParams, first bool) error {
	p.Environment = strings.TrimSpace(p.Environment)
	p.Paybill = strings.TrimSpace(p.Paybill)
	p.STKShortCode = strings.TrimSpace(p.STKShortCode)
	p.CallbackBaseURL = strings.TrimSpace(p.CallbackBaseURL)
	if !slices.Contains(s.environments(), p.Environment) {
		switch {
		case p.Environment != config.MpesaSandbox && p.Environment != config.MpesaProduction:
			return &ValidationError{"environment", "Choose sandbox or production."}
		case p.Environment == config.MpesaProduction:
			return &ValidationError{"environment", "Production Daraja is only allowed when this deployment runs as staging or production."}
		default:
			return &ValidationError{"environment", "This production deployment must use the production Daraja environment."}
		}
	}
	if p.Environment == config.MpesaProduction && len(s.o.Mpesa.CallbackCIDRs) == 0 {
		return &ValidationError{"environment", "Set TSP_MPESA_CALLBACK_CIDRS (Safaricom's callback IP ranges) in the deployment configuration before using production."}
	}
	if !shortCodeRe.MatchString(p.Paybill) {
		return &ValidationError{"shortcode", "The paybill number must be 5-7 digits."}
	}
	if p.STKShortCode == p.Paybill {
		p.STKShortCode = ""
	}
	if p.STKShortCode != "" && !shortCodeRe.MatchString(p.STKShortCode) {
		return &ValidationError{"stk_shortcode", "The STK shortcode must be 5-7 digits, or empty to use the paybill."}
	}
	u, err := url.Parse(p.CallbackBaseURL)
	switch {
	case err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p.CallbackBaseURL, " \t?#") ||
		(u.Scheme != "https" && u.Scheme != "http") || len(p.CallbackBaseURL) > 500:
		return &ValidationError{"callback_base_url", "Enter an absolute URL such as https://api.pay.example.com/."}
	case u.Scheme != "https" && s.o.Env != config.EnvDevelopment:
		return &ValidationError{"callback_base_url", "The callback base URL must use https."}
	case !strings.HasSuffix(p.CallbackBaseURL, "/"):
		return &ValidationError{"callback_base_url", "The callback base URL must end with /."}
	}
	for _, c := range []struct {
		field string
		v     *config.Secret
	}{{"consumer_key", &p.ConsumerKey}, {"consumer_secret", &p.ConsumerSecret}, {"passkey", &p.Passkey}} {
		v := strings.TrimSpace(c.v.Reveal())
		*c.v = config.NewSecret(v)
		if v == "" {
			if first {
				return &ValidationError{c.field, "Required the first time the paybill is saved."}
			}
			continue
		}
		if len(v) > maxCredentialLen || strings.IndexFunc(v, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
			return &ValidationError{c.field, "Paste the value exactly as Daraja shows it (no spaces, at most 512 characters)."}
		}
	}
	return nil
}

// Save validates, encrypts and stores the settings, and audits which
// fields changed (never their values). Saving identical settings changes
// nothing and records nothing.
func (s *Service) Save(ctx context.Context, actor audit.Actor, p SaveParams) (Status, error) {
	if err := actor.Validate(); err != nil {
		return Status{}, err
	}
	var saved Settings
	changedAny := false
	err := db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		changedAny = false
		var old *Settings
		row, err := q.LockMpesaSettings(ctx)
		switch {
		case db.IsNotFound(err):
		case err != nil:
			return err
		default:
			o, err := s.open(row)
			if err != nil {
				return err
			}
			old = &o
		}
		if err := s.validate(&p, old == nil); err != nil {
			return err
		}
		if (p.Environment == config.MpesaProduction || (old != nil && old.Environment == config.MpesaProduction)) &&
			p.Confirm != ConfirmProduction {
			return ErrConfirm
		}
		next := Settings{Environment: p.Environment, Paybill: p.Paybill, STKShortCode: p.STKShortCode,
			CallbackBaseURL: p.CallbackBaseURL, ConsumerKey: p.ConsumerKey, ConsumerSecret: p.ConsumerSecret, Passkey: p.Passkey}
		var changed []string
		if old == nil {
			changed = []string{"environment", "shortcode", "callback_base_url", "consumer_key", "consumer_secret", "passkey"}
			if p.STKShortCode != "" {
				changed = append(changed, "stk_shortcode")
			}
		} else {
			oldSTK := ""
			if old.STKShortCode != old.Paybill {
				oldSTK = old.STKShortCode
			}
			for _, f := range []struct {
				name     string
				was, now string
			}{
				{"environment", old.Environment, next.Environment},
				{"shortcode", old.Paybill, next.Paybill},
				{"stk_shortcode", oldSTK, next.STKShortCode},
				{"callback_base_url", old.CallbackBaseURL, next.CallbackBaseURL},
			} {
				if f.was != f.now {
					changed = append(changed, f.name)
				}
			}
			for _, f := range []struct {
				name string
				was  config.Secret
				now  *config.Secret
			}{
				{"consumer_key", old.ConsumerKey, &next.ConsumerKey},
				{"consumer_secret", old.ConsumerSecret, &next.ConsumerSecret},
				{"passkey", old.Passkey, &next.Passkey},
			} {
				if f.now.IsZero() {
					*f.now = f.was // keep
				} else if f.now.Reveal() != f.was.Reveal() {
					changed = append(changed, f.name)
				}
			}
		}
		if len(changed) == 0 {
			saved = *old
			return nil
		}
		changedAny = true
		var cts [3][]byte
		ver := 0
		for i, f := range []struct {
			name string
			v    config.Secret
		}{{"consumer_key", next.ConsumerKey}, {"consumer_secret", next.ConsumerSecret}, {"passkey", next.Passkey}} {
			ct, v, err := s.o.Keyring.Seal([]byte(f.v.Reveal()), aad(f.name))
			if err != nil {
				return err
			}
			cts[i], ver = ct, v
		}
		var stk *string
		if next.STKShortCode != "" {
			stk = &next.STKShortCode
		}
		row, err = q.UpsertMpesaSettings(ctx, store.UpsertMpesaSettingsParams{
			Environment: next.Environment, Shortcode: next.Paybill, StkShortcode: stk,
			ConsumerKeyCiphertext: cts[0], ConsumerSecretCiphertext: cts[1], PasskeyCiphertext: cts[2],
			KeyVersion: int32(ver), CallbackBaseUrl: next.CallbackBaseURL, UpdatedBy: actor.String(),
		})
		if err != nil {
			return fmt.Errorf("mpesasettings: save: %w", err)
		}
		if saved, err = s.open(row); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "mpesa_settings.updated", EntityType: "mpesa_settings", EntityID: "1",
			Details: map[string]any{
				"changed": changed, "first_save": old == nil, "version": row.Version,
				"environment": next.Environment, "shortcode": next.Paybill, "stk_shortcode": next.STKShortCode,
				"callback_base_url": next.CallbackBaseURL,
			},
		})
	})
	if err != nil {
		return Status{}, err
	}
	if changedAny {
		s.o.Logger.InfoContext(ctx, "mpesa settings saved", slog.Int64("version", saved.Version),
			slog.String("env", saved.Environment), slog.String("shortcode", saved.Paybill))
		s.changed()
	}
	return s.status(saved), nil
}

// ---------------------------------------------------------------- daraja

// testTimeout and registerTimeout bound the Daraja calls.
const (
	testTimeout     = 30 * time.Second
	registerTimeout = 60 * time.Second
)

// Test asks Daraja for an OAuth token with the effective credentials and
// records the outcome (on the saved row, and in the audit log). The token
// is never returned.
func (s *Service) Test(ctx context.Context, actor audit.Actor) (TestResult, error) {
	if err := actor.Validate(); err != nil {
		return TestResult{}, err
	}
	st, err := s.Get(ctx)
	if err != nil {
		return TestResult{}, err
	}
	c, err := s.client(st)
	if err != nil {
		return TestResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	res := TestResult{OK: true}
	if derr := c.CheckCredentials(cctx); derr != nil {
		res.OK = false
		res.Message = RedactToken(derr.Error(), s.o.Mpesa.CallbackToken.Reveal())
	}
	res.At = s.o.Now()
	actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer acancel()
	err = db.WithTx(actx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if st.Source == SourceDatabase {
			if _, err := q.RecordMpesaSettingsTest(ctx, store.RecordMpesaSettingsTestParams{TestedAt: res.At, Ok: res.OK}); err != nil {
				return err
			}
		}
		d := map[string]any{"ok": res.OK, "source": string(st.Source), "environment": st.Environment, "shortcode": st.Paybill, "version": st.Version}
		if !res.OK {
			d["error"] = truncate(res.Message, 300)
		}
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "mpesa_settings.tested", EntityType: "mpesa_settings", EntityID: "1", Details: d})
	})
	if err != nil {
		return TestResult{}, err
	}
	return res, nil
}

// RegisterResult is a successful C2B URL registration (token masked).
type RegisterResult struct {
	At              time.Time
	ShortCode       string
	ConfirmationURL string
	ValidationURL   string
	ResponseType    string
}

// RegisterURLs registers the paybill's C2B confirmation (and validation)
// URLs with Safaricom (ResponseType Completed: we never reject money),
// records it and audits it. With production settings, confirm must be
// "production".
func (s *Service) RegisterURLs(ctx context.Context, actor audit.Actor, confirm string) (RegisterResult, error) {
	if err := actor.Validate(); err != nil {
		return RegisterResult{}, err
	}
	st, err := s.Get(ctx)
	if err != nil {
		return RegisterResult{}, err
	}
	if st.Source != SourceNone && st.Paybill == "" {
		return RegisterResult{}, &ValidationError{"shortcode", "No paybill number is set (TSP_MPESA_PAYBILL)."}
	}
	c, err := s.client(st)
	if err != nil {
		return RegisterResult{}, err
	}
	if st.Environment == config.MpesaProduction && confirm != ConfirmProduction {
		return RegisterResult{}, ErrConfirm
	}
	token := s.o.Mpesa.CallbackToken.Reveal()
	_, confirmation, validation := c.CallbackURLs()
	req := mpesa.RegisterURLsRequest{ShortCode: st.Paybill, ConfirmationURL: confirmation, ValidationURL: validation, ResponseType: "Completed"}
	cctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	if err := c.RegisterC2BURLs(cctx, req); err != nil {
		// Daraja may echo the URL (with the token) back in its message.
		return RegisterResult{}, &DarajaError{Message: RedactToken(err.Error(), token)}
	}
	out := RegisterResult{At: s.o.Now(), ShortCode: st.Paybill, ConfirmationURL: RedactToken(confirmation, token),
		ValidationURL: RedactToken(validation, token), ResponseType: req.ResponseType}
	// Safaricom has accepted: the record must not be lost to a cancelled
	// request.
	actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer acancel()
	err = db.WithTx(actx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if st.Source == SourceDatabase {
			if _, err := q.RecordMpesaURLsRegistered(ctx, store.RecordMpesaURLsRegisteredParams{
				RegisteredAt: out.At, RegisteredBy: actor.String(), RegisteredUrl: out.ConfirmationURL, RegisteredShortcode: out.ShortCode,
			}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "mpesa.paybill_urls_registered", EntityType: "mpesa_shortcode", EntityID: st.Paybill,
			Details: map[string]any{"shortcode": st.Paybill, "confirmation_url": out.ConfirmationURL, "validation_url": out.ValidationURL,
				"response_type": req.ResponseType, "environment": st.Environment, "source": string(st.Source)},
		})
	})
	if err != nil {
		return RegisterResult{}, fmt.Errorf("URLs were registered with Safaricom but recording it failed: %w", err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
