package integrationtest

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/store"
)

func TestAppIsolation(t *testing.T) {
	e := newEnv(t)
	a := e.app("BILL").App
	b := e.app("HOST").App
	chB := e.charge(b.ID, 1000)
	q := store.New(e.db.App)

	if _, err := q.GetChargeForApp(e.ctx, store.GetChargeForAppParams{AppID: a.ID, ID: chB.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("A read B's charge: %v", err)
	}
	if _, err := q.CancelCharge(e.ctx, store.CancelChargeParams{AppID: a.ID, ID: chB.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("A cancelled B's charge: %v", err)
	}

	// Even bypassing the services, the database refuses cross-app rows.
	execFails(t, e.db.App, "foreign key", `INSERT INTO transactions (id, app_id, charge_id, direction, kind, channel, provider, status, amount, provider_transaction_id, completed_at)
		VALUES ($1, $2, $3, 'in', 'payment', 'mpesa_c2b', 'mpesa', 'succeeded', 10, 'XAPP1', now())`, uuid.Must(uuid.NewV7()), a.ID, chB.ID)
	execFails(t, e.db.App, "prefix", `INSERT INTO charges (id, app_id, reference, amount, account_number, account_number_norm, account_generated,
		hosted_token, idempotency_key, request_hash, expires_at)
		VALUES ($1, $2, 'X', 10, 'HOST-1', 'HOST1', false, repeat('a', 52), 'x', '\x00', now() + interval '1 day')`, uuid.Must(uuid.NewV7()), a.ID)
	// A prompt of one app cannot name another app's charge.
	execFails(t, e.db.App, "foreign key", `INSERT INTO stk_pushes (id, app_id, charge_id, transaction_id, source, phone, amount)
		VALUES ($1, $2, $3, $4, 'pay_page', '254712345678', 10)`, uuid.Must(uuid.NewV7()), a.ID, chB.ID, uuid.Must(uuid.NewV7()))
}

func TestAPIKeyAuthentication(t *testing.T) {
	e := newEnv(t)
	c := e.app("BILL")
	k, err := e.apps.IssueAPIKey(e.ctx, operator, c.App.ID, "isp-billing prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.apps.Authenticate(e.ctx, k.Plaintext)
	if err != nil || p.AppID != c.App.ID || p.AccountPrefix != "BILL" {
		t.Fatalf("authenticate: %+v %v", p, err)
	}

	bad := []string{
		"", "garbage", k.Plaintext + "x",
		strings.Replace(k.Plaintext, "tsp_test_", "tsp_live_", 1), // wrong environment
		k.Plaintext[:len(k.Plaintext)-1] + flip(k.Plaintext[len(k.Plaintext)-1]),
	}
	for _, s := range bad {
		if _, err := e.apps.Authenticate(e.ctx, s); !errors.Is(err, app.ErrUnauthorized) {
			t.Fatalf("accepted bad key %q: %v", s, err)
		}
	}

	// Expired and revoked keys stop working.
	soon := time.Now().Add(time.Hour)
	k2, err := e.apps.IssueAPIKey(e.ctx, operator, c.App.ID, "rotating", &soon)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.apps.RevokeAPIKey(e.ctx, operator, c.App.ID, k2.Key.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.Authenticate(e.ctx, k2.Plaintext); !errors.Is(err, app.ErrUnauthorized) {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if err := e.apps.RevokeAPIKey(e.ctx, operator, c.App.ID, k2.Key.KeyID); !errors.Is(err, app.ErrKeyNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
	// A key cannot be revoked through another app.
	other := e.app("HOST")
	if err := e.apps.RevokeAPIKey(e.ctx, operator, other.App.ID, k.Key.KeyID); !errors.Is(err, app.ErrKeyNotFound) {
		t.Fatalf("cross-app revoke: %v", err)
	}

	// Suspending the app disables its keys.
	if _, err := e.db.App.Exec(e.ctx, `UPDATE apps SET status = 'suspended' WHERE id = $1`, c.App.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.Authenticate(e.ctx, k.Plaintext); !errors.Is(err, app.ErrUnauthorized) {
		t.Fatalf("suspended app key accepted: %v", err)
	}
}

func TestHTTPAuthAndWhoami(t *testing.T) {
	e := newEnv(t)
	c := e.app("HOST")
	k, err := e.apps.IssueAPIKey(e.ctx, operator, c.App.ID, "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: e.db.App, Apps: e.apps, MaxBodyBytes: 1 << 20, Version: "test",
	}))
	defer srv.Close()

	do := func(key string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp, body
	}

	resp, body := do("")
	if resp.StatusCode != 401 || resp.Header.Get("Content-Type") != "application/problem+json" || body["request_id"] == "" {
		t.Fatalf("no key: %d %v", resp.StatusCode, body)
	}
	resp, _ = do("tsp_test_aaaaaaaaaaaaaaaaaaaa_" + strings.Repeat("a", 52))
	if resp.StatusCode != 401 {
		t.Fatalf("unknown key: %d", resp.StatusCode)
	}
	resp, body = do(k.Plaintext)
	if resp.StatusCode != 200 || body["account_prefix"] != "HOST" || body["app_id"] != c.App.ID.String() {
		t.Fatalf("whoami: %d %v", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") == "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing request id or security headers")
	}
	// The presented key never appears in logs.
	if strings.Contains(e.logs.String(), k.Plaintext) {
		t.Fatal("API key leaked into logs")
	}
}

func flip(b byte) string {
	if b == 'a' {
		return "b"
	}
	return "a"
}
