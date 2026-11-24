// Package config loads and validates runtime configuration.
//
// Non-secret settings come from environment variables. Secrets are never read
// from plain environment variables in staging or production: they are read
// from files (TSP_*_FILE), which are decrypted at deploy time from the
// SOPS-encrypted secrets bundle and mounted as Docker secrets on tmpfs.
// See docs/runbooks/secrets.md.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Environment is the deployment environment.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

// IsProductionLike reports whether strict security checks apply.
func (e Environment) IsProductionLike() bool { return e == EnvStaging || e == EnvProduction }

// Config is the fully validated runtime configuration.
type Config struct {
	Env      Environment
	LogLevel slog.Level
	HTTP     HTTPConfig
	Metrics  MetricsConfig
	Database DatabaseConfig
	Crypto   CryptoConfig
	Admin    AdminConfig
	Public   PublicConfig
	API      APIConfig
	Worker   WorkerConfig
	// TrustedProxies are peers (the Next.js servers, the reverse proxy)
	// whose X-Talksasa-Client-IP header is believed. Anyone else's is ignored.
	TrustedProxies []netip.Prefix
}

// AdminConfig configures the staff admin API listener and sessions.
type AdminConfig struct {
	// Addr must only be reachable from the admin Next.js server (internal
	// network); it is never published to the internet.
	Addr            string
	SetupBaseURL    string // the one-time setup token is appended
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
}

// PublicConfig configures the public pay-page API.
type PublicConfig struct {
	// Paybill is Talksasa's M-Pesa paybill number shown on the pay page.
	// Empty hides the manual paybill instructions.
	Paybill string
	// PayBaseURL is the hosted pay page base; a request's token is appended
	// to form its pay_url. Must end in "/"; https outside development.
	PayBaseURL string
}

// APIConfig bounds the product integration API (/v1).
type APIConfig struct {
	// RatePerKey / BurstPerKey: token bucket per API key (requests/second).
	RatePerKey  float64
	BurstPerKey int
	// MaxInFlight caps concurrent /v1 requests; beyond it requests are shed
	// with 503 + Retry-After instead of queueing until everything times out.
	MaxInFlight int
	// RequestTimeout is the deadline for one /v1 request (incl. DB work).
	RequestTimeout time.Duration
	// LockTimeout bounds how long a request waits for a row lock.
	LockTimeout time.Duration
}

// WorkerConfig configures the background worker (River).
type WorkerConfig struct {
	WebhookConcurrency int           // parallel webhook deliveries
	WebhookTimeout     time.Duration // per HTTP attempt
	MaintenanceEvery   time.Duration // expiry sweep interval
	AttemptRetention   time.Duration // webhook_attempts kept this long
	// HealthAddr serves the worker's /healthz and /metrics. Internal only.
	HealthAddr string
	// ShutdownTimeout: running jobs get this long to finish on SIGTERM
	// before their contexts are cancelled (they are retried later).
	ShutdownTimeout time.Duration
}

// HTTPConfig configures the public API listener.
type HTTPConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	// DrainDelay is how long readiness reports "draining" (503) before the
	// listener stops accepting, so load balancers stop routing new traffic
	// first. 0 (the default) shuts down immediately after flipping readiness.
	DrainDelay   time.Duration
	MaxBodyBytes int64
}

// MetricsConfig configures the internal-only metrics listener.
type MetricsConfig struct {
	Addr string
}

// DatabaseConfig configures the PostgreSQL pool.
type DatabaseConfig struct {
	URL              Secret // application role (no DELETE/TRUNCATE privileges)
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	StatementTimeout time.Duration
	// LockTimeout is the session lock_timeout (TSP_DB_LOCK_TIMEOUT; also
	// mirrored in APIConfig.LockTimeout). A statement waiting longer for a
	// row lock fails with lock_not_available instead of piling up.
	LockTimeout time.Duration
	// IdleInTxTimeout is idle_in_transaction_session_timeout: a session left
	// idle inside an open transaction (a leaked tx) is terminated, releasing
	// its locks.
	IdleInTxTimeout time.Duration
}

// CryptoConfig points at key material used for envelope encryption and API
// key hashing.
type CryptoConfig struct {
	// MasterKeys holds the raw keyring file content ("<version>:<base64>" per line).
	MasterKeys Secret
	// ActiveKeyVersion is the version used for new encryptions; 0 = highest.
	ActiveKeyVersion int
	// APIKeyPepper is mixed into API key hashes so a database dump alone
	// cannot be used to verify guessed keys.
	APIKeyPepper Secret
}

// Source abstracts the process environment and filesystem for testing.
type Source struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	Stat     func(string) (fs.FileInfo, error)
}

// OSSource reads from the real process environment and filesystem.
func OSSource() Source {
	return Source{Getenv: os.Getenv, ReadFile: os.ReadFile, Stat: os.Stat}
}

// Load reads and validates the configuration needed by the api, worker and
// admin commands.
func Load(src Source) (*Config, error) {
	l := loader{src: src}

	cfg := &Config{}
	cfg.Env = Environment(l.str("TSP_ENV", string(EnvDevelopment)))
	switch cfg.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		l.fail("TSP_ENV", fmt.Errorf("must be development, staging or production, got %q", cfg.Env))
	}
	cfg.LogLevel = l.level("TSP_LOG_LEVEL", slog.LevelInfo)

	cfg.HTTP = HTTPConfig{
		Addr:              l.str("TSP_HTTP_ADDR", ":8088"),
		ReadHeaderTimeout: l.duration("TSP_HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:       l.duration("TSP_HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:      l.duration("TSP_HTTP_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:       l.duration("TSP_HTTP_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout:   l.duration("TSP_HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		DrainDelay:        l.nonNegDuration("TSP_HTTP_DRAIN_DELAY", 0),
		MaxBodyBytes:      l.int64("TSP_HTTP_MAX_BODY_BYTES", 1<<20),
	}
	cfg.Metrics = MetricsConfig{Addr: l.str("TSP_METRICS_ADDR", "127.0.0.1:9464")}
	cfg.Admin = AdminConfig{
		Addr:            l.str("TSP_ADMIN_ADDR", "127.0.0.1:8089"),
		SetupBaseURL:    l.str("TSP_ADMIN_SETUP_BASE_URL", "http://127.0.0.1:3001/setup/"),
		IdleTimeout:     l.duration("TSP_ADMIN_IDLE_TIMEOUT", 15*time.Minute),
		AbsoluteTimeout: l.duration("TSP_ADMIN_ABSOLUTE_TIMEOUT", 8*time.Hour),
	}
	cfg.Public = PublicConfig{
		Paybill:    l.str("TSP_MPESA_PAYBILL", ""),
		PayBaseURL: l.str("TSP_PAY_BASE_URL", "http://127.0.0.1:3000/"),
	}
	lockTimeout := l.duration("TSP_DB_LOCK_TIMEOUT", 5*time.Second)
	cfg.API = APIConfig{
		RatePerKey:     float64(l.int64("TSP_API_RATE_PER_KEY", 50)),
		BurstPerKey:    int(l.int64("TSP_API_BURST_PER_KEY", 100)),
		MaxInFlight:    int(l.int64("TSP_API_MAX_IN_FLIGHT", 512)),
		RequestTimeout: l.duration("TSP_API_REQUEST_TIMEOUT", 15*time.Second),
		LockTimeout:    lockTimeout,
	}
	cfg.Worker = WorkerConfig{
		WebhookConcurrency: int(l.int64("TSP_WORKER_WEBHOOK_CONCURRENCY", 50)),
		WebhookTimeout:     l.duration("TSP_WEBHOOK_TIMEOUT", 10*time.Second),
		MaintenanceEvery:   l.duration("TSP_WORKER_MAINTENANCE_EVERY", time.Minute),
		AttemptRetention:   l.duration("TSP_WEBHOOK_ATTEMPT_RETENTION", 90*24*time.Hour),
		HealthAddr:         l.str("TSP_WORKER_HEALTH_ADDR", "127.0.0.1:9465"),
		ShutdownTimeout:    l.duration("TSP_WORKER_SHUTDOWN_TIMEOUT", 25*time.Second),
	}
	cfg.TrustedProxies = l.prefixes("TSP_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")

	cfg.Database = DatabaseConfig{
		URL:              l.secret("TSP_DATABASE_URL", cfg.Env, true),
		MaxConns:         int32(l.int64("TSP_DB_MAX_CONNS", 20)),
		MinConns:         int32(l.int64("TSP_DB_MIN_CONNS", 2)),
		MaxConnLifetime:  l.duration("TSP_DB_MAX_CONN_LIFETIME", time.Hour),
		StatementTimeout: l.duration("TSP_DB_STATEMENT_TIMEOUT", 15*time.Second),
		LockTimeout:      lockTimeout,
		IdleInTxTimeout:  l.duration("TSP_DB_IDLE_IN_TX_TIMEOUT", 60*time.Second),
	}
	cfg.Crypto = CryptoConfig{
		MasterKeys:       l.secret("TSP_MASTER_KEYS", cfg.Env, true),
		ActiveKeyVersion: int(l.int64("TSP_MASTER_KEY_ACTIVE_VERSION", 0)),
		APIKeyPepper:     l.secret("TSP_API_KEY_PEPPER", cfg.Env, true),
	}

	// Report parse and validation problems together so operators can fix
	// everything in one pass.
	if err := errors.Join(l.err(), cfg.validate()); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadMigrate reads only what the migrate command needs: the owner-role DSN.
func LoadMigrate(src Source) (env Environment, dsn Secret, err error) {
	l := loader{src: src}
	env = Environment(l.str("TSP_ENV", string(EnvDevelopment)))
	dsn = l.secret("TSP_MIGRATE_DATABASE_URL", env, true)
	if err := l.err(); err != nil {
		return "", Secret{}, err
	}
	if err := validateDSN("TSP_MIGRATE_DATABASE_URL", dsn, env); err != nil {
		return "", Secret{}, err
	}
	return env, dsn, nil
}

func (c *Config) validate() error {
	var errs []error
	if err := validateDSN("TSP_DATABASE_URL", c.Database.URL, c.Env); err != nil {
		errs = append(errs, err)
	}
	if c.Database.MaxConns < 1 || c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		errs = append(errs, errors.New("TSP_DB_MIN_CONNS/TSP_DB_MAX_CONNS: need 0 <= min <= max and max >= 1"))
	}
	if c.HTTP.MaxBodyBytes < 1024 {
		errs = append(errs, errors.New("TSP_HTTP_MAX_BODY_BYTES: must be at least 1024"))
	}
	if len(c.Crypto.APIKeyPepper.Reveal()) < 32 {
		errs = append(errs, errors.New("TSP_API_KEY_PEPPER: must be at least 32 characters"))
	}
	if c.Metrics.Addr == c.HTTP.Addr {
		// Metrics must never be served on the public listener.
		errs = append(errs, errors.New("TSP_METRICS_ADDR: must differ from TSP_HTTP_ADDR"))
	}
	if c.Admin.Addr == c.HTTP.Addr || c.Admin.Addr == c.Metrics.Addr {
		// The admin API must never share the public listener.
		errs = append(errs, errors.New("TSP_ADMIN_ADDR: must differ from TSP_HTTP_ADDR and TSP_METRICS_ADDR"))
	}
	if u, err := url.Parse(c.Admin.SetupBaseURL); err != nil || u.Host == "" || !strings.HasSuffix(u.Path, "/") ||
		(u.Scheme != "https" && (c.Env.IsProductionLike() || u.Scheme != "http")) {
		errs = append(errs, errors.New("TSP_ADMIN_SETUP_BASE_URL: must be an absolute URL ending in / (https outside development)"))
	}
	if c.Admin.IdleTimeout > c.Admin.AbsoluteTimeout || c.Admin.AbsoluteTimeout > 24*time.Hour {
		errs = append(errs, errors.New("TSP_ADMIN_IDLE_TIMEOUT/TSP_ADMIN_ABSOLUTE_TIMEOUT: need idle <= absolute <= 24h"))
	}
	if u, err := url.Parse(c.Public.PayBaseURL); err != nil || u.Host == "" || !strings.HasSuffix(u.Path, "/") ||
		(u.Scheme != "https" && (c.Env.IsProductionLike() || u.Scheme != "http")) {
		errs = append(errs, errors.New("TSP_PAY_BASE_URL: must be an absolute URL ending in / (https outside development)"))
	}
	if c.API.RatePerKey <= 0 || c.API.BurstPerKey < 1 || c.API.MaxInFlight < 1 {
		errs = append(errs, errors.New("TSP_API_RATE_PER_KEY, TSP_API_BURST_PER_KEY, TSP_API_MAX_IN_FLIGHT: must be positive"))
	}
	if c.API.RequestTimeout < time.Second || c.API.RequestTimeout > c.HTTP.WriteTimeout {
		errs = append(errs, errors.New("TSP_API_REQUEST_TIMEOUT: must be between 1s and TSP_HTTP_WRITE_TIMEOUT"))
	}
	if c.API.LockTimeout < 100*time.Millisecond || c.API.LockTimeout >= c.API.RequestTimeout {
		errs = append(errs, errors.New("TSP_DB_LOCK_TIMEOUT: must be at least 100ms and below TSP_API_REQUEST_TIMEOUT"))
	}
	if c.Database.LockTimeout > c.Database.StatementTimeout {
		errs = append(errs, errors.New("TSP_DB_LOCK_TIMEOUT: must not exceed TSP_DB_STATEMENT_TIMEOUT"))
	}
	if c.Database.IdleInTxTimeout < time.Second {
		errs = append(errs, errors.New("TSP_DB_IDLE_IN_TX_TIMEOUT: must be at least 1s"))
	}
	if c.HTTP.DrainDelay > time.Minute {
		errs = append(errs, errors.New("TSP_HTTP_DRAIN_DELAY: must be at most 1m"))
	}
	if c.Worker.WebhookConcurrency < 1 || c.Worker.WebhookConcurrency > 1000 {
		errs = append(errs, errors.New("TSP_WORKER_WEBHOOK_CONCURRENCY: must be 1-1000"))
	}
	if c.Worker.WebhookTimeout < time.Second || c.Worker.WebhookTimeout > time.Minute {
		errs = append(errs, errors.New("TSP_WEBHOOK_TIMEOUT: must be 1s-1m"))
	}
	if c.Worker.MaintenanceEvery < 10*time.Second || c.Worker.AttemptRetention < 24*time.Hour {
		errs = append(errs, errors.New("TSP_WORKER_MAINTENANCE_EVERY >= 10s and TSP_WEBHOOK_ATTEMPT_RETENTION >= 24h required"))
	}
	if c.Worker.ShutdownTimeout < time.Second || c.Worker.ShutdownTimeout > 5*time.Minute {
		errs = append(errs, errors.New("TSP_WORKER_SHUTDOWN_TIMEOUT: must be 1s-5m"))
	}
	if c.Public.Paybill != "" && !paybillRe.MatchString(c.Public.Paybill) {
		errs = append(errs, errors.New("TSP_MPESA_PAYBILL: must be 5-7 digits"))
	}
	return errors.Join(errs...)
}

var paybillRe = regexp.MustCompile(`^[0-9]{5,7}$`)

func validateDSN(name string, dsn Secret, env Environment) error {
	u, err := url.Parse(dsn.Reveal())
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// Do not include err: url.Parse errors echo the input, which holds the password.
		return fmt.Errorf("%s: must be a postgres:// URL", name)
	}
	if env.IsProductionLike() {
		mode := u.Query().Get("sslmode")
		if mode != "require" && mode != "verify-ca" && mode != "verify-full" {
			return fmt.Errorf("%s: sslmode must be require, verify-ca or verify-full in %s", name, env)
		}
	}
	return nil
}

// loader accumulates errors so operators see every problem at once.
type loader struct {
	src  Source
	errs []error
}

func (l *loader) fail(name string, err error) {
	l.errs = append(l.errs, fmt.Errorf("%s: %w", name, err))
}
func (l *loader) err() error { return errors.Join(l.errs...) }

func (l *loader) str(name, def string) string {
	if v := strings.TrimSpace(l.src.Getenv(name)); v != "" {
		return v
	}
	return def
}

func (l *loader) int64(name string, def int64) int64 {
	v := strings.TrimSpace(l.src.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail(name, errors.New("must be an integer"))
		return def
	}
	return n
}

func (l *loader) duration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(l.src.Getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail(name, errors.New("must be a positive Go duration such as 15s"))
		return def
	}
	return d
}

// nonNegDuration is duration but also accepts "0" (disabled).
func (l *loader) nonNegDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(l.src.Getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		l.fail(name, errors.New("must be a non-negative Go duration such as 5s"))
		return def
	}
	return d
}

func (l *loader) prefixes(name, def string) []netip.Prefix {
	v := strings.TrimSpace(l.src.Getenv(name))
	if v == "" {
		v = def
	}
	var out []netip.Prefix
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			l.fail(name, fmt.Errorf("invalid CIDR %q", part))
			continue
		}
		out = append(out, p.Masked())
	}
	return out
}

func (l *loader) level(name string, def slog.Level) slog.Level {
	v := strings.TrimSpace(l.src.Getenv(name))
	if v == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail(name, errors.New("must be debug, info, warn or error"))
		return def
	}
	return lvl
}

// secret reads NAME_FILE (preferred) or, in development only, NAME.
func (l *loader) secret(name string, env Environment, required bool) Secret {
	fileVar := name + "_FILE"
	path := strings.TrimSpace(l.src.Getenv(fileVar))
	direct := l.src.Getenv(name)

	switch {
	case path != "":
		if direct != "" {
			l.fail(name, fmt.Errorf("set only one of %s and %s", name, fileVar))
			return Secret{}
		}
		if env.IsProductionLike() && l.src.Stat != nil {
			if fi, err := l.src.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
				l.fail(fileVar, fmt.Errorf("file must not be readable by group or others (mode %v)", fi.Mode().Perm()))
				return Secret{}
			}
		}
		b, err := l.src.ReadFile(path)
		if err != nil {
			l.fail(fileVar, errors.New("cannot read secret file"))
			return Secret{}
		}
		return NewSecret(strings.TrimSpace(string(b)))
	case direct != "":
		if env.IsProductionLike() {
			l.fail(name, fmt.Errorf("secrets must be provided via %s in %s", fileVar, env))
			return Secret{}
		}
		return NewSecret(strings.TrimSpace(direct))
	case required:
		l.fail(name, fmt.Errorf("required (set %s)", fileVar))
	}
	return Secret{}
}
