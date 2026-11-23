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

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/store"
)

func TestProductIsolation(t *testing.T) {
	e := newEnv(t)
	a := e.product("BILL").Product
	b := e.product("HOST").Product
	payerB := e.payer(b.ID, "shared-ref")
	reqB := e.request(b.ID, payerB.ID, 1000)

	// Same external_ref in another product is a different payer.
	payerA := e.payer(a.ID, "shared-ref")
	if payerA.ID == payerB.ID || !strings.HasPrefix(payerA.AccountNumber, "BILL-") {
		t.Fatalf("payer collision across products: %+v vs %+v", payerA, payerB)
	}

	if _, err := e.payers.Get(e.ctx, a.ID, payerB.ID); !errors.Is(err, payer.ErrNotFound) {
		t.Fatalf("A read B's payer: %v", err)
	}
	if _, err := e.requests.Get(e.ctx, a.ID, reqB.ID); !errors.Is(err, paymentrequest.ErrNotFound) {
		t.Fatalf("A read B's request: %v", err)
	}
	if _, err := e.requests.Cancel(e.ctx, audit.Product(a.ID), a.ID, reqB.ID); !errors.Is(err, paymentrequest.ErrNotFound) {
		t.Fatalf("A cancelled B's request: %v", err)
	}
	_, err := e.requests.Create(e.ctx, audit.Product(a.ID), a.ID, paymentrequest.CreateParams{
		PayerAccountID: payerB.ID, ProductReference: "X", Description: "x", Amount: 10, IdempotencyKey: "x",
	})
	if !errors.Is(err, paymentrequest.ErrPayerNotFound) {
		t.Fatalf("A billed B's payer: %v", err)
	}

	// Even bypassing the services, the database refuses cross-product rows.
	execFails(t, e.db.App, "foreign key", `INSERT INTO payment_requests (id, product_id, payer_account_id, product_reference, description, amount, hosted_token, idempotency_key)
		VALUES ($1, $2, $3, 'X', 'x', 10, repeat('a', 52), 'x')`, uuid.Must(uuid.NewV7()), a.ID, payerB.ID)
	execFails(t, e.db.App, "foreign key", `INSERT INTO transactions (id, product_id, direction, kind, channel, provider, status, amount, provider_transaction_id, payer_account_id, completed_at)
		VALUES ($1, $2, 'in', 'payment', 'mpesa_c2b', 'mpesa', 'succeeded', 10, 'XPROD1', $3, now())`, uuid.Must(uuid.NewV7()), a.ID, payerB.ID)
	execFails(t, e.db.App, "prefix", `INSERT INTO payer_accounts (id, product_id, external_ref, account_number, account_number_norm, display_name)
		VALUES ($1, $2, 'z', 'HOST-1239', 'HOST1239', 'z')`, uuid.Must(uuid.NewV7()), a.ID)
}

func TestAccountNumbersAndRouting(t *testing.T) {
	e := newEnv(t)
	h := e.product("HOST").Product
	first := e.payer(h.ID, "c1")
	second := e.payer(h.ID, "c2")
	if first.AccountNumber == second.AccountNumber {
		t.Fatal("account number reused")
	}
	again := e.payer(h.ID, "c1") // update path keeps the number
	if again.AccountNumber != first.AccountNumber || again.ID != first.ID {
		t.Fatalf("upsert changed identity: %+v", again)
	}

	digits := strings.TrimPrefix(first.AccountNumber, "HOST-")
	q := store.New(e.db.App)
	for _, typed := range []string{first.AccountNumber, strings.ToLower(first.AccountNumber), "HOST" + digits, " host " + digits + " ", "Host-" + digits} {
		r, ok, err := e.payers.Route(e.ctx, q, typed)
		if err != nil || !ok || r.PayerID != first.ID || r.ProductID != h.ID || !r.Active {
			t.Fatalf("route %q: %+v ok=%v err=%v", typed, r, ok, err)
		}
	}
	// A one-digit typo fails the check digit instead of hitting someone else.
	typo := []byte(digits)
	typo[0] = '0' + (typo[0]-'0'+1)%10
	for _, typed := range []string{"HOST-" + string(typo), "HOST-99999", "BILL-" + digits, "", "pay rent"} {
		if _, ok, err := e.payers.Route(e.ctx, q, typed); ok || err != nil {
			t.Fatalf("route %q should not match (ok=%v err=%v)", typed, ok, err)
		}
	}
}

func TestPayerValidation(t *testing.T) {
	e := newEnv(t)
	h := e.product("HOST").Product
	cases := []payer.UpsertParams{
		{ExternalRef: "", DisplayName: "x"},
		{ExternalRef: "a", DisplayName: ""},
		{ExternalRef: "a", DisplayName: "x", Email: "not-an-email"},
		{ExternalRef: "a", DisplayName: "x", Phones: []string{"0812345678"}},
	}
	for _, c := range cases {
		if _, _, err := e.payers.Upsert(e.ctx, operator, h.ID, c); err == nil {
			t.Fatalf("accepted invalid payer %+v", c)
		}
	}
	p, _, err := e.payers.Upsert(e.ctx, operator, h.ID, payer.UpsertParams{
		ExternalRef: "ok", DisplayName: "Jane", Phones: []string{"+254 712 345 678", "0712345678", "0110000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Phones) != 2 || p.Phones[0] != "254712345678" || p.Phones[1] != "254110000000" {
		t.Fatalf("phones = %v", p.Phones)
	}
}

func TestAPIKeyAuthentication(t *testing.T) {
	e := newEnv(t)
	c := e.product("BILL")
	k, err := e.products.IssueAPIKey(e.ctx, operator, c.Product.ID, "isp-billing prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.products.Authenticate(e.ctx, k.Plaintext)
	if err != nil || p.ProductID != c.Product.ID || p.AccountPrefix != "BILL" {
		t.Fatalf("authenticate: %+v %v", p, err)
	}

	bad := []string{
		"", "garbage", k.Plaintext + "x",
		strings.Replace(k.Plaintext, "tsp_test_", "tsp_live_", 1), // wrong environment
		k.Plaintext[:len(k.Plaintext)-1] + flip(k.Plaintext[len(k.Plaintext)-1]),
	}
	for _, s := range bad {
		if _, err := e.products.Authenticate(e.ctx, s); !errors.Is(err, product.ErrUnauthorized) {
			t.Fatalf("accepted bad key %q: %v", s, err)
		}
	}

	// Expired and revoked keys stop working.
	soon := time.Now().Add(time.Hour)
	k2, err := e.products.IssueAPIKey(e.ctx, operator, c.Product.ID, "rotating", &soon)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.products.RevokeAPIKey(e.ctx, operator, c.Product.ID, k2.Key.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.products.Authenticate(e.ctx, k2.Plaintext); !errors.Is(err, product.ErrUnauthorized) {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if err := e.products.RevokeAPIKey(e.ctx, operator, c.Product.ID, k2.Key.KeyID); !errors.Is(err, product.ErrKeyNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
	// A key cannot be revoked through another product.
	other := e.product("HOST")
	if err := e.products.RevokeAPIKey(e.ctx, operator, other.Product.ID, k.Key.KeyID); !errors.Is(err, product.ErrKeyNotFound) {
		t.Fatalf("cross-product revoke: %v", err)
	}

	// Suspending the product disables its keys.
	if _, err := e.db.App.Exec(e.ctx, `UPDATE products SET status = 'suspended' WHERE id = $1`, c.Product.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.products.Authenticate(e.ctx, k.Plaintext); !errors.Is(err, product.ErrUnauthorized) {
		t.Fatalf("suspended product key accepted: %v", err)
	}
}

func TestHTTPAuthAndWhoami(t *testing.T) {
	e := newEnv(t)
	c := e.product("HOST")
	k, err := e.products.IssueAPIKey(e.ctx, operator, c.Product.ID, "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: e.db.App, Products: e.products, MaxBodyBytes: 1 << 20, Version: "test",
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
	if resp.StatusCode != 200 || body["account_prefix"] != "HOST" || body["product_id"] != c.Product.ID.String() {
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
