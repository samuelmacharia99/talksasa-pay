package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const (
	settingsSecret = "cs_super_secret_value_9876"
	settingsKey    = "ck_super_secret_key_1234"
)

type fakeSettings struct {
	got     mpesasettings.SaveParams
	actor   audit.Actor
	confirm string
	saveErr error
}

func (f *fakeSettings) Status(context.Context) (mpesasettings.Status, error) {
	now := time.Now()
	return mpesasettings.Status{
		Source: mpesasettings.SourceDatabase, Environment: "sandbox", Paybill: "174379", CallbackBaseURL: "https://api.example/",
		ConsumerKey: mpesasettings.Credential{Set: true, Last4: "1234"}, ConsumerSecret: mpesasettings.Credential{Set: true},
		Passkey: mpesasettings.Credential{Set: true, Last4: "c919"}, CallbackTokenSet: true,
		CallbackURLs: mpesasettings.CallbackURLs{Confirmation: "https://api.example/cb/mpesa/[TOKEN]/c2b/confirmation"},
		Version:      2, UpdatedBy: "cli:Jane", UpdatedAt: &now, Ready: true, Environments: []string{"sandbox"},
	}, nil
}

func (f *fakeSettings) Save(ctx context.Context, actor audit.Actor, p mpesasettings.SaveParams) (mpesasettings.Status, error) {
	f.got, f.actor = p, actor
	if f.saveErr != nil {
		return mpesasettings.Status{}, f.saveErr
	}
	return f.Status(ctx)
}

func (f *fakeSettings) Test(context.Context, audit.Actor) (mpesasettings.TestResult, error) {
	return mpesasettings.TestResult{At: time.Now(), OK: false, Message: "mpesa: rejected (HTTP 400): Invalid credentials"}, nil
}

func (f *fakeSettings) RegisterURLs(_ context.Context, _ audit.Actor, confirm string) (mpesasettings.RegisterResult, error) {
	f.confirm = confirm
	if confirm != "production" {
		return mpesasettings.RegisterResult{}, mpesasettings.ErrConfirm
	}
	return mpesasettings.RegisterResult{At: time.Now(), ShortCode: "174379", ResponseType: "Completed",
		ConfirmationURL: "https://api.example/cb/mpesa/[TOKEN]/c2b/confirmation"}, nil
}

func settingsServer(t *testing.T, s SettingsService, role store.AdminRole) *httptest.Server {
	t.Helper()
	d := Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MpesaSettings: s}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), principalKey{}, admin.Principal{UserID: uuid.New(), Role: role})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	for _, rt := range Routes(d) {
		if strings.HasPrefix(rt.Pattern, "/settings/") {
			r.With(requirePerm(rt.Perm)).Method(rt.Method, rt.Pattern, rt.handler)
		}
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestSettingsRoutes(t *testing.T) {
	f := &fakeSettings{}
	srv := settingsServer(t, f, store.AdminRoleAdmin)

	code, body := do(t, http.MethodGet, srv.URL+"/settings/mpesa", "")
	creds, _ := body["credentials"].(map[string]any)
	ck, _ := creds["consumer_key"].(map[string]any)
	cs, _ := creds["consumer_secret"].(map[string]any)
	if code != 200 || body["source"] != "database" || ck["set"] != true || ck["last4"] != "1234" || cs["last4"] != nil {
		t.Fatalf("GET %d %v", code, body)
	}

	// Secrets go in, never come back out.
	code, body = do(t, http.MethodPut, srv.URL+"/settings/mpesa",
		`{"environment":"sandbox","shortcode":"174379","callback_base_url":"https://api.example/","consumer_key":"`+settingsKey+`","consumer_secret":"`+settingsSecret+`"}`)
	if code != 200 || f.got.ConsumerKey.Reveal() != settingsKey || f.got.ConsumerSecret.Reveal() != settingsSecret || !f.got.Passkey.IsZero() ||
		f.actor.Type != store.AuditActorTypeAdmin {
		t.Fatalf("PUT %d %v %+v", code, body, f.actor)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), settingsKey) || strings.Contains(string(raw), settingsSecret) {
		t.Fatalf("PUT response echoes a secret: %s", raw)
	}

	f.saveErr = &mpesasettings.ValidationError{Field: "shortcode", Message: "bad"}
	if code, body = do(t, http.MethodPut, srv.URL+"/settings/mpesa", `{"environment":"sandbox"}`); code != 422 || body["code"] != "validation_failed" {
		t.Fatalf("validation %d %v", code, body)
	}
	if code, _ = do(t, http.MethodPut, srv.URL+"/settings/mpesa", `{"environment":"sandbox","unknown":1}`); code != 400 {
		t.Fatalf("unknown field %d", code)
	}

	if code, body = do(t, http.MethodPost, srv.URL+"/settings/mpesa/test", ""); code != 200 || body["ok"] != false || body["message"] == nil {
		t.Fatalf("test %d %v", code, body)
	}
	if code, body = do(t, http.MethodPost, srv.URL+"/settings/mpesa/register-urls", ""); code != 422 {
		t.Fatalf("unconfirmed %d %v", code, body)
	}
	if code, body = do(t, http.MethodPost, srv.URL+"/settings/mpesa/register-urls", `{"confirm":"production"}`); code != 200 || f.confirm != "production" {
		t.Fatalf("register %d %v", code, body)
	}
}

func TestSettingsRBAC(t *testing.T) {
	for _, role := range []store.AdminRole{store.AdminRoleViewer, store.AdminRoleFinance, store.AdminRoleApprover} {
		srv := settingsServer(t, &fakeSettings{}, role)
		if code, _ := do(t, http.MethodGet, srv.URL+"/settings/mpesa", ""); code != 200 {
			t.Errorf("%s cannot read settings: %d", role, code)
		}
		for _, c := range []struct{ method, path, body string }{
			{http.MethodPut, "/settings/mpesa", `{}`},
			{http.MethodPost, "/settings/mpesa/test", ""},
			{http.MethodPost, "/settings/mpesa/register-urls", ""},
		} {
			if code, _ := do(t, c.method, srv.URL+c.path, c.body); code != 403 {
				t.Errorf("%s %s %s: %d, want 403", role, c.method, c.path, code)
			}
		}
	}
}
