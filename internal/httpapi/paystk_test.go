package httpapi_test

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var operator = audit.CLI("httpapi-test")

type payEnv struct {
	t      *testing.T
	ctx    context.Context
	db     *testdb.DB
	logs   *syncBuffer
	logger *slog.Logger
	apps   *app.Service
	stk    *fakeSTK
	h      http.Handler
}

type bill struct {
	appID uuid.UUID
	ch    store.Charge
}

func newPayEnv(t *testing.T, mutate func(*httpapi.Deps)) *payEnv {
	t.Helper()
	d := testdb.New(t)
	logs := &syncBuffer{}
	logger := tlog.New(logs, tlog.Options{Level: slog.LevelDebug})
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	kr, err := crypto.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), 0)
	if err != nil {
		t.Fatal(err)
	}
	e := &payEnv{
		t: t, ctx: context.Background(), db: d, logs: logs, logger: logger,
		apps: app.NewService(app.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("h", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		stk: newFakeSTK(),
	}
	deps := httpapi.Deps{
		Logger: logger, Pool: d.App, Apps: e.apps, Reporting: reporting.NewService(d.App), MaxBodyBytes: 1 << 20,
		Paybill: "4123456", STK: e.stk,
		PaySTKLimiter: httpx.NewRateLimiter(10000, 10000), // tests that need the real limit override it
	}
	if mutate != nil {
		mutate(&deps)
	}
	e.h = httpapi.NewRouter(deps)
	return e
}

func (e *payEnv) bill(prefix string, amount int64) bill {
	e.t.Helper()
	c, err := e.apps.Create(e.ctx, operator, app.CreateParams{
		Name: prefix + " app", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	pid := c.App.ID
	ch := seed.Charge(e.t, e.ctx, e.db.App, pid, prefix, amount, seed.ChargeOpts{Description: "Invoice for Customer Jane " + prefix})
	return bill{appID: pid, ch: ch}
}

func (e *payEnv) get(token string) result {
	return serve(e.h, http.MethodGet, "/public/v1/pay/"+token, nil)
}

func (e *payEnv) push(token, body string, opts ...reqOpt) result {
	return serve(e.h, http.MethodPost, "/public/v1/pay/"+token+"/stk", strings.NewReader(body), opts...)
}

// assertNoLeak: pay-page responses never carry ids or the phone.
func assertNoLeak(t *testing.T, body string, b bill, extra ...string) {
	t.Helper()
	for _, s := range append([]string{
		b.appID.String(), b.ch.ID.String(),
		fullPhone, "0712345678", "712345678", "ws_CO_", "MERCHANT_",
	}, extra...) {
		if strings.Contains(body, s) {
			t.Errorf("response leaks %q: %s", s, body)
		}
	}
}

func TestPayPageWithoutMpesa(t *testing.T) {
	e := newPayEnv(t, func(d *httpapi.Deps) { d.STK = nil })
	b := e.bill("NOMP", 2500)
	m := e.get(b.ch.HostedToken).expect(t, 200, "")
	if m["stk_available"] != false || m["stk_push"] != nil {
		t.Fatalf("%v", m)
	}
	if _, ok := m["stk_push"]; !ok {
		t.Fatal("stk_push must be present (null)")
	}
	e.push(b.ch.HostedToken, `{"phone":"0712345678"}`).expect(t, 503, "mpesa_unavailable")
	e.push(strings.Repeat("a", 52), `{"phone":"0712345678"}`).expect(t, 404, "not_found")
}

func TestPayPageSTKStatus(t *testing.T) {
	e := newPayEnv(t, nil)
	b := e.bill("PSTK", 2500)

	// No prompt yet.
	m := e.get(b.ch.HostedToken).expect(t, 200, "")
	if m["stk_available"] != true || m["stk_push"] != nil {
		t.Fatalf("%v", m)
	}
	calls := e.stk.latestCalls
	if len(calls) != 1 || calls[0].chargeID != b.ch.ID || time.Since(calls[0].since) < 14*time.Minute || time.Since(calls[0].since) > 16*time.Minute {
		t.Fatalf("latest lookup %+v", calls)
	}

	// A failed prompt shows a customer message, never ids or the phone.
	code := "1032"
	p := testPush(b.appID, b.ch.ID, store.StkPushStatusFailed, &code)
	desc := "Request cancelled by user"
	p.ResultDesc = &desc
	e.stk.latest = &p
	r := e.get(b.ch.HostedToken)
	m = r.expect(t, 200, "")
	sp := m["stk_push"].(map[string]any)
	if sp["status"] != "failed" || sp["message"] != "You cancelled the M-Pesa prompt." || sp["created_at"] == nil || len(sp) != 3 {
		t.Fatalf("stk_push %v", sp)
	}
	assertNoLeak(t, r.body, b, p.ID.String(), p.TransactionID.String(), desc)

	// Live and unknown (needs_review) prompts: no message.
	for st, want := range map[store.StkPushStatus]string{
		store.StkPushStatusSent: "sent", store.StkPushStatusInitiating: "initiating",
		store.StkPushStatusUnknown: "needs_review", store.StkPushStatusSucceeded: "succeeded",
	} {
		p := testPush(b.appID, b.ch.ID, st, nil)
		e.stk.latest = &p
		sp := e.get(b.ch.HostedToken).expect(t, 200, "")["stk_push"].(map[string]any)
		if sp["status"] != want || sp["message"] != nil {
			t.Errorf("%s: %v", st, sp)
		}
	}

	// A lookup failure still serves the page (without the prompt).
	e.stk.latest, e.stk.latestErr = nil, errors.New("db hiccup")
	m = e.get(b.ch.HostedToken).expect(t, 200, "")
	if m["stk_push"] != nil || m["amount"].(float64) != 2500 {
		t.Fatal(m)
	}

	// M-Pesa configured but not available: the latest prompt still shows,
	// new prompts cannot be sent.
	e.stk.latestErr = nil
	e.stk.available = false
	if m := e.get(b.ch.HostedToken).expect(t, 200, ""); m["stk_available"] != false {
		t.Fatal(m)
	}
}

func TestPayPageSTKAvailability(t *testing.T) {
	e := newPayEnv(t, nil)
	// A charge can never be above the M-Pesa limit (amount 1..250000).
	edge := e.bill("PEDG", mpesa.MaxAmount)
	if m := e.get(edge.ch.HostedToken).expect(t, 200, ""); m["stk_available"] != true || m["payable"] != true {
		t.Fatal(m)
	}

	// Succeeded: nothing to prompt for, no paybill steps.
	paid := e.bill("PPAI", 100)
	seed.PayCharge(t, e.ctx, e.db.App, paid.ch)
	if m := e.get(paid.ch.HostedToken).expect(t, 200, ""); m["stk_available"] != false || m["status"] != "succeeded" || m["paybill"] != nil {
		t.Fatal(m)
	}
	e.push(paid.ch.HostedToken, `{"phone":"0712345678"}`).expect(t, 409, "not_payable")

	// Cancelled: not payable either.
	gone := e.bill("PCAN", 100)
	if _, err := store.New(e.db.App).CancelCharge(e.ctx, store.CancelChargeParams{AppID: gone.appID, ID: gone.ch.ID}); err != nil {
		t.Fatal(err)
	}
	e.push(gone.ch.HostedToken, `{"phone":"0712345678"}`).expect(t, 409, "not_payable")
	if len(e.stk.sendCalls()) != 0 {
		t.Fatal("sent for an unpayable charge")
	}
}

func TestPayPageSTKSend(t *testing.T) {
	e := newPayEnv(t, nil)
	b := e.bill("PSND", 2500)
	r := e.push(b.ch.HostedToken, `{"phone":"0712 345 678"}`)
	m := r.expect(t, 202, "")
	if m["status"] != "sent" || m["message"] != nil || m["created_at"] == nil || len(m) != 3 {
		t.Fatalf("%s", r.body)
	}
	if r.hdr.Get("X-Robots-Tag") == "" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal("missing noindex / no-store")
	}
	assertNoLeak(t, r.body, b)
	calls := e.stk.sendCalls()
	want := stk.SendParams{AppID: b.appID, ChargeID: b.ch.ID, Phone: "0712 345 678", Source: stk.SourcePayPage}
	if len(calls) != 1 || calls[0].p != want || calls[0].actor != audit.System("pay_page") {
		t.Fatalf("send %+v", calls)
	}
	if strings.Contains(e.logs.String(), fullPhone) || strings.Contains(e.logs.String(), "345 678") || strings.Contains(e.logs.String(), b.ch.HostedToken) {
		t.Fatalf("logs leak the phone or pay token:\n%s", e.logs.String())
	}
}

func TestPayPageSTKValidationAndNotFound(t *testing.T) {
	e := newPayEnv(t, nil)
	b := e.bill("PVAL", 2500)
	tok := b.ch.HostedToken
	for body, code := range map[string]string{
		`{"phone":"0812345678"}`:              "validation_failed",
		`{"phone":"12"}`:                      "validation_failed",
		`{}`:                                  "validation_failed",
		`{"phone":5}`:                         "validation_failed",
		`{"phone":"07123456789012345678901"}`: "validation_failed",
	} {
		r := e.push(tok, body)
		m := r.expect(t, 422, code)
		if errs, _ := m["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "phone" {
			t.Errorf("%s: %s", body, r.body)
		}
	}
	e.push(tok, `{"phone":"0712345678","amount":1}`).expect(t, 400, "invalid_json")
	e.push(tok, `{"phone":"0712345678"`).expect(t, 400, "invalid_json")
	e.push(tok, `{"phone":"0712345678"}`, withHeader("Content-Type", "text/plain")).expect(t, 415, "unsupported_media_type")
	e.push(tok, `{"phone":"`+strings.Repeat("1", 2000)+`"}`).expect(t, 413, "payload_too_large")
	if len(e.stk.sendCalls()) != 0 {
		t.Fatal("invalid input reached the service")
	}

	// 404s identical to GET's: malformed, unknown, suspended app.
	ref := e.get("not-a-token").expect(t, 404, "not_found")
	for _, tk := range []string{"not-a-token", strings.Repeat("a", 52)} {
		m := e.push(tk, `{"phone":"0712345678"}`).expect(t, 404, "not_found")
		if m["detail"] != ref["detail"] {
			t.Fatal(m)
		}
	}
	if _, err := e.apps.SetStatus(e.ctx, operator, b.appID, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	e.push(tok, `{"phone":"0712345678"}`).expect(t, 404, "not_found")
	// Unknown token beats a bad body: nothing about the body is revealed.
	e.push(strings.Repeat("a", 52), `garbage`).expect(t, 404, "not_found")
	if len(e.stk.sendCalls()) != 0 {
		t.Fatal("sent for an invalid link")
	}
}

func TestPayPageSTKErrorMapping(t *testing.T) {
	e := newPayEnv(t, nil)
	b := e.bill("PERR", 2500)
	live := uuid.New()
	failed := testPush(b.appID, b.ch.ID, store.StkPushStatusFailed, nil)
	cases := []struct {
		name   string
		push   stk.Push
		err    error
		status int
		code   string
	}{
		{"not found", stk.Push{}, stk.ErrNotFound, 404, "not_found"},
		{"unavailable", stk.Push{}, stk.ErrUnavailable, 503, "mpesa_unavailable"},
		{"not payable", stk.Push{}, stk.ErrNotPayable, 409, "not_payable"},
		{"too large", stk.Push{}, stk.ErrAmountTooLarge, 409, "amount_too_large"},
		{"in progress", stk.Push{}, &stk.InProgressError{LiveID: live}, 409, "stk_in_progress"},
		{"phone", stk.Push{}, phone.ErrInvalid, 422, "validation_failed"},
		{"daraja", failed, stk.ErrDaraja, 502, "mpesa_error"},
		{"busy", stk.Push{}, &pgconn.PgError{Code: "40001"}, 503, "busy"},
		{"deadline", stk.Push{}, context.DeadlineExceeded, 503, "overloaded"},
		{"unexpected", stk.Push{}, stk.ErrIdempotencyConflict, 500, "internal_error"},
	}
	for _, tc := range cases {
		e.stk.result = func(stk.SendParams) (stk.Push, bool, error) { return tc.push, false, tc.err }
		r := e.push(b.ch.HostedToken, `{"phone":"0712345678"}`)
		if r.code != tc.status || r.json(t)["code"] != tc.code {
			t.Errorf("%s: %d %s", tc.name, r.code, r.body)
		}
		assertNoLeak(t, r.body, b, live.String(), failed.ID.String(), failed.TransactionID.String(), "idempotency")
	}
}

func TestPayPageSTKRateLimit(t *testing.T) {
	e := newPayEnv(t, func(d *httpapi.Deps) { d.PaySTKLimiter = httpx.NewRateLimiter(60, 2) })
	b := e.bill("PRLM", 2500)
	e.push(b.ch.HostedToken, `{"phone":"0712345678"}`).expect(t, 202, "")
	e.push(b.ch.HostedToken, `{"phone":"0712345678"}`).expect(t, 202, "")
	e.push(b.ch.HostedToken, `{"phone":"0712345678"}`).expect(t, 429, "rate_limited")
	// Reads have their own (looser) budget.
	e.get(b.ch.HostedToken).expect(t, 200, "")
	// Another visitor is unaffected.
	e.push(b.ch.HostedToken, `{"phone":"0712345678"}`, fromAddr("203.0.113.50:1")).expect(t, 202, "")
	if n := len(e.stk.sendCalls()); n != 3 {
		t.Fatalf("sends %d", n)
	}
}

// The default prompt limit is stricter than the read limit.
func TestPayPageSTKDefaultLimit(t *testing.T) {
	e := newPayEnv(t, func(d *httpapi.Deps) { d.PaySTKLimiter = nil })
	b := e.bill("PDEF", 2500)
	var codes []int
	for i := 0; i < httpapi.DefaultPaySTKBurst+1; i++ {
		codes = append(codes, e.push(b.ch.HostedToken, `{"phone":"0712345678"}`).code)
	}
	if codes[len(codes)-1] != http.StatusTooManyRequests {
		t.Fatalf("no default limit: %v", codes)
	}
}
