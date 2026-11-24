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
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
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
		Logger: a.logger, Admin: a.svc, Products: a.products, Reporting: rep, MaxBodyBytes: 64 << 10,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		AuthLimiter:    httpx.NewRateLimiter(6000, 6000), // tests exercise auth many times
	}
	return &apiEnv{
		adminEnv: a, deps: deps, admin: adminapi.NewRouter(deps),
		pub: httpapi.NewRouter(httpapi.Deps{Logger: a.logger, Pool: a.db.App, Products: a.products, Reporting: rep,
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
	fill := strings.NewReplacer("{id}", "00000000-0000-7000-8000-000000000000", "{keyID}", "abcdefghijklmnopqrst")
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
	prod := e.product("LST")
	py := e.payer(prod.Product.ID, "lst-1")
	var reqIDs []string
	for i := 0; i < 5; i++ {
		reqIDs = append(reqIDs, e.request(prod.Product.ID, py.ID, 1000).ID.String())
	}
	txn, _ := e.pay(py, money.KES(1500), nil, newReceipt())

	// Pagination: 5 requests, pages of 2, newest first, no overlap.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		r := call(e.admin, "GET", "/admin/v1/payment-requests?limit=2&product_id="+prod.Product.ID.String()+"&cursor="+cursor, tok, nil)
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		m := r.json(t)
		for _, it := range m["items"].([]any) {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("request %s on two pages", id)
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
		t.Fatalf("saw %d requests over %d pages", len(seen), pages)
	}

	// Search by account number typed loosely, and by phone.
	for _, q := range []string{strings.ToLower(strings.ReplaceAll(py.AccountNumber, "-", "")), "0712345678", "customer lst"} {
		r := call(e.admin, "GET", "/admin/v1/payers?q="+urlEscape(q), tok, nil)
		if r.code != 200 || !strings.Contains(r.body, py.ID.String()) {
			t.Errorf("payer search %q: %d %s", q, r.code, r.body)
		}
	}
	// LIKE wildcards are literal.
	if r := call(e.admin, "GET", "/admin/v1/payers?q=%25", tok, nil); strings.Contains(r.body, py.ID.String()) {
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
	if len(d["allocations"].([]any)) == 0 || len(d["events"].([]any)) == 0 {
		t.Fatalf("detail missing allocations/events: %s", r.body)
	}
	r = call(e.admin, "GET", "/admin/v1/payment-requests/"+reqIDs[0], tok, nil)
	if r.code != 200 || r.json(t)["status"] != "paid" {
		t.Fatalf("oldest request should be paid first: %s", r.body)
	}
	for _, bad := range []string{"/admin/v1/transactions?status=bogus", "/admin/v1/transactions?from=yesterday", "/admin/v1/payers?cursor=@@", "/admin/v1/transactions?phone=12"} {
		if r := call(e.admin, "GET", bad, tok, nil); r.code != 400 {
			t.Errorf("%s: %d", bad, r.code)
		}
	}
	dash := call(e.admin, "GET", "/admin/v1/dashboard", tok, nil)
	if dash.code != 200 || int(dash.json(t)["today"].(map[string]any)["total"].(float64)) != 1500 {
		t.Fatalf("dashboard: %s", dash.body)
	}
	integ := call(e.admin, "GET", "/admin/v1/integrity", tok, nil)
	if integ.code != 200 || integ.json(t)["audit_chain_ok"] != true || integ.json(t)["ledger_ok"] != true {
		t.Fatalf("integrity: %s", integ.body)
	}
	aud := call(e.admin, "GET", "/admin/v1/audit?action=product.", tok, nil)
	if aud.code != 200 || !strings.Contains(aud.body, "product.created") {
		t.Fatalf("audit: %s", aud.body)
	}
}

func TestAdminAPIProductManagementAndSecretsShownOnce(t *testing.T) {
	e := newAPIEnv(t)
	adminTok := e.signIn(e.onboard("pm@talksasa.co.ke", store.AdminRoleAdmin)).Token
	r := call(e.admin, "POST", "/admin/v1/products", adminTok, map[string]any{"name": "Domains", "account_prefix": "DOM"})
	if r.code != 201 {
		t.Fatal(r.body)
	}
	created := r.json(t)
	secret := created["webhook_secret"].(string)
	pid := created["product"].(map[string]any)["id"].(string)
	if g := call(e.admin, "GET", "/admin/v1/products/"+pid, adminTok, nil); strings.Contains(g.body, secret) || strings.Contains(g.body, "whsec_") {
		t.Fatal("webhook secret re-readable")
	}
	k := call(e.admin, "POST", "/admin/v1/products/"+pid+"/api-keys", adminTok, map[string]any{"label": "staging"})
	if k.code != 201 {
		t.Fatal(k.body)
	}
	apiKey := k.json(t)["api_key"].(string)
	keyID := k.json(t)["key"].(map[string]any)["key_id"].(string)
	if l := call(e.admin, "GET", "/admin/v1/products/"+pid+"/api-keys", adminTok, nil); strings.Contains(l.body, apiKey[len(apiKey)-20:]) {
		t.Fatal("API key secret re-readable")
	}
	if call(e.pub, "GET", "/v1/whoami", apiKey, nil).code != 200 {
		t.Fatal("issued key does not work")
	}
	if call(e.admin, "POST", "/admin/v1/products/"+pid+"/status", adminTok, map[string]any{"status": "suspended"}).code != 200 {
		t.Fatal("suspend failed")
	}
	if call(e.pub, "GET", "/v1/whoami", apiKey, nil).code != 401 {
		t.Fatal("suspended product's key still works")
	}
	call(e.admin, "POST", "/admin/v1/products/"+pid+"/status", adminTok, map[string]any{"status": "active"})
	if call(e.admin, "POST", "/admin/v1/products/"+pid+"/api-keys/"+keyID+"/revoke", adminTok, nil).code != 204 {
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
	prod := e.product("PAY")
	py := e.payer(prod.Product.ID, "pay-1")
	req := e.request(prod.Product.ID, py.ID, 2500)

	r := call(e.pub, "GET", "/public/v1/pay/"+req.HostedToken, "", nil)
	if r.code != 200 {
		t.Fatal(r.body)
	}
	m := r.json(t)
	if m["outstanding"].(float64) != 2500 || m["payable"] != true || m["stk_available"] != false {
		t.Fatalf("%s", r.body)
	}
	pb := m["paybill"].(map[string]any)
	if pb["business_number"] != "4123456" || pb["account_number"] != py.AccountNumber {
		t.Fatalf("paybill instructions: %s", r.body)
	}
	for _, leak := range []string{py.DisplayName, py.Email, py.ExternalRef, "254712345678", py.ID.String(), req.ID.String(), prod.Product.ID.String()} {
		if strings.Contains(r.body, leak) {
			t.Errorf("pay page leaks %q", leak)
		}
	}
	if r.hdr.Get("X-Robots-Tag") == "" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Error("missing noindex / no-store")
	}

	// Malformed and unknown tokens are indistinguishable.
	a := call(e.pub, "GET", "/public/v1/pay/not-a-token", "", nil)
	b := call(e.pub, "GET", "/public/v1/pay/"+strings.Repeat("a", 52), "", nil)
	if a.code != 404 || b.code != 404 || a.json(t)["detail"] != b.json(t)["detail"] {
		t.Fatalf("token probing possible: %s | %s", a.body, b.body)
	}

	// Paid: no longer payable, no paybill instructions.
	e.pay(py, money.KES(2500), &req.ID, newReceipt())
	m = call(e.pub, "GET", "/public/v1/pay/"+req.HostedToken, "", nil).json(t)
	if m["status"] != "paid" || m["payable"] != false || m["paybill"] != nil {
		t.Fatalf("paid request: %v", m)
	}

	// Suspended product: the link stops resolving.
	if _, err := e.products.SetStatus(e.ctx, audit.CLI("t"), prod.Product.ID, store.ProductStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if call(e.pub, "GET", "/public/v1/pay/"+req.HostedToken, "", nil).code != 404 {
		t.Fatal("suspended product's pay page still resolves")
	}
}

func TestPublicPayPageRateLimited(t *testing.T) {
	e := newAPIEnv(t)
	h := httpapi.NewRouter(httpapi.Deps{Logger: e.logger, Pool: e.db.App, Products: e.products, Reporting: reporting.NewService(e.db.App),
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
