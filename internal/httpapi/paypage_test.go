package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
)

// The pay page shows what the customer needs to pay and nothing else: no
// metadata, phone, account ids, idempotency key or token-like fields.
func TestPayPageFieldsAndNoLeaks(t *testing.T) {
	e := newPayEnv(t, nil)
	c, err := e.apps.Create(e.ctx, operator, app.CreateParams{
		Name: "Leaky Hosting", AccountPrefix: "LEAK", WebhookURL: "https://leak.example.com/hooks",
	})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	ch := seed.Charge(t, e.ctx, e.db.App, c.App.ID, "LEAK", 4200, seed.ChargeOpts{
		Reference: "INV-77", Description: "November hosting", Phone: fullPhone, ExpiresAt: exp,
		Metadata: []byte(`{"customer_email":"jane@example.com","internal_note":"vip"}`),
	})
	r := e.get(ch.HostedToken)
	m := r.expect(t, 200, "")
	if m["app_name"] != "Leaky Hosting" || m["reference"] != "INV-77" || m["description"] != "November hosting" ||
		m["amount"].(float64) != 4200 || m["currency"] != "KES" || m["status"] != "pending" || m["payable"] != true ||
		m["succeeded_at"] != nil || m["stk_available"] != true || m["stk_push"] != nil {
		t.Fatalf("pay page %v", m)
	}
	if got, _ := time.Parse(time.RFC3339, m["expires_at"].(string)); !got.Equal(exp) {
		t.Fatalf("expires_at %v, want %v", m["expires_at"], exp)
	}
	if pb := m["paybill"].(map[string]any); pb["business_number"] != "4123456" || pb["account_number"] != ch.AccountNumber {
		t.Fatalf("paybill %v", pb)
	}
	if r.hdr.Get("X-Robots-Tag") == "" {
		t.Fatal("pay page must not be indexed")
	}
	assertNoLeak(t, r.body, bill{appID: c.App.ID, ch: ch},
		"jane@example.com", "internal_note", "metadata", ch.IdempotencyKey, ch.HostedToken, "app_id", "charge_id")

	// Past expiry (before the job runs): shown as expired, not payable, no paybill steps.
	old := seed.Charge(t, e.ctx, e.db.App, c.App.ID, "LEAK", 100, seed.ChargeOpts{ExpiresAt: time.Now().Add(-time.Minute)})
	m = e.get(old.HostedToken).expect(t, 200, "")
	if m["status"] != string(store.ChargeStatusExpired) || m["payable"] != false || m["paybill"] != nil || m["stk_available"] != false {
		t.Fatalf("expired %v", m)
	}
	e.push(old.HostedToken, `{"phone":"0712345678"}`).expect(t, 409, "not_payable")

	// A suspended app's links look exactly like unknown links.
	if _, err := e.apps.SetStatus(e.ctx, operator, c.App.ID, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	gone := e.get(ch.HostedToken)
	unknown := e.get(strings.Repeat("a", 52))
	gone.expect(t, 404, "not_found")
	unknown.expect(t, 404, "not_found")
	if gone.json(t)["detail"] != unknown.json(t)["detail"] {
		t.Fatal("suspended and unknown tokens differ")
	}
	for _, bad := range []string{"short", strings.Repeat("A", 52), strings.Repeat("a", 53), strings.Repeat("1", 52)} {
		e.get(bad).expect(t, 404, "not_found")
	}
}
