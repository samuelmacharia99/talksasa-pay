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
