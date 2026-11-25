package adminapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

type fakeLedger struct {
	calls     int
	gotActor  audit.Actor
	gotTxn    uuid.UUID
	gotApp    uuid.UUID
	gotReason string
	gotRev    ledger.ReverseParams
	assignFn  func() (ledger.Transaction, error)
	reverseFn func() (ledger.ReverseResult, error)
}

func (f *fakeLedger) AssignToApp(_ context.Context, actor audit.Actor, txnID, appID uuid.UUID, reason string) (ledger.Transaction, error) {
	f.calls++
	f.gotActor, f.gotTxn, f.gotApp, f.gotReason = actor, txnID, appID, reason
	return f.assignFn()
}

func (f *fakeLedger) Reverse(_ context.Context, actor audit.Actor, p ledger.ReverseParams) (ledger.ReverseResult, error) {
	f.calls++
	f.gotActor, f.gotRev = actor, p
	return f.reverseFn()
}

type fakeApps map[uuid.UUID]app.App

func (f fakeApps) Get(_ context.Context, id uuid.UUID) (app.App, error) {
	a, ok := f[id]
	if !ok {
		return a, app.ErrNotFound
	}
	return a, nil
}

// permOf returns the permission the real route table gives a route.
func permOf(t *testing.T, method, pattern string) admin.Permission {
	t.Helper()
	for _, r := range Routes(Deps{}) {
		if r.Method == method && r.Pattern == pattern {
			return r.Perm
		}
	}
	t.Fatalf("no route %s %s", method, pattern)
	return ""
}

// txnServer serves assign and reverse (with the permissions of the real
// route table) as a signed-in staff member of role.
func txnServer(t *testing.T, l LedgerService, rd reads, apps appGetter, role store.AdminRole) (*httptest.Server, uuid.UUID) {
	t.Helper()
	h := &handlers{d: Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Ledger: l}, rd: rd, ap: apps}
	userID := uuid.New()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), principalKey{}, admin.Principal{UserID: userID, Role: role})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	for _, rt := range []Route{
		{http.MethodPost, "/transactions/{id}/assign", permOf(t, http.MethodPost, "/transactions/{id}/assign"), h.assignTransaction},
		{http.MethodPost, "/transactions/{id}/reverse", permOf(t, http.MethodPost, "/transactions/{id}/reverse"), h.reverseTransaction},
	} {
		r.With(requirePerm(rt.Perm)).Method(rt.Method, rt.Pattern, rt.handler)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, userID
}

type txnFixture struct {
	rd                              *fakeReads
	apps                            fakeApps
	appID, suspendedID              uuid.UUID
	unmatched, matched, failed, out uuid.UUID
	reversal, manual                uuid.UUID
}

func newTxnFixture() *txnFixture {
	f := &txnFixture{appID: uuid.New(), suspendedID: uuid.New(),
		unmatched: uuid.New(), matched: uuid.New(), failed: uuid.New(), out: uuid.New(), reversal: uuid.New(), manual: uuid.New()}
	f.apps = fakeApps{
		f.appID:       {ID: f.appID, Name: "Hosting", AccountPrefix: "HOST", Status: store.AppStatusActive},
		f.suspendedID: {ID: f.suspendedID, Name: "SMS", AccountPrefix: "SMS", Status: store.AppStatusSuspended},
	}
	receipt := "SKL4ABCD12"
	in := func(id uuid.UUID, appID *uuid.UUID, status store.TxnStatus) store.GetTransactionAdminRow {
		return store.GetTransactionAdminRow{ID: id, AppID: appID, Direction: store.TxnDirectionIn, Kind: store.TxnKindPayment,
			Channel: store.TxnChannelMpesaC2b, Provider: "mpesa", Status: status, Amount: 1500, ProviderTransactionID: &receipt}
	}
	rows := []store.GetTransactionAdminRow{
		in(f.unmatched, nil, store.TxnStatusSucceeded),
		in(f.matched, &f.appID, store.TxnStatusSucceeded),
		in(f.failed, nil, store.TxnStatusFailed),
		{ID: f.out, AppID: &f.appID, Direction: store.TxnDirectionOut, Kind: store.TxnKindRefund, Provider: "mpesa", Status: store.TxnStatusSucceeded},
		{ID: f.reversal, AppID: &f.appID, Direction: store.TxnDirectionOut, Kind: store.TxnKindReversal, Provider: "mpesa", Status: store.TxnStatusSucceeded},
		{ID: f.manual, AppID: &f.appID, Direction: store.TxnDirectionIn, Kind: store.TxnKindPayment, Channel: store.TxnChannelManual,
			Provider: "manual", Status: store.TxnStatusSucceeded, Amount: 900},
	}
	f.rd = &fakeReads{txns: map[uuid.UUID]reporting.TxnDetail{}}
	for _, r := range rows {
		f.rd.txns[r.ID] = reporting.TxnDetail{Txn: r}
	}
	return f
}

// assignOK simulates the ledger assigning txn to appID (and the read model
// seeing it).
func (f *txnFixture) assignOK(txn, appID uuid.UUID) func() (ledger.Transaction, error) {
	return func() (ledger.Transaction, error) {
		d := f.rd.txns[txn]
		d.Txn.AppID = &appID
		st := store.NullAppStatus{AppStatus: f.apps[appID].Status, Valid: true}
		d.Txn.AppStatus = st
		f.rd.txns[txn] = d
		return ledger.Transaction{ID: txn, AppID: &appID}, nil
	}
}

func TestAssignAndReversePermissions(t *testing.T) {
	// finance may assign but not reverse; viewer and approver (support
	// roles) may do neither; admin may do both.
	want := map[store.AdminRole][2]int{ // [assign, reverse]
		store.AdminRoleViewer:   {http.StatusForbidden, http.StatusForbidden},
		store.AdminRoleApprover: {http.StatusForbidden, http.StatusForbidden},
		store.AdminRoleFinance:  {http.StatusOK, http.StatusForbidden},
		store.AdminRoleAdmin:    {http.StatusOK, http.StatusOK},
	}
	for role, w := range want {
		f := newTxnFixture()
		revID := uuid.New()
		f.rd.txns[revID] = reporting.TxnDetail{Txn: store.GetTransactionAdminRow{ID: revID, Kind: store.TxnKindReversal}}
		l := &fakeLedger{assignFn: f.assignOK(f.unmatched, f.appID),
			reverseFn: func() (ledger.ReverseResult, error) {
				return ledger.ReverseResult{Reversal: ledger.Transaction{ID: revID}}, nil
			}}
		srv, _ := txnServer(t, l, f.rd, f.apps, role)
		code, body := do(t, http.MethodPost, srv.URL+"/transactions/"+f.unmatched.String()+"/assign",
			`{"app_id":"`+f.appID.String()+`","reason":"Customer typed ACME instead of HOST"}`)
		if code != w[0] {
			t.Errorf("%s assign: %d %v, want %d", role, code, body, w[0])
		}
		code, body = do(t, http.MethodPost, srv.URL+"/transactions/"+f.matched.String()+"/reverse",
			`{"reason":"Safaricom reversed it","receipt":"SRV1234567"}`)
		if code != w[1] {
			t.Errorf("%s reverse: %d %v, want %d", role, code, body, w[1])
		}
		if allowed := (w[0] == http.StatusOK) || (w[1] == http.StatusOK); !allowed && l.calls != 0 {
			t.Errorf("%s: ledger called %d times despite 403", role, l.calls)
		}
	}
}

func TestAssignTransaction(t *testing.T) {
	f := newTxnFixture()
	l := &fakeLedger{assignFn: f.assignOK(f.unmatched, f.suspendedID)}
	srv, userID := txnServer(t, l, f.rd, f.apps, store.AdminRoleFinance)
	url := func(id uuid.UUID) string { return srv.URL + "/transactions/" + id.String() + "/assign" }
	body := func(appID uuid.UUID, reason string) string {
		return `{"app_id":"` + appID.String() + `","reason":"` + reason + `"}`
	}

	// Assigning to a suspended app is allowed and the response says so.
	code, out := do(t, http.MethodPost, url(f.unmatched), body(f.suspendedID, "  Paid to the old SMS account  "))
	if code != http.StatusOK || out["app_id"] != f.suspendedID.String() || out["app_status"] != "suspended" {
		t.Fatalf("assign: %d %v", code, out)
	}
	if l.gotActor != audit.Admin(userID) || l.gotTxn != f.unmatched || l.gotApp != f.suspendedID || l.gotReason != "Paid to the old SMS account" {
		t.Fatalf("ledger got actor=%v txn=%v app=%v reason=%q", l.gotActor, l.gotTxn, l.gotApp, l.gotReason)
	}

	// Set once: now it has an app.
	l.calls = 0
	code, out = do(t, http.MethodPost, url(f.unmatched), body(f.appID, "second try"))
	if code != http.StatusConflict || out["code"] != "already_assigned" || l.calls != 0 {
		t.Fatalf("reassign: %d %v calls=%d", code, out, l.calls)
	}
	for _, id := range []uuid.UUID{f.failed, f.out} {
		code, out = do(t, http.MethodPost, url(id), body(f.appID, "a reason"))
		if code != http.StatusConflict || (out["code"] != "not_assignable" && out["code"] != "already_assigned") {
			t.Fatalf("not assignable %v: %d %v", id, code, out)
		}
	}
	if code, out = do(t, http.MethodPost, url(f.failed), body(f.appID, "a reason")); out["code"] != "not_assignable" {
		t.Fatalf("failed payment: %d %v", code, out)
	}
	if l.calls != 0 {
		t.Fatalf("ledger called on 409 paths: %d", l.calls)
	}

	// Validation.
	f2 := newTxnFixture()
	l2 := &fakeLedger{assignFn: f2.assignOK(f2.unmatched, f2.appID)}
	srv2, _ := txnServer(t, l2, f2.rd, f2.apps, store.AdminRoleAdmin)
	url2 := srv2.URL + "/transactions/" + f2.unmatched.String() + "/assign"
	for _, tc := range []struct{ body, field string }{
		{`{"app_id":"nope","reason":"a reason"}`, "app_id"},
		{`{"reason":"a reason"}`, "app_id"},
		{body(uuid.New(), "a reason"), "app_id"}, // no such app
		{body(f2.appID, "no"), "reason"},
		{body(f2.appID, strings.Repeat("r", 501)), "reason"},
	} {
		code, out := do(t, http.MethodPost, url2, tc.body)
		if code != http.StatusUnprocessableEntity || out["code"] != "validation_failed" {
			t.Fatalf("%s: %d %v", tc.body, code, out)
		}
		if errs, _ := out["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field {
			t.Fatalf("%s: errors %v", tc.body, out["errors"])
		}
	}
	if code, _ := do(t, http.MethodPost, url2, `{"app_id":"`+f2.appID.String()+`","reason":"abc","x":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	if code, _ := do(t, http.MethodPost, srv2.URL+"/transactions/"+uuid.NewString()+"/assign", body(f2.appID, "a reason")); code != http.StatusNotFound {
		t.Fatalf("unknown txn: %d", code)
	}
	if l2.calls != 0 {
		t.Fatalf("ledger called on invalid input: %d", l2.calls)
	}

	// Lost race: another staff member assigned it between the check and
	// the ledger call; the ledger refuses and the API answers 409.
	l2.assignFn = func() (ledger.Transaction, error) {
		_, _ = f2.assignOK(f2.unmatched, f2.suspendedID)()
		return ledger.Transaction{}, errors.New("ledger: already assigned")
	}
	if code, out := do(t, http.MethodPost, url2, body(f2.appID, "a reason")); code != http.StatusConflict || out["code"] != "already_assigned" {
		t.Fatalf("race: %d %v", code, out)
	}

	// Ledger not wired: 503, after input checks.
	f3 := newTxnFixture()
	srv3, _ := txnServer(t, nil, f3.rd, f3.apps, store.AdminRoleAdmin)
	if code, out := do(t, http.MethodPost, srv3.URL+"/transactions/"+f3.unmatched.String()+"/assign", body(f3.appID, "a reason")); code != http.StatusServiceUnavailable || out["code"] != "ledger_unavailable" {
		t.Fatalf("no ledger: %d %v", code, out)
	}
}

func TestReverseTransaction(t *testing.T) {
	f := newTxnFixture()
	revID := uuid.New()
	l := &fakeLedger{reverseFn: func() (ledger.ReverseResult, error) {
		f.rd.txns[revID] = reporting.TxnDetail{Txn: store.GetTransactionAdminRow{ID: revID, AppID: &f.appID,
			Direction: store.TxnDirectionOut, Kind: store.TxnKindReversal, Status: store.TxnStatusSucceeded, ReversesTransactionID: &f.matched}}
		return ledger.ReverseResult{Reversal: ledger.Transaction{ID: revID}}, nil
	}}
	srv, userID := txnServer(t, l, f.rd, f.apps, store.AdminRoleAdmin)
	url := func(id uuid.UUID) string { return srv.URL + "/transactions/" + id.String() + "/reverse" }

	code, out := do(t, http.MethodPost, url(f.matched), `{"reason":" Safaricom reversed it ","receipt":" srv1234567 "}`)
	if code != http.StatusOK || out["id"] != revID.String() || out["kind"] != "reversal" || out["reverses_transaction_id"] != f.matched.String() {
		t.Fatalf("reverse: %d %v", code, out)
	}
	if l.gotActor != audit.Admin(userID) || l.gotRev != (ledger.ReverseParams{OriginalID: f.matched, Reason: "Safaricom reversed it", ProviderReceipt: "SRV1234567"}) {
		t.Fatalf("ledger got %v %+v", l.gotActor, l.gotRev)
	}
	// A manual payment needs no M-Pesa receipt.
	if code, out := do(t, http.MethodPost, url(f.manual), `{"reason":"Recorded in error"}`); code != http.StatusOK {
		t.Fatalf("manual: %d %v", code, out)
	}

	// 409: not reversible (checked before the ledger).
	l.calls = 0
	for name, id := range map[string]uuid.UUID{"unmatched": f.unmatched, "failed": f.failed, "refund": f.out, "reversal": f.reversal} {
		code, out := do(t, http.MethodPost, url(id), `{"reason":"a reason","receipt":"SRV1"}`)
		if code != http.StatusConflict || out["code"] != "not_reversible" {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	if l.calls != 0 {
		t.Fatalf("ledger called on 409 paths: %d", l.calls)
	}

	// Validation.
	for _, tc := range []struct{ body, field string }{
		{`{"reason":"ab","receipt":"SRV1"}`, "reason"},
		{`{"reason":"a reason"}`, "receipt"}, // M-Pesa payment needs the reversal receipt
		{`{"reason":"a reason","receipt":"BAD-RECEIPT"}`, "receipt"},
	} {
		code, out := do(t, http.MethodPost, url(f.matched), tc.body)
		if code != http.StatusUnprocessableEntity || out["code"] != "validation_failed" {
			t.Fatalf("%s: %d %v", tc.body, code, out)
		}
		if errs, _ := out["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field {
			t.Fatalf("%s: errors %v", tc.body, out["errors"])
		}
	}
	if code, _ := do(t, http.MethodPost, url(uuid.New()), `{"reason":"a reason","receipt":"SRV1"}`); code != http.StatusNotFound {
		t.Fatalf("unknown txn: %d", code)
	}

	// Ledger errors.
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{ledger.ErrAlreadyReversed, http.StatusConflict, "already_reversed"},
		{ledger.ErrNotReversible, http.StatusConflict, "not_reversible"},
		{ledger.ErrReceiptConflict, http.StatusConflict, "receipt_taken"},
		{ledger.ErrReversalReceipt, http.StatusUnprocessableEntity, "validation_failed"},
		{ledger.ErrReasonRequired, http.StatusUnprocessableEntity, "validation_failed"},
		{errors.New("db down"), http.StatusInternalServerError, ""},
	} {
		l.reverseFn = func() (ledger.ReverseResult, error) { return ledger.ReverseResult{}, tc.err }
		code, out := do(t, http.MethodPost, url(f.matched), `{"reason":"a reason","receipt":"SRV1"}`)
		if code != tc.status || (tc.code != "" && out["code"] != tc.code) {
			t.Fatalf("%v: %d %v", tc.err, code, out)
		}
	}

	srv2, _ := txnServer(t, nil, f.rd, f.apps, store.AdminRoleAdmin)
	if code, out := do(t, http.MethodPost, srv2.URL+"/transactions/"+f.matched.String()+"/reverse", `{"reason":"a reason","receipt":"SRV1"}`); code != http.StatusServiceUnavailable || out["code"] != "ledger_unavailable" {
		t.Fatalf("no ledger: %d %v", code, out)
	}
}

// The dashboard JSON carries exactly the counts the spec requires, plus the
// windows and the daily series.
func TestDashboardShape(t *testing.T) {
	d := reporting.Dashboard{Daily: []reporting.DayTotal{{Day: "2026-11-25", ByApp: map[uuid.UUID]int64{}}}}
	d.Counts.ChargesPending, d.Counts.UnmatchedAmount, d.Counts.InactiveAppPayments = 3, 4200, 1
	b, err := json.Marshal(toDashboard(d, []app.App{{ID: uuid.New(), Name: "Hosting", AccountPrefix: "HOST", Status: store.AppStatusActive}}))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"today", "last_7_days", "last_30_days", "daily", "apps", "counts", "timezone", "generated_at"} {
		if _, ok := out[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	counts := out["counts"].(map[string]any)
	if counts["charges_pending"] != float64(3) || counts["unmatched_amount"] != float64(4200) || counts["inactive_app_payments"] != float64(1) {
		t.Fatalf("counts %v", counts)
	}
	var got []string
	for k := range counts {
		got = append(got, k)
	}
	sort.Strings(got)
	want := specDashboardCounts(t)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("counts keys\n got %v\nspec %v", got, want)
	}
}

// specDashboardCounts reads Dashboard.counts.required from the spec.
func specDashboardCounts(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("../../api/admin.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var buf strings.Builder
	state := 0 // 0: before Dashboard, 1: in Dashboard, 2: in counts, 3: collecting required
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case state == 0 && line == "    Dashboard:":
			state = 1
		case state == 1 && strings.TrimSpace(line) == "counts:":
			state = 2
		case state == 2 && strings.Contains(line, "required: ["):
			state = 3
			buf.WriteString(line[strings.Index(line, "[")+1:])
		case state == 3:
			buf.WriteString(line)
		}
		if state == 3 && strings.Contains(buf.String(), "]") {
			break
		}
	}
	s, _, _ := strings.Cut(buf.String(), "]")
	keys := regexp.MustCompile(`[a-z_0-9]+`).FindAllString(s, -1)
	if len(keys) == 0 {
		t.Fatal("Dashboard.counts.required not found in the spec")
	}
	sort.Strings(keys)
	return keys
}
