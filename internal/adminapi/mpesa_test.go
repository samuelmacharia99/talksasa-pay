package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

type fakeSTK struct {
	gotActor  audit.Actor
	gotParams stk.ResolveParams
	resolveFn func(stk.ResolveParams) (store.Transaction, error)
	pushes    []stk.Push
	gotLimit  int
}

func (f *fakeSTK) Resolve(_ context.Context, actor audit.Actor, p stk.ResolveParams) (store.Transaction, error) {
	f.gotActor, f.gotParams = actor, p
	return f.resolveFn(p)
}

func (f *fakeSTK) ListForRequest(_ context.Context, _ uuid.UUID, limit int) ([]stk.Push, error) {
	f.gotLimit = limit
	return f.pushes, nil
}

type fakeReads struct {
	txns      map[uuid.UUID]reporting.TxnDetail
	requests  map[uuid.UUID]bool
	callbacks []store.ListMpesaCallbacksAttentionRow
	gotFilter reporting.CallbackFilter
}

func (f *fakeReads) GetTransaction(_ context.Context, id uuid.UUID) (reporting.TxnDetail, error) {
	d, ok := f.txns[id]
	if !ok {
		return d, reporting.ErrNotFound
	}
	return d, nil
}

func (f *fakeReads) GetPaymentRequest(_ context.Context, id uuid.UUID) (reporting.RequestDetail, error) {
	if !f.requests[id] {
		return reporting.RequestDetail{}, reporting.ErrNotFound
	}
	return reporting.RequestDetail{}, nil
}

func (f *fakeReads) ListMpesaCallbacks(_ context.Context, cf reporting.CallbackFilter) (reporting.Page[store.ListMpesaCallbacksAttentionRow], error) {
	f.gotFilter = cf
	if cf.Cursor == "bad" {
		return reporting.Page[store.ListMpesaCallbacksAttentionRow]{}, fmt.Errorf("%w: malformed cursor", reporting.ErrInvalidFilter)
	}
	return reporting.Page[store.ListMpesaCallbacksAttentionRow]{Items: f.callbacks, NextCursor: "next"}, nil
}

// mpesaServer serves the M-Pesa routes as a signed-in staff member of role.
func mpesaServer(t *testing.T, s STKService, rd reads, role store.AdminRole) (*httptest.Server, uuid.UUID) {
	t.Helper()
	d := Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), STK: s}
	h := &handlers{d: d, rd: rd}
	userID := uuid.New()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), principalKey{}, admin.Principal{UserID: userID, Role: role})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	for _, rt := range []Route{
		{http.MethodPost, "/transactions/{id}/resolve", admin.PermResolvePayments, h.resolveTransaction},
		{http.MethodGet, "/payment-requests/{id}/stk-pushes", admin.PermRead, h.listSTKPushes},
		{http.MethodGet, "/mpesa/callbacks", admin.PermRead, h.listMpesaCallbacks},
	} {
		r.With(requirePerm(rt.Perm)).Method(rt.Method, rt.Pattern, rt.handler)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, userID
}

func do(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestResolveTransaction(t *testing.T) {
	txnID := uuid.New()
	receipt := "SKL4ABCD12"
	rd := &fakeReads{txns: map[uuid.UUID]reporting.TxnDetail{txnID: {Txn: store.GetTransactionAdminRow{
		ID: txnID, Status: store.TxnStatusSucceeded, Channel: store.TxnChannelMpesaStk, ProviderTransactionID: &receipt,
	}}}}
	f := &fakeSTK{resolveFn: func(p stk.ResolveParams) (store.Transaction, error) {
		return store.Transaction{ID: p.TransactionID}, nil
	}}
	srv, userID := mpesaServer(t, f, rd, store.AdminRoleFinance)
	url := srv.URL + "/transactions/" + txnID.String() + "/resolve"

	code, body := do(t, http.MethodPost, url, `{"action":"confirm","receipt":" skl4abcd12 ","reason":"Seen on the M-Pesa portal"}`)
	if code != http.StatusOK {
		t.Fatalf("confirm: %d %v", code, body)
	}
	if body["id"] != txnID.String() || body["status"] != "succeeded" || body["receipt"] != receipt {
		t.Fatalf("confirm body: %v", body)
	}
	if !f.gotParams.Confirm || f.gotParams.Receipt != receipt || f.gotParams.Reason != "Seen on the M-Pesa portal" || f.gotParams.TransactionID != txnID {
		t.Fatalf("params: %+v", f.gotParams)
	}
	if f.gotActor != audit.Admin(userID) {
		t.Fatalf("actor %v", f.gotActor)
	}

	if code, _ = do(t, http.MethodPost, url, `{"action":"fail","reason":"No money on the statement"}`); code != http.StatusOK || f.gotParams.Confirm {
		t.Fatalf("fail: %d confirm=%v", code, f.gotParams.Confirm)
	}

	// Input checked before the service.
	for _, tc := range []struct{ body, field string }{
		{`{"action":"confirm","reason":"x reason"}`, "receipt"},
		{`{"action":"maybe","reason":"x reason"}`, "action"},
		{`{"action":"fail","receipt":"ABC","reason":"x reason"}`, "receipt"},
		{`{"action":"fail","reason":"` + strings.Repeat("r", 501) + `"}`, "reason"},
	} {
		code, body := do(t, http.MethodPost, url, tc.body)
		if code != http.StatusUnprocessableEntity || body["code"] != "validation_failed" {
			t.Fatalf("%s: %d %v", tc.body, code, body)
		}
		if errs, _ := body["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field {
			t.Fatalf("%s: errors %v", tc.body, body["errors"])
		}
	}
	if code, _ := do(t, http.MethodPost, url, `{"action":"fail","reason":"ok reason","extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}

	// Service errors.
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{stk.ErrNotResolvable, http.StatusConflict, "not_resolvable"},
		{stk.ErrReceiptTaken, http.StatusConflict, "receipt_taken"},
		{stk.ErrInvalidReceipt, http.StatusUnprocessableEntity, "validation_failed"},
		{stk.ErrReasonRequired, http.StatusUnprocessableEntity, "validation_failed"},
		{stk.ErrNotFound, http.StatusNotFound, "not_found"},
		{fmt.Errorf("wrapped: %w", stk.ErrReceiptTaken), http.StatusConflict, "receipt_taken"},
		{errors.New("db down"), http.StatusInternalServerError, ""},
	} {
		f.resolveFn = func(stk.ResolveParams) (store.Transaction, error) { return store.Transaction{}, tc.err }
		code, body := do(t, http.MethodPost, url, `{"action":"confirm","receipt":"SKL4ABCD12","reason":"portal"}`)
		if code != tc.status || (tc.code != "" && body["code"] != tc.code) {
			t.Fatalf("%v: %d %v", tc.err, code, body)
		}
	}

	// Bad id.
	if code, _ := do(t, http.MethodPost, srv.URL+"/transactions/nope/resolve", `{"action":"fail","reason":"abc"}`); code != http.StatusNotFound {
		t.Fatalf("bad id: %d", code)
	}
}

func TestResolveTransactionPermissions(t *testing.T) {
	f := &fakeSTK{resolveFn: func(p stk.ResolveParams) (store.Transaction, error) {
		return store.Transaction{ID: p.TransactionID}, nil
	}}
	for role, want := range map[store.AdminRole]int{
		store.AdminRoleViewer: http.StatusForbidden, store.AdminRoleApprover: http.StatusForbidden,
		store.AdminRoleFinance: http.StatusNotFound, store.AdminRoleAdmin: http.StatusNotFound, // allowed; fake reads has no txn
	} {
		srv, _ := mpesaServer(t, f, &fakeReads{}, role)
		code, _ := do(t, http.MethodPost, srv.URL+"/transactions/"+uuid.NewString()+"/resolve", `{"action":"fail","reason":"none came"}`)
		if code != want {
			t.Errorf("%s: %d, want %d", role, code, want)
		}
	}
}

func TestResolveWithoutSTKService(t *testing.T) {
	srv, _ := mpesaServer(t, nil, &fakeReads{}, store.AdminRoleAdmin)
	code, body := do(t, http.MethodPost, srv.URL+"/transactions/"+uuid.NewString()+"/resolve", `{"action":"fail","reason":"none came"}`)
	if code != http.StatusServiceUnavailable || body["code"] != "mpesa_unavailable" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestListSTKPushes(t *testing.T) {
	reqID := uuid.New()
	checkout := "ws_CO_123"
	now := time.Now().UTC().Truncate(time.Second)
	f := &fakeSTK{pushes: []stk.Push{{
		ID: uuid.New(), PaymentRequestID: reqID, TransactionID: uuid.New(), Source: "pay_page", Phone: "254712345678",
		Amount: 1500, CheckoutRequestID: &checkout, Status: store.StkPushStatusUnknown, QueryAttempts: 3, CreatedAt: now,
	}}}
	srv, _ := mpesaServer(t, f, &fakeReads{requests: map[uuid.UUID]bool{reqID: true}}, store.AdminRoleViewer)

	code, body := do(t, http.MethodGet, srv.URL+"/payment-requests/"+reqID.String()+"/stk-pushes", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items %v", items)
	}
	p := items[0].(map[string]any)
	if p["status"] != "unknown" || p["phone"] == "254712345678" || !strings.Contains(p["phone"].(string), "*") ||
		p["checkout_request_id"] != checkout || p["amount"] != float64(1500) || p["completed_at"] != nil {
		t.Fatalf("push %v", p)
	}
	for _, k := range []string{"id", "source", "status", "amount", "phone", "checkout_request_id", "result_code", "result_desc", "query_attempts", "transaction_id", "created_at", "completed_at"} {
		if _, ok := p[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if f.gotLimit != stkListLimit {
		t.Fatalf("limit %d", f.gotLimit)
	}

	if code, _ := do(t, http.MethodGet, srv.URL+"/payment-requests/"+uuid.NewString()+"/stk-pushes", ""); code != http.StatusNotFound {
		t.Fatalf("unknown request: %d", code)
	}

	// M-Pesa off: empty list, not an error.
	srv2, _ := mpesaServer(t, nil, &fakeReads{requests: map[uuid.UUID]bool{reqID: true}}, store.AdminRoleViewer)
	code, body = do(t, http.MethodGet, srv2.URL+"/payment-requests/"+reqID.String()+"/stk-pushes", "")
	if code != http.StatusOK || len(body["items"].([]any)) != 0 {
		t.Fatalf("off: %d %v", code, body)
	}
}

func TestListMpesaCallbacks(t *testing.T) {
	ip := netip.MustParseAddr("196.201.214.200")
	errMsg := "amount mismatch"
	rd := &fakeReads{callbacks: []store.ListMpesaCallbacksAttentionRow{
		{ID: uuid.New(), Kind: store.MpesaCallbackKindStk, RemoteIp: &ip, ReceivedAt: time.Now(), Error: &errMsg, Attempts: 2},
		{ID: uuid.New(), Kind: store.MpesaCallbackKindC2bConfirmation, ReceivedAt: time.Now()},
	}}
	srv, _ := mpesaServer(t, &fakeSTK{}, rd, store.AdminRoleViewer)

	code, body := do(t, http.MethodGet, srv.URL+"/mpesa/callbacks?unprocessed=true&cursor=abc&limit=10", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	if rd.gotFilter != (reporting.CallbackFilter{AttentionOnly: true, Cursor: "abc", Limit: 10}) {
		t.Fatalf("filter %+v", rd.gotFilter)
	}
	items := body["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if len(items) != 2 || body["next_cursor"] != "next" || first["remote_ip"] != ip.String() || first["error"] != errMsg ||
		first["kind"] != "stk" || second["remote_ip"] != nil || second["kind"] != "c2b_confirmation" {
		t.Fatalf("body %v", body)
	}
	if _, ok := first["raw"]; ok {
		t.Fatal("callback bodies must not be listed")
	}

	if _, _ = do(t, http.MethodGet, srv.URL+"/mpesa/callbacks", ""); rd.gotFilter.AttentionOnly {
		t.Fatal("default must list all callbacks")
	}
	if code, body := do(t, http.MethodGet, srv.URL+"/mpesa/callbacks?unprocessed=yes", ""); code != http.StatusBadRequest || body["code"] != "invalid_filter" {
		t.Fatalf("bad flag: %d %v", code, body)
	}
	if code, body := do(t, http.MethodGet, srv.URL+"/mpesa/callbacks?cursor=bad", ""); code != http.StatusBadRequest || body["code"] != "invalid_filter" {
		t.Fatalf("bad cursor: %d %v", code, body)
	}
}
