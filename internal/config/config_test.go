package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

type fakeFile struct{ mode fs.FileMode }

func (f fakeFile) Name() string       { return "f" }
func (f fakeFile) Size() int64        { return 0 }
func (f fakeFile) Mode() fs.FileMode  { return f.mode }
func (f fakeFile) ModTime() time.Time { return time.Time{} }
func (f fakeFile) IsDir() bool        { return false }
func (f fakeFile) Sys() any           { return nil }

func src(env map[string]string, files map[string]string, mode fs.FileMode) Source {
	return Source{
		Getenv: func(k string) string { return env[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		},
		Stat: func(string) (fs.FileInfo, error) { return fakeFile{mode}, nil },
	}
}

var pepper = strings.Repeat("p", 40)

func TestDevelopmentAllowsDirectEnv(t *testing.T) {
	cfg, err := Load(src(map[string]string{
		"TSP_DATABASE_URL": "postgres://u:p@h/db?sslmode=disable", "TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper,
	}, nil, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvDevelopment || cfg.HTTP.Addr != ":8088" || cfg.Database.URL.Reveal() == "" {
		t.Fatalf("%+v", cfg)
	}
}

func TestProductionRequiresFilesTLSAndPerms(t *testing.T) {
	env := map[string]string{"TSP_ENV": "production", "TSP_DATABASE_URL": "postgres://u:p@h/db?sslmode=require",
		"TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper}
	_, err := Load(src(env, nil, 0o600))
	if err == nil || !strings.Contains(err.Error(), "TSP_DATABASE_URL_FILE") {
		t.Fatalf("direct secret accepted in production: %v", err)
	}

	files := map[string]string{"/s/db": "postgres://u:p@h/db?sslmode=disable", "/s/k": "1:x", "/s/p": pepper}
	envF := map[string]string{"TSP_ENV": "production", "TSP_DATABASE_URL_FILE": "/s/db", "TSP_MASTER_KEYS_FILE": "/s/k", "TSP_API_KEY_PEPPER_FILE": "/s/p"}
	if _, err := Load(src(envF, files, 0o600)); err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("plaintext DB connection accepted in production: %v", err)
	}
	files["/s/db"] = "postgres://u:p@h/db?sslmode=verify-full"
	if _, err := Load(src(envF, files, 0o644)); err == nil || !strings.Contains(err.Error(), "readable") {
		t.Fatalf("world-readable secret accepted: %v", err)
	}
	if _, err := Load(src(envF, files, 0o400)); err == nil || !strings.Contains(err.Error(), "TSP_ADMIN_SETUP_BASE_URL") {
		t.Fatalf("http setup link accepted in production: %v", err)
	}
	envF["TSP_ADMIN_SETUP_BASE_URL"] = "https://admin.talksasa.co.ke/setup/"
	envF["TSP_PAY_BASE_URL"] = "https://pay.talksasa.co.ke/"
	if _, err := Load(src(envF, files, 0o400)); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
}

func TestAdminListenerSeparationAndPaybill(t *testing.T) {
	base := map[string]string{"TSP_DATABASE_URL": "postgres://u:p@h/db", "TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for k, v := range map[string]string{
		"TSP_ADMIN_ADDR":      ":8088", // same as public listener
		"TSP_MPESA_PAYBILL":   "12ab",
		"TSP_TRUSTED_PROXIES": "10.0.0.0/33",
	} {
		if _, err := Load(src(with(k, v), nil, 0o600)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s accepted: %v", k, v, err)
		}
	}
	cfg, err := Load(src(with("TSP_MPESA_PAYBILL", "4123456"), nil, 0o600))
	if err != nil || cfg.Public.Paybill != "4123456" || len(cfg.TrustedProxies) != 2 {
		t.Fatalf("%v %+v", err, cfg)
	}
}

func TestErrorsNeverEchoSecrets(t *testing.T) {
	_, err := Load(src(map[string]string{
		"TSP_DATABASE_URL": "mysql://user:TOPSECRET@h/db", "TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": "short-TOPSECRET",
		"TSP_HTTP_READ_TIMEOUT": "soon",
	}, nil, 0o600))
	if err == nil {
		t.Fatal("expected errors")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("secret in error: %v", err)
	}
	for _, want := range []string{"TSP_DATABASE_URL", "TSP_API_KEY_PEPPER", "TSP_HTTP_READ_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestSecretNeverFormats(t *testing.T) {
	s := NewSecret("TOPSECRET")
	b, _ := json.Marshal(struct{ S Secret }{s})
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), string(b)} {
		if strings.Contains(out, "TOPSECRET") {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	if s.Reveal() != "TOPSECRET" {
		t.Fatal("Reveal")
	}
}

func TestDatabaseSessionTimeoutsAndDrainDelay(t *testing.T) {
	base := map[string]string{"TSP_DATABASE_URL": "postgres://u:p@h/db", "TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper}
	cfg, err := Load(src(base, nil, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.LockTimeout != 5*time.Second || cfg.API.LockTimeout != cfg.Database.LockTimeout ||
		cfg.Database.IdleInTxTimeout != time.Minute || cfg.HTTP.DrainDelay != 0 {
		t.Fatalf("defaults: %+v %+v %+v", cfg.Database, cfg.API, cfg.HTTP)
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cfg, err = Load(src(with("TSP_DB_LOCK_TIMEOUT", "750ms", "TSP_DB_IDLE_IN_TX_TIMEOUT", "30s", "TSP_HTTP_DRAIN_DELAY", "3s"), nil, 0o600))
	if err != nil || cfg.Database.LockTimeout != 750*time.Millisecond || cfg.API.LockTimeout != 750*time.Millisecond ||
		cfg.Database.IdleInTxTimeout != 30*time.Second || cfg.HTTP.DrainDelay != 3*time.Second {
		t.Fatalf("%v %+v", err, cfg)
	}
	if _, err := Load(src(with("TSP_HTTP_DRAIN_DELAY", "0"), nil, 0o600)); err != nil {
		t.Fatalf("drain delay 0 rejected: %v", err)
	}
	for _, kv := range [][]string{
		{"TSP_HTTP_DRAIN_DELAY", "-1s"},
		{"TSP_HTTP_DRAIN_DELAY", "2m"},
		{"TSP_DB_IDLE_IN_TX_TIMEOUT", "500ms"},
		{"TSP_DB_LOCK_TIMEOUT", "4s", "TSP_DB_STATEMENT_TIMEOUT", "2s"},
	} {
		if _, err := Load(src(with(kv...), nil, 0o600)); err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%v accepted: %v", kv, err)
		}
	}
}

func TestWebhookAllowLoopbackDevelopmentOnly(t *testing.T) {
	dev := map[string]string{"TSP_DATABASE_URL": "postgres://u:p@h/db?sslmode=disable", "TSP_MASTER_KEYS": "1:x",
		"TSP_API_KEY_PEPPER": pepper, "TSP_WEBHOOK_ALLOW_LOOPBACK": "true"}
	cfg, err := Load(src(dev, nil, 0o600))
	if err != nil || !cfg.Worker.WebhookAllowLoopback {
		t.Fatalf("development: %v %+v", err, cfg)
	}
	dev["TSP_WEBHOOK_ALLOW_LOOPBACK"] = "maybe"
	if _, err := Load(src(dev, nil, 0o600)); err == nil || !strings.Contains(err.Error(), "TSP_WEBHOOK_ALLOW_LOOPBACK") {
		t.Fatalf("bad bool accepted: %v", err)
	}
	files := map[string]string{"/s/db": "postgres://u:p@h/db?sslmode=require", "/s/k": "1:x", "/s/p": pepper}
	staging := map[string]string{"TSP_ENV": "staging", "TSP_DATABASE_URL_FILE": "/s/db", "TSP_MASTER_KEYS_FILE": "/s/k",
		"TSP_API_KEY_PEPPER_FILE": "/s/p", "TSP_PAY_BASE_URL": "https://pay.example/", "TSP_ADMIN_SETUP_BASE_URL": "https://a.example/setup/",
		"TSP_WEBHOOK_ALLOW_LOOPBACK": "true"}
	if _, err := Load(src(staging, files, 0o600)); err == nil || !strings.Contains(err.Error(), "TSP_WEBHOOK_ALLOW_LOOPBACK: only allowed in development") {
		t.Fatalf("loopback webhooks accepted in staging: %v", err)
	}
}

func TestPortAndSecretsFromEnv(t *testing.T) {
	base := map[string]string{"TSP_DATABASE_URL": "postgres://u:p@h/db?sslmode=disable", "TSP_MASTER_KEYS": "1:x",
		"TSP_API_KEY_PEPPER": pepper, "PORT": "9090"}
	cfg, err := Load(src(base, nil, 0o600))
	if err != nil || cfg.HTTP.Addr != ":9090" {
		t.Fatalf("PORT: %v %q", err, cfg.HTTP.Addr)
	}
	base["TSP_HTTP_ADDR"] = "127.0.0.1:7000"
	if cfg, err = Load(src(base, nil, 0o600)); err != nil || cfg.HTTP.Addr != "127.0.0.1:7000" {
		t.Fatalf("TSP_HTTP_ADDR must win over PORT: %v %q", err, cfg.HTTP.Addr)
	}

	prod := map[string]string{"TSP_ENV": "production", "TSP_DATABASE_URL": "postgres://u:p@h/db?sslmode=require",
		"TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper,
		"TSP_PAY_BASE_URL": "https://pay.example/", "TSP_ADMIN_SETUP_BASE_URL": "https://a.example/setup/"}
	if _, err := Load(src(prod, nil, 0o600)); err == nil || !strings.Contains(err.Error(), "TSP_SECRETS_FROM_ENV") {
		t.Fatalf("production must refuse env secrets by default: %v", err)
	}
	prod["TSP_SECRETS_FROM_ENV"] = "true"
	cfg, err = Load(src(prod, nil, 0o600))
	if err != nil || !cfg.SecretsFromEnv || cfg.Database.URL.Reveal() == "" {
		t.Fatalf("opt-in: %v %+v", err, cfg)
	}
	prod["TSP_DATABASE_URL"] = "postgres://u:p@h/db?sslmode=disable"
	if _, err := Load(src(prod, nil, 0o600)); err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("the opt-in must not relax database TLS: %v", err)
	}
}

// Hosts that manage the database (Talksasa Cloud) provide DATABASE_URL and
// rotate its password; the app follows it unless TSP_DATABASE_URL is set.
func TestPlatformDatabaseURL(t *testing.T) {
	prod := map[string]string{"TSP_ENV": "production", "TSP_SECRETS_FROM_ENV": "true",
		"DATABASE_URL":    "postgresql://u:p@db-sidecar:5432/s_db",
		"TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper,
		"TSP_PAY_BASE_URL": "https://pay.example/", "TSP_ADMIN_SETUP_BASE_URL": "https://a.example/setup/"}
	cfg, err := Load(src(prod, nil, 0o600))
	if err != nil || cfg.Database.URL.Reveal() != "postgresql://u:p@db-sidecar:5432/s_db?sslmode=require" || cfg.DBPlaintext {
		t.Fatalf("DATABASE_URL fallback defaults to sslmode=require: %v %q", err, cfg.Database.URL.Reveal())
	}
	if _, dsn, err := LoadMigrate(src(prod, nil, 0o600)); err != nil || dsn.Reveal() != "postgresql://u:p@db-sidecar:5432/s_db?sslmode=require" {
		t.Fatalf("migrate uses DATABASE_URL too: %v", err)
	}

	prod["TSP_DB_SSLMODE"] = "disable"
	cfg, err = Load(src(prod, nil, 0o600))
	if err != nil || !cfg.DBPlaintext || !strings.HasSuffix(cfg.Database.URL.Reveal(), "sslmode=disable") {
		t.Fatalf("explicit TSP_DB_SSLMODE=disable on a private network: %v %+v", err, cfg)
	}
	if _, _, err := LoadMigrate(src(prod, nil, 0o600)); err != nil {
		t.Fatalf("migrate with explicit disable: %v", err)
	}

	// sslmode=disable inside the URL alone is still refused in production.
	delete(prod, "TSP_DB_SSLMODE")
	prod["DATABASE_URL"] = "postgresql://u:p@db-sidecar:5432/s_db?sslmode=disable"
	if _, err := Load(src(prod, nil, 0o600)); err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("a disable hidden in the URL must still be refused: %v", err)
	}
	prod["TSP_DB_SSLMODE"] = "off"
	if _, err := Load(src(prod, nil, 0o600)); err == nil || !strings.Contains(err.Error(), "TSP_DB_SSLMODE") {
		t.Fatalf("bad TSP_DB_SSLMODE accepted: %v", err)
	}

	// Without the env-secrets opt-in, DATABASE_URL is refused in production.
	delete(prod, "TSP_SECRETS_FROM_ENV")
	prod["TSP_DB_SSLMODE"] = "require"
	if _, err := Load(src(prod, nil, 0o600)); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("DATABASE_URL without TSP_SECRETS_FROM_ENV: %v", err)
	}
	// An explicit TSP_DATABASE_URL wins over DATABASE_URL.
	dev := map[string]string{"DATABASE_URL": "postgres://a:b@one/db", "TSP_DATABASE_URL": "postgres://a:b@two/db",
		"TSP_MASTER_KEYS": "1:x", "TSP_API_KEY_PEPPER": pepper}
	if cfg, err := Load(src(dev, nil, 0o600)); err != nil || !strings.Contains(cfg.Database.URL.Reveal(), "@two/") {
		t.Fatalf("TSP_DATABASE_URL must win: %v", err)
	}
}
