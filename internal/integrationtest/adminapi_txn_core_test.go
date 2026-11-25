//go:build gatewaycore

// TODO(gateway-core): drop the build tag once ledger.Service.AssignToApp and
// Reverse land (they are stubs on the gateway foundation).

package integrationtest

import (
	"net/http"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/adminapi"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
)

var _ adminapi.LedgerService = (*ledger.Service)(nil)

// Staff assign an unmatched payment and reverse a charge payment end to
// end: the money moves, the app gets its events and both actions are
// audited with the staff member and the reason.
func TestAdminAPIAssignAndReverse(t *testing.T) {
	e := newAPIEnv(t)
	if e.deps.Ledger == nil {
		t.Fatal("ledger not wired into the admin API")
	}
	adminS := e.onboard("admin-txn@talksasa.co.ke", store.AdminRoleAdmin)
	finS := e.onboard("fin-txn@talksasa.co.ke", store.AdminRoleFinance)
	adminTok, finTok := e.signIn(adminS).Token, e.signIn(finS).Token
	a := e.app("TXN")
	unmatched := seed.Payment(t, e.ctx, e.db.App, nil, "TXM-1", 800)
	c := e.charge(a.App.ID, 1200)
	paid := e.payCharge(c)

	// Assign (finance).
	r := call(e.admin, "POST", "/admin/v1/transactions/"+unmatched.ID.String()+"/assign", finTok,
		map[string]any{"app_id": a.App.ID.String(), "reason": "Customer typed TXM instead of TXN"})
	if r.code != http.StatusOK || r.json(t)["app_id"] != a.App.ID.String() || r.json(t)["app_status"] != "active" {
		t.Fatalf("assign: %d %s", r.code, r.body)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE app_id = $1 AND type = 'payment.received' AND subject_id = $2`, a.App.ID, unmatched.ID); n != 1 {
		t.Fatalf("payment.received events: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE actor_type = 'admin' AND actor_id = $1 AND entity_id = $2`,
		finS.id.String(), unmatched.ID.String()); n != 1 {
		t.Fatalf("assign audit rows: %d", n)
	}
	r = call(e.admin, "POST", "/admin/v1/transactions/"+unmatched.ID.String()+"/assign", finTok,
		map[string]any{"app_id": a.App.ID.String(), "reason": "again"})
	if r.code != http.StatusConflict || r.json(t)["code"] != "already_assigned" {
		t.Fatalf("reassign: %d %s", r.code, r.body)
	}

	// Reverse (admin only).
	body := map[string]any{"reason": "Safaricom reversed the payment", "receipt": "SRV7654321"}
	if r := call(e.admin, "POST", "/admin/v1/transactions/"+paid.ID.String()+"/reverse", finTok, body); r.code != http.StatusForbidden {
		t.Fatalf("finance reverse: %d", r.code)
	}
	r = call(e.admin, "POST", "/admin/v1/transactions/"+paid.ID.String()+"/reverse", adminTok, body)
	if r.code != http.StatusOK || r.json(t)["kind"] != "reversal" || r.json(t)["reverses_transaction_id"] != paid.ID.String() {
		t.Fatalf("reverse: %d %s", r.code, r.body)
	}
	if n := e.count(`SELECT count(*) FROM charges WHERE id = $1 AND status = 'reversed'`, c.ID); n != 1 {
		t.Fatal("charge not reversed")
	}
	if n := e.count(`SELECT count(*) FROM events WHERE app_id = $1 AND type = 'transaction.reversed'`, a.App.ID); n != 1 {
		t.Fatalf("transaction.reversed events: %d", n)
	}
	reversalID := r.json(t)["id"].(string)
	if n := e.count(`SELECT count(*) FROM audit_log WHERE actor_type = 'admin' AND actor_id = $1 AND entity_id IN ($2, $3)`,
		adminS.id.String(), paid.ID.String(), reversalID); n != 1 {
		t.Fatalf("reverse audit rows: %d", n)
	}
	r = call(e.admin, "POST", "/admin/v1/transactions/"+paid.ID.String()+"/reverse", adminTok, map[string]any{"reason": "twice", "receipt": "SRV7654322"})
	if r.code != http.StatusConflict || r.json(t)["code"] != "already_reversed" {
		t.Fatalf("reverse twice: %d %s", r.code, r.body)
	}
	e.assertAuditChain()
}
