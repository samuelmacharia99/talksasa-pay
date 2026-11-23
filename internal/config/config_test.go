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
	if _, err := Load(src(envF, files, 0o400)); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
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
