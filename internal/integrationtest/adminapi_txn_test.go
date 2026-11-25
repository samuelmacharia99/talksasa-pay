package integrationtest

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
)

// The dashboard counts against real rows: pending, succeeded today (count
// and amount), needs_review, unmatched (count and amount) and payments for
// an inactive app.
func TestAdminAPIDashboardCounts(t *testing.T) {
	e := newAPIEnv(t)
	tok := e.signIn(e.onboard("dash@talksasa.co.ke", store.AdminRoleViewer)).Token
	a := e.app("DSH")
	old := e.app("OLD")

	e.charge(a.App.ID, 100) // pending
	paid1, paid2 := e.charge(a.App.ID, 1500), e.charge(a.App.ID, 2500)
	e.payCharge(paid1)
	e.payCharge(paid2)
	review := e.charge(a.App.ID, 900)
	if _, err := store.New(e.db.App).MarkChargeNeedsReview(e.ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	seed.Payment(t, e.ctx, e.db.App, nil, "ZZZ-1", 700)
	seed.Payment(t, e.ctx, e.db.App, nil, "ZZZ-2", 300)
	seed.Payment(t, e.ctx, e.db.App, &old.App.ID, "OLD-1", 450)
	if _, err := e.apps.SetStatus(e.ctx, operator, old.App.ID, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}

	r := call(e.admin, "GET", "/admin/v1/dashboard", tok, nil)
	if r.code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", r.code, r.body)
	}
	m := r.json(t)
	counts := m["counts"].(map[string]any)
	want := map[string]float64{
		"charges_pending": 1, "charges_succeeded_today": 2, "charges_succeeded_today_amount": 4000,
		"charges_needs_review": 1, "unmatched_count": 2, "unmatched_amount": 1000, "inactive_app_payments": 1,
		"unprocessed_callbacks": 0, "transactions_needs_review": 0,
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%s] = %v, want %v", k, counts[k], v)
		}
	}
	// Collections include unmatched money (under the nil UUID).
	today := m["today"].(map[string]any)
	if today["total"] != float64(1500+2500+700+300+450) || today["payments"] != float64(5) {
		t.Fatalf("today: %v", today)
	}
	if by := today["by_app"].(map[string]any); by[uuid.Nil.String()] != float64(1000) || by[a.App.ID.String()] != float64(4000) {
		t.Fatalf("by_app: %v", by)
	}
	if len(m["apps"].([]any)) != 2 || len(m["daily"].([]any)) != 30 {
		t.Fatalf("apps/daily: %v %d", m["apps"], len(m["daily"].([]any)))
	}
}

// The checks the admin API makes before touching the ledger, against real
// rows: who may call, what can be assigned or reversed (409) and input
// validation (422). They hold with or without the money core.
func TestAdminAPIAssignAndReverseGuards(t *testing.T) {
	e := newAPIEnv(t)
	adminTok := e.signIn(e.onboard("admin-guards@talksasa.co.ke", store.AdminRoleAdmin)).Token
	finTok := e.signIn(e.onboard("fin-guards@talksasa.co.ke", store.AdminRoleFinance)).Token
	viewTok := e.signIn(e.onboard("view-guards@talksasa.co.ke", store.AdminRoleViewer)).Token
	a := e.app("GRD")
	c := e.charge(a.App.ID, 1200)
	paid := e.payCharge(c)
	unmatched := seed.Payment(t, e.ctx, e.db.App, nil, "NOPE-1", 800)
	failed := failedSTK(t, e, a.App.ID)

	assign := func(tok string, id, appID uuid.UUID) resp {
		return call(e.admin, "POST", "/admin/v1/transactions/"+id.String()+"/assign", tok,
			map[string]any{"app_id": appID.String(), "reason": "Customer typed the wrong prefix"})
	}
	reverse := func(tok string, id uuid.UUID, body map[string]any) resp {
		return call(e.admin, "POST", "/admin/v1/transactions/"+id.String()+"/reverse", tok, body)
	}
	revBody := map[string]any{"reason": "Safaricom reversed it", "receipt": "SRV0000001"}

	// RBAC: finance may assign, not reverse; viewers neither.
	if r := reverse(finTok, paid.ID, revBody); r.code != http.StatusForbidden {
		t.Fatalf("finance reverse: %d %s", r.code, r.body)
	}
	if r := assign(viewTok, unmatched.ID, a.App.ID); r.code != http.StatusForbidden {
		t.Fatalf("viewer assign: %d %s", r.code, r.body)
	}
	if r := reverse(viewTok, paid.ID, revBody); r.code != http.StatusForbidden {
		t.Fatalf("viewer reverse: %d %s", r.code, r.body)
	}

	// 409s.
	for name, tc := range map[string]struct {
		r    resp
		code string
	}{
		"assign matched":    {assign(finTok, paid.ID, a.App.ID), "already_assigned"},
		"assign failed":     {assign(adminTok, failed.ID, a.App.ID), "already_assigned"},
		"reverse unmatched": {reverse(adminTok, unmatched.ID, revBody), "not_reversible"},
		"reverse failed":    {reverse(adminTok, failed.ID, revBody), "not_reversible"},
	} {
		if tc.r.code != http.StatusConflict || tc.r.json(t)["code"] != tc.code {
			t.Errorf("%s: %d %s", name, tc.r.code, tc.r.body)
		}
	}
	// 404 and 422.
	if r := assign(finTok, uuid.New(), a.App.ID); r.code != http.StatusNotFound {
		t.Errorf("unknown txn: %d %s", r.code, r.body)
	}
	if r := assign(finTok, unmatched.ID, uuid.New()); r.code != http.StatusUnprocessableEntity {
		t.Errorf("unknown app: %d %s", r.code, r.body)
	}
	if r := reverse(adminTok, paid.ID, map[string]any{"reason": "no receipt"}); r.code != http.StatusUnprocessableEntity {
		t.Errorf("M-Pesa reversal without receipt: %d %s", r.code, r.body)
	}
	// Nothing changed.
	if n := e.count(`SELECT count(*) FROM transactions WHERE kind = 'reversal'`); n != 0 {
		t.Fatalf("%d reversals recorded", n)
	}
	if n := e.count(`SELECT count(*) FROM transactions WHERE id = $1 AND app_id IS NULL`, unmatched.ID); n != 1 {
		t.Fatal("unmatched payment was assigned")
	}
}

// failedSTK records a failed STK attempt for a fresh charge of appID.
func failedSTK(t *testing.T, e *apiEnv, appID uuid.UUID) store.Transaction {
	t.Helper()
	c := e.charge(appID, 650)
	var out store.Transaction
	err := db.WithTx(e.ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		txn, err := ledger.StartSTK(ctx, q, ledger.StartSTKParams{
			AppID: appID, ChargeID: c.ID, Amount: 650, Phone: "254712345678", AccountNumber: c.AccountNumber,
		})
		if err != nil {
			return err
		}
		out, err = ledger.FailSTK(ctx, q, txn.ID, "1032", "Request cancelled by user", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
