package integrationtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/adminapi"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
	"github.com/talksasa/talksasa-pay/internal/webhook"
)

type apiEnv struct {
	*adminEnv
	deps  adminapi.Deps
	admin http.Handler
	pub   http.Handler
}

func newAPIEnv(t *testing.T) *apiEnv {
	a := newAdminEnv(t)
	rep := reporting.NewService(a.db.App)
	deps := adminapi.Deps{
		Logger: a.logger, Admin: a.svc, Apps: a.apps, Reporting: rep, MaxBodyBytes: 64 << 10,
		Webhooks:       webhook.NewDeliveries(a.db.App, a.river),
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		AuthLimiter:    httpx.NewRateLimiter(6000, 6000), // tests exercise auth many times
	}
	// TODO(gateway-core): `Ledger: a.ledger` once ledger.Service.AssignToApp lands.
	if l, ok := any(a.ledger).(adminapi.LedgerService); ok {
		deps.Ledger = l
	}
	return &apiEnv{
		adminEnv: a, deps: deps, admin: adminapi.NewRouter(deps),
		pub: httpapi.NewRouter(httpapi.Deps{Logger: a.logger, Pool: a.db.App, Apps: a.apps, Reporting: rep,
			MaxBodyBytes: 1 << 20, Paybill: "4123456", PayLimiter: httpx.NewRateLimiter(6000, 6000)}),
	}
}

type resp struct {
	code int
	body string
	hdr  http.Header
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.code, r.body)
	}
	return m
}

func call(h http.Handler, method, path, token string, body any, mutate ...func(*http.Request)) resp {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "10.1.1.1:4000"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, m := range mutate {
		m(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return resp{code: w.Code, body: w.Body.String(), hdr: w.Header()}
}

func TestAdminAPIEveryRouteRequiresSessionAndPermission(t *testing.T) {
	e := newAPIEnv(t)
	tokens := map[store.AdminRole]string{}
	for _, role := range store.AllAdminRoleValues() {
		s := e.onboard(string(role)+"@talksasa.co.ke", role)
		tokens[role] = e.signIn(s).Token
	}
	fill := strings.NewReplacer("{id}", "00000000-0000-7000-8000-000000000000", "{keyID}", "abcdefghijklmnopqrst",
		"{eventID}", "00000000-0000-7000-8000-000000000000")
	for _, rt := range adminapi.Routes(e.deps) {
		if rt.Pattern == "/auth/logout" || rt.Pattern == "/auth/password" {
			continue // would end the session used by later checks; covered elsewhere
		}
		path := "/admin/v1" + fill.Replace(rt.Pattern)
		if r := call(e.admin, rt.Method, path, "", map[string]any{}); r.code != http.StatusUnauthorized {
			t.Errorf("%s %s without session: %d", rt.Method, rt.Pattern, r.code)
		}
		if r := call(e.admin, rt.Method, path, strings.Repeat("a", 52), map[string]any{}); r.code != http.StatusUnauthorized {
			t.Errorf("%s %s with forged session: %d", rt.Method, rt.Pattern, r.code)
		}
		for role, tok := range tokens {
			r := call(e.admin, rt.Method, path, tok, map[string]any{})
			allowed := admin.Allows(role, rt.Perm)
			if !allowed && r.code != http.StatusForbidden {
				t.Errorf("%s %s as %s: got %d, want 403", rt.Method, rt.Pattern, role, r.code)
			}
			if allowed && (r.code == http.StatusForbidden || r.code == http.StatusUnauthorized || r.code >= 500) {
				t.Errorf("%s %s as %s: got %d (%s)", rt.Method, rt.Pattern, role, r.code, r.body)
			}
		}
	}
}

func TestAdminAPISignInFlowOverHTTP(t *testing.T) {
	e := newAPIEnv(t)
	s := e.onboard("http@talksasa.co.ke", store.AdminRoleAdmin)
	e.clock.Advance(30 * time.Second)

	bad := call(e.admin, "POST", "/admin/v1/auth/login", "", map[string]any{"email": s.email, "password": "nope-nope-nope"})
	unknown := call(e.admin, "POST", "/admin/v1/auth/login", "", map[string]any{"email": "ghost@talksasa.co.ke", "password": "nope-nope-nope"})
	if bad.code != 401 || unknown.code != 401 || bad.json(t)["detail"] != unknown.json(t)["detail"] {
		t.Fatalf("sign-in failures distinguishable: %s | %s", bad.body, unknown.body)
	}
	if r := call(e.admin, "POST", "/admin/v1/auth/login", "", map[string]any{"email": s.email, "password": goodPassword, "extra": 1}); r.code != 400 {
		t.Fatalf("unknown field accepted: %d", r.code)
	}
	r := call(e.admin, "POST", "/admin/v1/auth/login", "", map[string]any{"email": s.email, "password": goodPassword})
	if r.code != 200 {
		t.Fatalf("login: %d %s", r.code, r.body)
	}
	challenge := r.json(t)["challenge"].(string)
	if call(e.admin, "GET", "/admin/v1/auth/me", challenge, nil).code != 401 {
		t.Fatal("challenge accepted as a session")
	}
	r = call(e.admin, "POST", "/admin/v1/auth/mfa", "", map[string]any{"challenge": challenge, "code": totp(t, s.secret, e.clock.Now())})
	if r.code != 200 {
		t.Fatalf("mfa: %d %s", r.code, r.body)
	}
	token := r.json(t)["session_token"].(string)
	me := call(e.admin, "GET", "/admin/v1/auth/me", token, nil)
	if me.code != 200 || me.json(t)["email"] != s.email || me.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("me: %d %s", me.code, me.body)
	}
	if call(e.admin, "POST", "/admin/v1/auth/logout", token, nil).code != 204 {
		t.Fatal("logout failed")
	}
	if call(e.admin, "GET", "/admin/v1/auth/me", token, nil).code != 401 {
		t.Fatal("session alive after logout")
	}
}

func TestAdminAPIClientIPOnlyFromTrustedProxy(t *testing.T) {
	e := newAPIEnv(t)
	s := e.onboard("ip@talksasa.co.ke", store.AdminRoleViewer)
	e.clock.Advance(30 * time.Second)
	spoof := func(peer string) func(*http.Request) {
		return func(r *http.Request) {
			r.RemoteAddr = peer
			r.Header.Set(httpx.ClientIPHeader, "198.51.100.200")
			r.Header.Set(httpx.ClientUAHeader, "Browser/1.0")
		}
	}
	r := call(e.admin, "POST", "/admin/v1/auth/login", "", map[string]any{"email": s.email, "password": goodPassword}, spoof("203.0.113.50:1234"))
	if r.code != 200 {
		t.Fatal(r.body)
	}
	if strings.Contains(e.logs.String(), "198.51.100.200") {
		t.Fatal("client IP header from an untrusted peer was believed")
	}
	call(e.admin, "POST", "/admin/v1/auth/login", "", map[string]any{"email": s.email, "password": "wrong-wrong-wrong"}, spoof("10.9.9.9:1234"))
	if !strings.Contains(e.logs.String(), "198.51.100.200") {
		t.Fatal("client IP header from a trusted proxy was ignored")
	}
}

func TestAdminAPIListsDetailsAndPagination(t *testing.T) {
	e := newAPIEnv(t)
	tok := e.signIn(e.onboard("lists@talksasa.co.ke", store.AdminRoleViewer)).Token
	a := e.app("LST")
	var charges []store.Charge
	for i := 0; i < 5; i++ {
		charges = append(charges, e.charge(a.App.ID, 1000+int64(i)))
	}
	txn := e.payCharge(charges[0])
	unmatched := seed.Payment(t, e.ctx, e.db.App, nil, "ZZZ-1", 700)

	// Pagination: 5 charges, pages of 2, newest first, no overlap.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		r := call(e.admin, "GET", "/admin/v1/charges?limit=2&app_id="+a.App.ID.String()+"&cursor="+cursor, tok, nil)
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		m := r.json(t)
		for _, it := range m["items"].([]any) {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("charge %s on two pages", id)
			}
			seen[id] = true
		}
		pages++
		next, _ := m["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("saw %d charges over %d pages", len(seen), pages)
	}

	// Search by account number typed loosely, and by reference.
	for _, q := range []string{strings.ToLower(strings.ReplaceAll(charges[1].AccountNumber, "-", "")), strings.ToLower(charges[1].Reference)} {
		r := call(e.admin, "GET", "/admin/v1/charges?q="+urlEscape(q), tok, nil)
		if r.code != 200 || !strings.Contains(r.body, charges[1].ID.String()) {
			t.Errorf("charge search %q: %d %s", q, r.code, r.body)
		}
	}
	// LIKE wildcards are literal.
	if r := call(e.admin, "GET", "/admin/v1/charges?q=%25", tok, nil); strings.Contains(r.body, charges[1].ID.String()) {
		t.Error("'%' matched everything")
	}

	r := call(e.admin, "GET", "/admin/v1/transactions/"+txn.ID.String(), tok, nil)
	if r.code != 200 {
		t.Fatal(r.body)
	}
	if strings.Contains(r.body, "254712345678") || !strings.Contains(r.body, "2547*****678") {
		t.Fatalf("phone not masked in transaction detail: %s", r.body)
	}
	d := r.json(t)
	if len(d["events"].([]any)) == 0 || d["charge_id"] != charges[0].ID.String() {
		t.Fatalf("detail missing events/charge: %s", r.body)
	}
	r = call(e.admin, "GET", "/admin/v1/charges/"+charges[0].ID.String(), tok, nil)
	if r.code != 200 || r.json(t)["status"] != "succeeded" || len(r.json(t)["transactions"].([]any)) != 1 {
		t.Fatalf("charge detail: %s", r.body)
	}
	r = call(e.admin, "GET", "/admin/v1/transactions/unmatched", tok, nil)
	if r.code != 200 || !strings.Contains(r.body, unmatched.ID.String()) || strings.Contains(r.body, txn.ID.String()) {
		t.Fatalf("unmatched: %s", r.body)
	}
	for _, bad := range []string{"/admin/v1/transactions?status=bogus", "/admin/v1/transactions?from=yesterday", "/admin/v1/charges?cursor=@@",
		"/admin/v1/transactions?phone=12", "/admin/v1/charges?status=paid"} {
		if r := call(e.admin, "GET", bad, tok, nil); r.code != 400 {
			t.Errorf("%s: %d", bad, r.code)
		}
	}
	dash := call(e.admin, "GET", "/admin/v1/dashboard", tok, nil)
	if dash.code != 200 || int(dash.json(t)["today"].(map[string]any)["total"].(float64)) != 1700 {
		t.Fatalf("dashboard: %s", dash.body)
	}
	counts := dash.json(t)["counts"].(map[string]any)
	if counts["charges_pending"].(float64) != 4 || counts["unmatched_count"].(float64) != 1 || counts["unmatched_amount"].(float64) != 700 {
		t.Fatalf("dashboard counts: %v", counts)
	}
	integ := call(e.admin, "GET", "/admin/v1/integrity", tok, nil)
	if integ.code != 200 || integ.json(t)["audit_chain_ok"] != true {
		t.Fatalf("integrity: %s", integ.body)
	}
	aud := call(e.admin, "GET", "/admin/v1/audit?action=app.", tok, nil)
	if aud.code != 200 || !strings.Contains(aud.body, "app.created") {
		t.Fatalf("audit: %s", aud.body)
	}
}

func TestAdminAPIAppManagementAndSecretsShownOnce(t *testing.T) {
	e := newAPIEnv(t)
	adminTok := e.signIn(e.onboard("pm@talksasa.co.ke", store.AdminRoleAdmin)).Token
	r := call(e.admin, "POST", "/admin/v1/apps", adminTok, map[string]any{"name": "Domains", "account_prefix": "DOM"})
	if r.code != 201 {
		t.Fatal(r.body)
	}
	created := r.json(t)
	secret := created["webhook_secret"].(string)
	pid := created["app"].(map[string]any)["id"].(string)
	if g := call(e.admin, "GET", "/admin/v1/apps/"+pid, adminTok, nil); strings.Contains(g.body, secret) || strings.Contains(g.body, "whsec_") {
		t.Fatal("webhook secret re-readable")
	}
	k := call(e.admin, "POST", "/admin/v1/apps/"+pid+"/api-keys", adminTok, map[string]any{"label": "staging"})
	if k.code != 201 {
		t.Fatal(k.body)
	}
	apiKey := k.json(t)["api_key"].(string)
	keyID := k.json(t)["key"].(map[string]any)["key_id"].(string)
	if l := call(e.admin, "GET", "/admin/v1/apps/"+pid+"/api-keys", adminTok, nil); strings.Contains(l.body, apiKey[len(apiKey)-20:]) {
		t.Fatal("API key secret re-readable")
	}
	if call(e.pub, "GET", "/v1/whoami", apiKey, nil).code != 200 {
		t.Fatal("issued key does not work")
	}
	if call(e.admin, "POST", "/admin/v1/apps/"+pid+"/status", adminTok, map[string]any{"status": "suspended"}).code != 200 {
		t.Fatal("suspend failed")
	}
	if call(e.pub, "GET", "/v1/whoami", apiKey, nil).code != 401 {
		t.Fatal("suspended app's key still works")
	}
	call(e.admin, "POST", "/admin/v1/apps/"+pid+"/status", adminTok, map[string]any{"status": "active"})
	if call(e.admin, "POST", "/admin/v1/apps/"+pid+"/api-keys/"+keyID+"/revoke", adminTok, nil).code != 204 {
		t.Fatal("revoke failed")
	}
	if call(e.pub, "GET", "/v1/whoami", apiKey, nil).code != 401 {
		t.Fatal("revoked key still works")
	}
	if strings.Contains(e.logs.String(), secret) || strings.Contains(e.logs.String(), apiKey) {
		t.Fatal("secret in logs")
	}
	// Staff invite returns a setup link once; the users list never does.
	inv := call(e.admin, "POST", "/admin/v1/users", adminTok, map[string]any{"email": "new@talksasa.co.ke", "name": "New Person", "role": "viewer"})
	if inv.code != 201 || !strings.HasPrefix(inv.json(t)["setup_url"].(string), "https://admin.test/setup/") {
		t.Fatal(inv.body)
	}
	if l := call(e.admin, "GET", "/admin/v1/users", adminTok, nil); strings.Contains(l.body, "setup/") || strings.Contains(l.body, "password") || strings.Contains(l.body, "totp") {
		t.Fatalf("users list leaks credentials: %s", l.body)
	}
}

func TestPublicPayPage(t *testing.T) {
	e := newAPIEnv(t)
	a := e.app("PAY")
	ch := e.charge(a.App.ID, 2500)

	r := call(e.pub, "GET", "/public/v1/pay/"+ch.HostedToken, "", nil)
	if r.code != 200 {
		t.Fatal(r.body)
	}
	m := r.json(t)
	if m["amount"].(float64) != 2500 || m["payable"] != true || m["stk_available"] != false || m["status"] != "pending" ||
		m["reference"] != ch.Reference || m["app_name"] != "PAY app" {
		t.Fatalf("%s", r.body)
	}
	pb := m["paybill"].(map[string]any)
	if pb["business_number"] != "4123456" || pb["account_number"] != ch.AccountNumber {
		t.Fatalf("paybill instructions: %s", r.body)
	}
	for _, leak := range []string{"254712345678", ch.ID.String(), a.App.ID.String(), ch.IdempotencyKey} {
		if strings.Contains(r.body, leak) {
			t.Errorf("pay page leaks %q", leak)
		}
	}
	if r.hdr.Get("X-Robots-Tag") == "" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Error("missing noindex / no-store")
	}

	// Malformed and unknown tokens are indistinguishable.
	x := call(e.pub, "GET", "/public/v1/pay/not-a-token", "", nil)
	y := call(e.pub, "GET", "/public/v1/pay/"+strings.Repeat("a", 52), "", nil)
	if x.code != 404 || y.code != 404 || x.json(t)["detail"] != y.json(t)["detail"] {
		t.Fatalf("token probing possible: %s | %s", x.body, y.body)
	}

	// Paid: no longer payable, no paybill instructions.
	e.payCharge(ch)
	m = call(e.pub, "GET", "/public/v1/pay/"+ch.HostedToken, "", nil).json(t)
	if m["status"] != "succeeded" || m["payable"] != false || m["paybill"] != nil || m["succeeded_at"] == nil {
		t.Fatalf("paid charge: %v", m)
	}

	// Suspended app: the link stops resolving.
	if _, err := e.apps.SetStatus(e.ctx, audit.CLI("t"), a.App.ID, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if call(e.pub, "GET", "/public/v1/pay/"+ch.HostedToken, "", nil).code != 404 {
		t.Fatal("suspended app's pay page still resolves")
	}
}

func TestPublicPayPageRateLimited(t *testing.T) {
	e := newAPIEnv(t)
	h := httpapi.NewRouter(httpapi.Deps{Logger: e.logger, Pool: e.db.App, Apps: e.apps, Reporting: reporting.NewService(e.db.App),
		MaxBodyBytes: 1 << 20, PayLimiter: httpx.NewRateLimiter(60, 3)})
	codes := []int{}
	for i := 0; i < 5; i++ {
		codes = append(codes, call(h, "GET", "/public/v1/pay/"+strings.Repeat("b", 52), "", nil).code)
	}
	if codes[3] != http.StatusTooManyRequests {
		t.Fatalf("no rate limit: %v", codes)
	}
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "+", "%", "%25").Replace(s)
}
