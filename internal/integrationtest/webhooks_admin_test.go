package integrationtest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/store"
)

func (e *apiEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Owner.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAdminAPIWebhookDeliveries(t *testing.T) {
	e := newAPIEnv(t)
	viewer := e.signIn(e.onboard("whview@talksasa.co.ke", store.AdminRoleViewer)).Token
	adm := e.signIn(e.onboard("whadmin@talksasa.co.ke", store.AdminRoleAdmin)).Token
	p := e.product("WHA")
	other := e.product("WHB")
	py := e.payer(p.Product.ID, "wh-1")
	r1 := e.request(p.Product.ID, py.ID, 1000)
	e.request(p.Product.ID, py.ID, 2000)
	e.request(other.Product.ID, e.payer(other.Product.ID, "wh-2").ID, 500)

	var evID string
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT id::text FROM events WHERE subject_id = $1`, r1.ID).Scan(&evID); err != nil {
		t.Fatal(err)
	}

	// List: filters and keyset pagination.
	base := "/admin/v1/webhook-deliveries?product_id=" + p.Product.ID.String()
	r := call(e.admin, "GET", base, viewer, nil)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	items := r.json(t)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 deliveries for product, got %d", len(items))
	}
	first := items[0].(map[string]any)
	for _, k := range []string{"event_id", "product_id", "account_prefix", "event_type", "subject_type", "subject_id", "status",
		"attempts", "last_attempt_at", "last_status_code", "last_error", "delivered_at", "created_at"} {
		if _, ok := first[k]; !ok {
			t.Errorf("delivery JSON lacks %q", k)
		}
	}
	if first["account_prefix"] != "WHA" || first["status"] != "pending" || first["event_type"] != "payment_request.created" {
		t.Fatalf("row: %v", first)
	}
	seen := map[string]bool{}
	cursor := ""
	for range 5 {
		r := call(e.admin, "GET", base+"&limit=1&cursor="+cursor, viewer, nil)
		m := r.json(t)
		for _, it := range m["items"].([]any) {
			seen[it.(map[string]any)["event_id"].(string)] = true
		}
		next, _ := m["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 2 {
		t.Fatalf("pagination saw %d", len(seen))
	}
	if n := len(call(e.admin, "GET", "/admin/v1/webhook-deliveries?status=pending", viewer, nil).json(t)["items"].([]any)); n != 3 {
		t.Fatalf("status filter: %d", n)
	}
	if n := len(call(e.admin, "GET", "/admin/v1/webhook-deliveries?status=failed", viewer, nil).json(t)["items"].([]any)); n != 0 {
		t.Fatalf("failed filter: %d", n)
	}
	for _, q := range []string{"status=bogus", "cursor=%21%21", "product_id=nope"} {
		if r := call(e.admin, "GET", "/admin/v1/webhook-deliveries?"+q, viewer, nil); r.code != 400 {
			t.Errorf("%s: %d %s", q, r.code, r.body)
		}
	}

	// Detail.
	r = call(e.admin, "GET", "/admin/v1/webhook-deliveries/"+evID, viewer, nil)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	m := r.json(t)
	ev := m["event"].(map[string]any)
	if m["delivery"].(map[string]any)["event_id"] != evID || len(m["attempts"].([]any)) != 0 ||
		ev["id"] != evID || ev["type"] != "payment_request.created" || ev["data"].(map[string]any)["object"] != "payment_request" {
		t.Fatalf("detail: %s", r.body)
	}
	if r := call(e.admin, "GET", "/admin/v1/webhook-deliveries/00000000-0000-7000-8000-000000000000", viewer, nil); r.code != 404 {
		t.Fatalf("unknown: %d", r.code)
	}

	// Redeliver: permission, then 409 while pending.
	redeliver := "/admin/v1/webhook-deliveries/" + evID + "/redeliver"
	if r := call(e.admin, "POST", redeliver, viewer, nil); r.code != http.StatusForbidden {
		t.Fatalf("viewer redelivered: %d", r.code)
	}
	if r := call(e.admin, "POST", redeliver, adm, nil); r.code != http.StatusConflict {
		t.Fatalf("pending redelivered: %d %s", r.code, r.body)
	}

	// Simulate a delivery that ran out of retries.
	if _, err := e.db.Owner.Exec(e.ctx, `UPDATE webhook_deliveries SET status = 'failed', attempts = 25, last_status_code = 500,
		last_error = 'HTTP 500', last_attempt_at = now() WHERE event_id = $1`, evID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Owner.Exec(e.ctx, `INSERT INTO webhook_attempts (event_id, attempt, url_host, status_code, latency_ms, error, response_body)
		VALUES ($1, 25, 'wha.example.com', 500, 120, 'HTTP 500', 'oops')`, evID); err != nil {
		t.Fatal(err)
	}
	r = call(e.admin, "GET", "/admin/v1/webhook-deliveries/"+evID, viewer, nil)
	att := r.json(t)["attempts"].([]any)
	if len(att) != 1 || att[0].(map[string]any)["url_host"] != "wha.example.com" || att[0].(map[string]any)["response_body"] != "oops" {
		t.Fatalf("attempts: %s", r.body)
	}

	jobsBefore := e.count(`SELECT count(*) FROM river_job WHERE args->>'event_id' = $1`, evID)
	r = call(e.admin, "POST", redeliver, adm, nil)
	if r.code != 200 || r.json(t)["status"] != "pending" || r.json(t)["event_id"] != evID {
		t.Fatalf("redeliver: %d %s", r.code, r.body)
	}
	if n := e.count(`SELECT count(*) FROM river_job WHERE args->>'event_id' = $1`, evID); n != jobsBefore+1 {
		t.Fatalf("jobs: before %d after %d", jobsBefore, n)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'webhook.redelivered' AND entity_id = $1 AND product_id = $2`, evID, p.Product.ID); n != 1 {
		t.Fatalf("audit entries: %d", n)
	}
	if r := call(e.admin, "POST", redeliver, adm, nil); r.code != http.StatusConflict {
		t.Fatalf("second redeliver: %d", r.code)
	}
	if r := call(e.admin, "POST", "/admin/v1/webhook-deliveries/00000000-0000-7000-8000-000000000000/redeliver", adm, nil); r.code != 404 {
		t.Fatalf("unknown redeliver: %d", r.code)
	}
}

func TestAdminAPIAllowedIPs(t *testing.T) {
	e := newAPIEnv(t)
	viewer := e.signIn(e.onboard("ipview@talksasa.co.ke", store.AdminRoleViewer)).Token
	adm := e.signIn(e.onboard("ipadmin@talksasa.co.ke", store.AdminRoleAdmin)).Token
	p := e.product("IPS")
	k, err := e.products.IssueAPIKey(e.ctx, operator, p.Product.ID, "server", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/v1/products/" + p.Product.ID.String() + "/api-keys/" + k.Key.KeyID + "/allowed-ips"

	if r := call(e.admin, "PUT", path, viewer, map[string]any{"allowed_ips": []string{"203.0.113.4"}}); r.code != http.StatusForbidden {
		t.Fatalf("viewer: %d", r.code)
	}
	for _, bad := range []any{[]string{"not-an-ip"}, []string{"0.0.0.0/0"}, []string{""}, make([]string, 51)} {
		if r := call(e.admin, "PUT", path, adm, map[string]any{"allowed_ips": bad}); r.code != 400 {
			t.Errorf("%v: %d %s", bad, r.code, r.body)
		}
	}
	if r := call(e.admin, "PUT", path, adm, map[string]any{"allowed_ips": []string{"1.2.3.4"}, "x": 1}); r.code != 400 {
		t.Fatalf("unknown field: %d", r.code)
	}

	r := call(e.admin, "PUT", path, adm, map[string]any{"allowed_ips": []string{"203.0.113.4", " 10.1.2.3/8 ", "2001:db8::/32"}})
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	got := r.json(t)["allowed_ips"].([]any)
	if len(got) != 3 || got[0] != "203.0.113.4" || got[1] != "10.0.0.0/8" || got[2] != "2001:db8::/32" {
		t.Fatalf("allowed_ips: %v", got)
	}
	list := call(e.admin, "GET", "/admin/v1/products/"+p.Product.ID.String()+"/api-keys", viewer, nil)
	if !strings.Contains(list.body, `"allowed_ips":["203.0.113.4","10.0.0.0/8","2001:db8::/32"]`) {
		t.Fatalf("list: %s", list.body)
	}

	for _, clear := range []any{nil, []string{}} {
		r = call(e.admin, "PUT", path, adm, map[string]any{"allowed_ips": clear})
		if r.code != 200 || r.json(t)["allowed_ips"] != nil {
			t.Fatalf("clear with %v: %d %s", clear, r.code, r.body)
		}
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'api_key.allowed_ips_changed' AND entity_id = $1`, k.Key.KeyID); n != 3 {
		t.Fatalf("audit entries: %d", n)
	}

	if r := call(e.admin, "PUT", "/admin/v1/products/"+p.Product.ID.String()+"/api-keys/abcdefghijklmnopqrst/allowed-ips", adm,
		map[string]any{"allowed_ips": nil}); r.code != 404 {
		t.Fatalf("unknown key: %d", r.code)
	}
	if r := call(e.admin, "PUT", "/admin/v1/products/00000000-0000-7000-8000-000000000000/api-keys/"+k.Key.KeyID+"/allowed-ips", adm,
		map[string]any{"allowed_ips": nil}); r.code != 404 {
		t.Fatalf("unknown product: %d", r.code)
	}
	// Another product's key can't be changed through this product's path.
	o := e.product("IPT")
	if r := call(e.admin, "PUT", "/admin/v1/products/"+o.Product.ID.String()+"/api-keys/"+k.Key.KeyID+"/allowed-ips", adm,
		map[string]any{"allowed_ips": nil}); r.code != 404 {
		t.Fatalf("cross-product key: %d", r.code)
	}
}
