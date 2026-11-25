package appapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/appapi"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
)

// fakeCharges stands in for *charge.Service (the money core is tested in
// internal/charge; here only the HTTP mapping is under test).
type fakeCharges struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]store.Charge
	byKey   map[string]uuid.UUID // app/idempotency key -> charge
	creates []charge.CreateParams
	actors  []audit.Actor
	lists   []charge.ListParams
	// create overrides Create's outcome when set.
	create func(appID uuid.UUID, p charge.CreateParams) (charge.Created, error)
	// err, when set, is returned by Get, List and Cancel.
	err error
	// anyApp makes Get return charges of any app (to test the handler's
	// own app check).
	anyApp bool
	clock  time.Time
}

func newFakeCharges() *fakeCharges {
	return &fakeCharges{byID: map[uuid.UUID]store.Charge{}, byKey: map[string]uuid.UUID{}, clock: time.Now().UTC().Truncate(time.Microsecond)}
}

func withCharges(f appapi.ChargeService) func(*appapi.Deps) {
	return func(d *appapi.Deps) { d.Charges = f }
}

func (f *fakeCharges) Create(_ context.Context, actor audit.Actor, appID uuid.UUID, p charge.CreateParams) (charge.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, p)
	f.actors = append(f.actors, actor)
	if f.create != nil {
		return f.create(appID, p)
	}
	if id, ok := f.byKey[appID.String()+"/"+p.IdempotencyKey]; ok {
		return charge.Created{Charge: f.byID[id], Replayed: true}, nil
	}
	for _, c := range f.byID {
		if c.AppID == appID && c.Reference == p.Reference { // unique per app for ever
			return charge.Created{}, charge.ErrDuplicateReference
		}
	}
	c := f.newCharge(appID, p)
	f.byID[c.ID] = c
	f.byKey[appID.String()+"/"+p.IdempotencyKey] = c.ID
	return charge.Created{Charge: c}, nil
}

func (f *fakeCharges) newCharge(appID uuid.UUID, p charge.CreateParams) store.Charge {
	f.clock = f.clock.Add(time.Millisecond)
	acct := p.AccountNumber
	if acct == "" {
		acct = "GEN-1ABCDEFG"
	}
	exp := p.ExpiresIn
	if exp == 0 {
		exp = charge.DefaultExpiresIn
	}
	c := store.Charge{
		ID: uuid.New(), AppID: appID, Reference: p.Reference, Currency: "KES", Amount: p.Amount,
		Metadata: p.Metadata, AccountNumber: acct, Status: store.ChargeStatusPending,
		HostedToken: seed.HostedToken(), IdempotencyKey: p.IdempotencyKey, ExpiresAt: f.clock.Add(exp),
		CreatedAt: f.clock, UpdatedAt: f.clock,
	}
	if p.Description != "" {
		d := p.Description
		c.Description = &d
	}
	if p.Phone != "" {
		ph := fullPhone
		c.Phone = &ph
	}
	return c
}

func (f *fakeCharges) Get(_ context.Context, appID, id uuid.UUID) (charge.Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return charge.Charge{}, f.err
	}
	c, ok := f.byID[id]
	if !ok || (!f.anyApp && c.AppID != appID) {
		return charge.Charge{}, charge.ErrNotFound
	}
	return c, nil
}

func (f *fakeCharges) List(_ context.Context, appID uuid.UUID, p charge.ListParams) ([]charge.Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, p)
	if f.err != nil {
		return nil, f.err
	}
	var out []charge.Charge
	for _, c := range f.byID {
		if c.AppID != appID || (p.Status != nil && c.Status != *p.Status) || (p.Reference != "" && c.Reference != p.Reference) {
			continue
		}
		if p.CursorAt != nil {
			before := c.CreatedAt.Before(*p.CursorAt) || (c.CreatedAt.Equal(*p.CursorAt) && c.ID.String() < p.CursorID.String())
			if !before {
				continue
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

func (f *fakeCharges) Cancel(_ context.Context, _ audit.Actor, appID, id uuid.UUID) (charge.Cancelled, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return charge.Cancelled{}, f.err
	}
	c, ok := f.byID[id]
	switch {
	case !ok || c.AppID != appID:
		return charge.Cancelled{}, charge.ErrNotFound
	case c.Status == store.ChargeStatusCancelled:
		return charge.Cancelled{Charge: c, AlreadyCancelled: true}, nil
	case c.Status != store.ChargeStatusPending:
		return charge.Cancelled{}, charge.ErrNotCancellable
	}
	c.Status = store.ChargeStatusCancelled
	f.byID[id] = c
	return charge.Cancelled{Charge: c}, nil
}

func (f *fakeCharges) set(c store.Charge) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[c.ID] = c
}

func (f *fakeCharges) lastCreate() (charge.CreateParams, audit.Actor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates[len(f.creates)-1], f.actors[len(f.actors)-1]
}

// fakePushes stands in for the store's latest-prompt reads.
type fakePushes struct {
	mu     sync.Mutex
	latest map[uuid.UUID]store.StkPush
	err    error
}

func (p *fakePushes) GetLatestSTKPushForCharge(_ context.Context, id uuid.UUID) (store.StkPush, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return store.StkPush{}, p.err
	}
	if s, ok := p.latest[id]; ok {
		return s, nil
	}
	return store.StkPush{}, pgx.ErrNoRows
}

func (p *fakePushes) ListLatestSTKPushesForCharges(_ context.Context, a store.ListLatestSTKPushesForChargesParams) ([]store.StkPush, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	var out []store.StkPush
	for _, id := range a.ChargeIds {
		if s, ok := p.latest[id]; ok && s.AppID == a.AppID {
			out = append(out, s)
		}
	}
	return out, nil
}

func withPushes(p appapi.PushReader) func(*appapi.Deps) {
	return func(d *appapi.Deps) { d.Pushes = p }
}

func chargeBody(ref string, amount int64, extra string) string {
	b := fmt.Sprintf(`{"reference":%q,"amount":%d`, ref, amount)
	if extra != "" {
		b += "," + extra
	}
	return b + "}"
}

// ---------------------------------------------------------------------------

func TestCreateCharge(t *testing.T) {
	f := newFakeCharges()
	e := newEnv(t, nil, withCharges(f))
	tn := e.tenant("CRT")

	body := chargeBody("INV-1", 1500, `"description":"Hosting\nNovember","account_number":"CRT-1042",`+
		`"metadata":{ "invoice_id" : 42 },"expires_in":3600`)
	r := e.do("POST", "/v1/charges", tn.Key, body, hdr{"Idempotency-Key": "idem-1"})
	m := r.expect(201, "")
	if r.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("first create marked replayed")
	}
	p, actor := f.lastCreate()
	want := charge.CreateParams{
		Reference: "INV-1", Amount: 1500, Description: "Hosting\nNovember", AccountNumber: "CRT-1042",
		Metadata: json.RawMessage(`{"invoice_id":42}`), ExpiresIn: time.Hour, IdempotencyKey: "idem-1",
	}
	if fmt.Sprint(p) != fmt.Sprint(want) || actor != audit.App(tn.ID) {
		t.Fatalf("create params %+v actor %+v", p, actor)
	}
	id := uid(t, m)
	c, _ := f.Get(e.ctx, tn.ID, id)
	if m["object"] != "charge" || m["status"] != "pending" || m["amount"].(float64) != 1500 || m["currency"] != "KES" ||
		m["reference"] != "INV-1" || m["description"] != "Hosting\nNovember" || m["account_number"] != "CRT-1042" ||
		m["checkout_url"] != "https://pay.test/"+c.HostedToken || m["latest_stk_push"] != nil {
		t.Fatalf("charge %v", m)
	}
	if md := m["metadata"].(map[string]any); md["invoice_id"].(float64) != 42 {
		t.Fatalf("metadata %v", m["metadata"])
	}
	if pb := m["paybill"].(map[string]any); pb["business_number"] != "174379" || pb["account_number"] != "CRT-1042" {
		t.Fatalf("paybill %v", pb)
	}

	// Replay: 200 + Idempotent-Replayed, the same charge.
	r = e.do("POST", "/v1/charges", tn.Key, body, hdr{"Idempotency-Key": "idem-1"})
	if m := r.expect(200, ""); uid(t, m) != id || r.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %s %v", r.Body.String(), r.Header())
	}
	// Same reference, new key: 409 duplicate_reference.
	e.do("POST", "/v1/charges", tn.Key, body, hdr{"Idempotency-Key": "idem-2"}).expect(409, "duplicate_reference")

	// Minimal body: no metadata is null, generated account number.
	m = e.do("POST", "/v1/charges", tn.Key, chargeBody("INV-2", 1, `"metadata":null`), hdr{"Idempotency-Key": "idem-3"}).expect(201, "")
	if m["metadata"] != nil || m["description"] != nil || m["account_number"] != "GEN-1ABCDEFG" {
		t.Fatalf("minimal %v", m)
	}
	if p, _ := f.lastCreate(); p.Metadata != nil || p.ExpiresIn != 0 || p.Phone != "" {
		t.Fatalf("minimal params %+v", p)
	}
	e.assertNoLeaks()
}

func TestCreateChargeIdempotencyKey(t *testing.T) {
	f := newFakeCharges()
	e := newEnv(t, nil, withCharges(f))
	tn := e.tenant("CIK")
	body := chargeBody("INV-1", 100, "")
	e.do("POST", "/v1/charges", tn.Key, body, nil).expect(400, "idempotency_key_required")
	e.do("POST", "/v1/charges", tn.Key, body, hdr{"Idempotency-Key": "   "}).expect(400, "idempotency_key_required")
	r := e.do("POST", "/v1/charges", tn.Key, body, hdr{"Idempotency-Key": strings.Repeat("k", 256)})
	r.expect(400, "validation_failed")
	if fe := r.fieldErrors(); len(fe) != 1 || fe[0] != "Idempotency-Key" {
		t.Fatal(fe)
	}
	if len(f.creates) != 0 {
		t.Fatal("service called without a valid key")
	}
	// Unauthenticated callers learn nothing, not even that the key is missing.
	e.do("POST", "/v1/charges", "", body, nil).expect(401, "unauthorized")
}

func TestCreateChargeValidation(t *testing.T) {
	f := newFakeCharges()
	e := newEnv(t, nil, withCharges(f))
	tn := e.tenant("CVAL")
	cases := []struct {
		name, body string
		fields     []string
	}{
		{"empty", `{}`, []string{"reference", "amount"}},
		{"zero amount", chargeBody("R", 0, ""), []string{"amount"}},
		{"too much", chargeBody("R", 250_001, ""), []string{"amount"}},
		{"fraction", `{"reference":"R","amount":1.5}`, []string{"amount"}},
		{"string amount", `{"reference":"R","amount":"100"}`, []string{"amount"}},
		{"empty ref", chargeBody("", 100, ""), []string{"reference"}},
		{"long ref", chargeBody(strings.Repeat("r", 129), 100, ""), []string{"reference"}},
		{"control ref", chargeBody("a\tb", 100, ""), []string{"reference"}},
		{"empty desc", chargeBody("R", 100, `"description":""`), []string{"description"}},
		{"blank desc", chargeBody("R", 100, `"description":"   "`), []string{"description"}},
		{"blank ref", chargeBody("  ", 100, ""), []string{"reference"}},
		{"long desc", chargeBody("R", 100, `"description":"`+strings.Repeat("d", 501)+`"`), []string{"description"}},
		{"bad phone", chargeBody("R", 100, `"phone":"0800000000"`), []string{"phone"}},
		{"long phone", chargeBody("R", 100, `"phone":"`+strings.Repeat("7", 21)+`"`), []string{"phone"}},
		{"wrong prefix", chargeBody("R", 100, `"account_number":"OTHER-1"`), []string{"account_number"}},
		{"bad account", chargeBody("R", 100, `"account_number":"CVAL-X1"`), []string{"account_number"}},
		{"long account", chargeBody("R", 100, `"account_number":"CVAL-12345678"`), []string{"account_number"}},
		{"metadata array", chargeBody("R", 100, `"metadata":[1]`), []string{"metadata"}},
		{"metadata string", chargeBody("R", 100, `"metadata":"x"`), []string{"metadata"}},
		{"metadata big", chargeBody("R", 100, `"metadata":{"k":"`+strings.Repeat("m", 4090)+`"}`), []string{"metadata"}},
		{"short expiry", chargeBody("R", 100, `"expires_in":899`), []string{"expires_in"}},
		{"long expiry", chargeBody("R", 100, `"expires_in":604801`), []string{"expires_in"}},
		{"unknown field", chargeBody("R", 100, `"app_id":"x"`), []string{"app_id"}},
		{"many", `{"reference":"","amount":0,"expires_in":1}`, []string{"reference", "amount", "expires_in"}},
	}
	for i, c := range cases {
		r := e.do("POST", "/v1/charges", tn.Key, c.body, hdr{"Idempotency-Key": fmt.Sprint("v", i)})
		r.expect(422, "validation_failed")
		if got := r.fieldErrors(); fmt.Sprint(got) != fmt.Sprint(c.fields) {
			t.Errorf("%s: fields %v, want %v", c.name, got, c.fields)
		}
	}
	e.do("POST", "/v1/charges", tn.Key, `{"reference":`, hdr{"Idempotency-Key": "j"}).expect(400, "invalid_json")
	e.do("POST", "/v1/charges", tn.Key, `{}{}`, hdr{"Idempotency-Key": "j"}).expect(400, "invalid_json")
	e.do("POST", "/v1/charges", tn.Key, chargeBody("R", 1, ""), hdr{"Idempotency-Key": "j", "Content-Type": "text/plain"}).
		expect(415, "unsupported_media_type")
	if len(f.creates) != 0 {
		t.Fatalf("service called for invalid bodies: %d", len(f.creates))
	}
	// Edges that are valid.
	ok := chargeBody("R-edge", 250_000, `"expires_in":604800,"description":"`+strings.Repeat("é", 500)+`","phone":"+254 712 345 678",`+
		`"account_number":"CVAL-1234567"`)
	e.do("POST", "/v1/charges", tn.Key, ok, hdr{"Idempotency-Key": "edge"}).expect(201, "")
	if p, _ := f.lastCreate(); p.Phone != "+254 712 345 678" || p.ExpiresIn != 7*24*time.Hour || p.AccountNumber != "CVAL-1234567" {
		t.Fatalf("%+v", p)
	}
}

func TestCreateChargeErrorMapping(t *testing.T) {
	f := newFakeCharges()
	e := newEnv(t, nil, withCharges(f))
	tn := e.tenant("CERR")
	cases := []struct {
		err    error
		status int
		code   string
		field  string
	}{
		{charge.ErrDuplicateReference, 409, "duplicate_reference", ""},
		{charge.ErrIdempotencyConflict, 409, "idempotency_conflict", ""},
		{charge.ErrAppNotActive, 403, "app_suspended", ""},
		{fmt.Errorf("wrapped: %w", charge.ErrInvalidAmount), 422, "validation_failed", "amount"},
		{charge.ErrInvalidReference, 422, "validation_failed", "reference"},
		{charge.ErrInvalidDescription, 422, "validation_failed", "description"},
		{charge.ErrInvalidMetadata, 422, "validation_failed", "metadata"},
		{charge.ErrInvalidAccountNumber, 422, "validation_failed", "account_number"},
		{charge.ErrAccountNumberInUse, 422, "validation_failed", "account_number"},
		{charge.ErrInvalidPhone, 422, "validation_failed", "phone"},
		{charge.ErrInvalidExpiry, 422, "validation_failed", "expires_in"},
		{charge.ErrInvalidIdempotency, 400, "validation_failed", "Idempotency-Key"},
		{context.DeadlineExceeded, 503, "overloaded", ""},
		{errors.New("boom: secret detail"), 500, "internal_error", ""},
	}
	for i, c := range cases {
		f.create = func(uuid.UUID, charge.CreateParams) (charge.Created, error) { return charge.Created{}, c.err }
		r := e.do("POST", "/v1/charges", tn.Key, chargeBody("R", 100, ""), hdr{"Idempotency-Key": fmt.Sprint("e", i)})
		r.expect(c.status, c.code)
		if c.field != "" {
			if fe := r.fieldErrors(); len(fe) != 1 || fe[0] != c.field {
				t.Errorf("%v: fields %v", c.err, fe)
			}
		}
		if strings.Contains(r.Body.String(), "secret detail") {
			t.Fatal("internal error leaked")
		}
	}
	// A service bug returning another app's charge is never rendered.
	other := uuid.New()
	f.create = func(_ uuid.UUID, p charge.CreateParams) (charge.Created, error) {
		return charge.Created{Charge: f.newCharge(other, p)}, nil
	}
	r := e.do("POST", "/v1/charges", tn.Key, chargeBody("R", 100, ""), hdr{"Idempotency-Key": "x"})
	r.expect(500, "internal_error")
}

func TestCreateChargeWithPhone(t *testing.T) {
	f := newFakeCharges()
	pushes := &fakePushes{latest: map[uuid.UUID]store.StkPush{}}
	e := newEnv(t, nil, withCharges(f), withPushes(pushes))
	tn := e.tenant("CPH")

	// Prompt sent: embedded as latest_stk_push, phone masked.
	f.create = func(appID uuid.UUID, p charge.CreateParams) (charge.Created, error) {
		c := f.newCharge(appID, p)
		push := testPush(appID, c.ID, store.StkPushStatusSent)
		return charge.Created{Charge: c, Push: &push}, nil
	}
	m := e.do("POST", "/v1/charges", tn.Key, chargeBody("P1", 1500, `"phone":"0712345678"`), hdr{"Idempotency-Key": "p1"}).expect(201, "")
	sp := m["latest_stk_push"].(map[string]any)
	if sp["status"] != "sent" || sp["phone"] != "2547*****678" || sp["charge_id"] != m["id"] || sp["object"] != "stk_push" {
		t.Fatalf("push %v", sp)
	}
	if p, _ := f.lastCreate(); p.Phone != "0712345678" {
		t.Fatalf("phone not passed: %+v", p)
	}

	// Safaricom refused the prompt: the charge is still 201, the push failed.
	f.create = func(appID uuid.UUID, p charge.CreateParams) (charge.Created, error) {
		c := f.newCharge(appID, p)
		push := testPush(appID, c.ID, store.StkPushStatusFailed)
		desc := "Daraja said no"
		push.ResultDesc = &desc
		return charge.Created{Charge: c, Push: &push, PushErr: stk.ErrDaraja}, nil
	}
	m = e.do("POST", "/v1/charges", tn.Key, chargeBody("P2", 1500, `"phone":"0712345678"`), hdr{"Idempotency-Key": "p2"}).expect(201, "")
	if sp := m["latest_stk_push"].(map[string]any); sp["status"] != "failed" || sp["result_desc"] != "Daraja said no" || m["status"] != "pending" {
		t.Fatalf("failed push %v", m)
	}

	// M-Pesa off: no push at all, latest_stk_push null.
	f.create = func(appID uuid.UUID, p charge.CreateParams) (charge.Created, error) {
		return charge.Created{Charge: f.newCharge(appID, p), PushErr: stk.ErrUnavailable}, nil
	}
	r := e.do("POST", "/v1/charges", tn.Key, chargeBody("P3", 1500, `"phone":"0712345678"`), hdr{"Idempotency-Key": "p3"})
	if m := r.expect(201, ""); m["latest_stk_push"] != nil {
		t.Fatalf("%v", m)
	}

	// Replay without a push from the service: the stored latest push shows.
	f.create = nil
	m = e.do("POST", "/v1/charges", tn.Key, chargeBody("P4", 1500, ""), hdr{"Idempotency-Key": "p4"}).expect(201, "")
	id := uid(t, m)
	later := testPush(tn.ID, id, store.StkPushStatusSucceeded)
	pushes.mu.Lock()
	pushes.latest[id] = later
	pushes.mu.Unlock()
	r = e.do("POST", "/v1/charges", tn.Key, chargeBody("P4", 1500, ""), hdr{"Idempotency-Key": "p4"})
	if m := r.expect(200, ""); m["latest_stk_push"].(map[string]any)["id"] != later.ID.String() {
		t.Fatalf("replay push %v", m)
	}
	e.assertNoLeaks()
}

func TestListCharges(t *testing.T) {
	f := newFakeCharges()
	pushes := &fakePushes{latest: map[uuid.UUID]store.StkPush{}}
	e := newEnv(t, nil, withCharges(f), withPushes(pushes))
	a, b := e.tenant("LSA"), e.tenant("LSB")

	var ids []string
	for i := range 5 {
		m := e.do("POST", "/v1/charges", a.Key, chargeBody(fmt.Sprint("L", i), int64(100+i), ""), hdr{"Idempotency-Key": fmt.Sprint("l", i)}).expect(201, "")
		ids = append(ids, m["id"].(string))
	}
	first, _ := uuid.Parse(ids[4])
	pushes.latest[first] = testPush(a.ID, first, store.StkPushStatusSent)

	// Paging, newest first, no gaps or repeats.
	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		path := "/v1/charges?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		m := e.do("GET", path, a.Key, "", nil).expect(200, "")
		for _, c := range m["data"].([]any) {
			seen = append(seen, c.(map[string]any)["id"].(string))
		}
		if page == 0 {
			if sp := m["data"].([]any)[0].(map[string]any)["latest_stk_push"]; sp == nil {
				t.Fatal("latest push not embedded in the list")
			}
		}
		nc, _ := m["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
		if page > 5 {
			t.Fatal("paging does not end")
		}
	}
	if fmt.Sprint(seen) != fmt.Sprint([]string{ids[4], ids[3], ids[2], ids[1], ids[0]}) {
		t.Fatalf("paging order %v", seen)
	}
	// The service is asked for one more row than the page.
	f.mu.Lock()
	if p := f.lists[0]; p.Limit != 3 || p.CursorAt != nil || p.Status != nil || p.Reference != "" {
		t.Fatalf("first list params %+v", p)
	}
	f.mu.Unlock()

	// Filters reach the service.
	m := e.do("GET", "/v1/charges?status=pending&reference=L2", a.Key, "", nil).expect(200, "")
	if d := m["data"].([]any); len(d) != 1 || d[0].(map[string]any)["reference"] != "L2" || m["next_cursor"] != nil {
		t.Fatalf("filtered %v", m)
	}
	f.mu.Lock()
	if p := f.lists[len(f.lists)-1]; p.Status == nil || *p.Status != store.ChargeStatusPending || p.Reference != "L2" || p.Limit != 51 {
		t.Fatalf("filter params %+v", p)
	}
	f.mu.Unlock()

	// Bad query parameters are 400 validation_failed.
	for q, field := range map[string]string{
		"status=paid": "status", "limit=0": "limit", "limit=201": "limit", "cursor=nope": "cursor",
		"reference=" + strings.Repeat("r", 129): "reference",
	} {
		r := e.do("GET", "/v1/charges?"+q, a.Key, "", nil)
		r.expect(400, "validation_failed")
		if fe := r.fieldErrors(); len(fe) != 1 || fe[0] != field {
			t.Errorf("%s: %v", q, fe)
		}
	}
	// A transactions cursor is not a charges cursor.
	m = e.do("GET", "/v1/charges?limit=1", a.Key, "", nil).expect(200, "")
	e.do("GET", "/v1/transactions?cursor="+url.QueryEscape(m["next_cursor"].(string)), a.Key, "", nil).expect(400, "validation_failed")

	// Another app sees none of them.
	m = e.do("GET", "/v1/charges", b.Key, "", nil).expect(200, "")
	if len(m["data"].([]any)) != 0 || m["next_cursor"] != nil {
		t.Fatalf("B sees %v", m)
	}

	// A push lookup failure is a 500 (never a partial page).
	pushes.err = errors.New("db down")
	e.do("GET", "/v1/charges", a.Key, "", nil).expect(500, "internal_error")
	pushes.err = nil
	f.err = charge.ErrInvalidCursor
	e.do("GET", "/v1/charges", a.Key, "", nil).expect(400, "validation_failed")
	e.assertNoLeaks()
}

func TestGetAndCancelCharge(t *testing.T) {
	f := newFakeCharges()
	pushes := &fakePushes{latest: map[uuid.UUID]store.StkPush{}}
	e := newEnv(t, nil, withCharges(f), withPushes(pushes))
	a, b := e.tenant("GCA"), e.tenant("GCB")

	m := e.do("POST", "/v1/charges", a.Key, chargeBody("G1", 700, ""), hdr{"Idempotency-Key": "g1"}).expect(201, "")
	id := uid(t, m)
	path := "/v1/charges/" + id.String()
	push := testPush(a.ID, id, store.StkPushStatusFailed)
	pushes.latest[id] = push

	g := e.do("GET", path, a.Key, "", nil).expect(200, "")
	if g["id"] != id.String() || g["latest_stk_push"].(map[string]any)["id"] != push.ID.String() {
		t.Fatalf("get %v", g)
	}
	// Other apps, unknown ids and malformed ids: 404.
	e.do("GET", path, b.Key, "", nil).expect(404, "not_found")
	e.do("POST", path+"/cancel", b.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/charges/"+uuid.NewString(), a.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/charges/nope", a.Key, "", nil).expect(404, "not_found")
	e.do("POST", "/v1/charges/nope/cancel", a.Key, "", nil).expect(404, "not_found")
	// Defence in depth: a service that leaked another app's charge is 404.
	f.anyApp = true
	e.do("GET", path, b.Key, "", nil).expect(404, "not_found")
	f.anyApp = false

	// A push lookup failure still serves the charge.
	pushes.err = errors.New("db hiccup")
	if g := e.do("GET", path, a.Key, "", nil).expect(200, ""); g["latest_stk_push"] != nil {
		t.Fatal(g)
	}
	pushes.err = nil

	// Cancel, then cancel again (idempotent): both 200.
	c := e.do("POST", path+"/cancel", a.Key, "", nil).expect(200, "")
	if c["status"] != "cancelled" || c["paybill"] != nil {
		t.Fatalf("cancel %v", c)
	}
	e.do("POST", path+"/cancel", a.Key, "", nil).expect(200, "")

	// Succeeded charges cannot be cancelled.
	m = e.do("POST", "/v1/charges", a.Key, chargeBody("G2", 700, ""), hdr{"Idempotency-Key": "g2"}).expect(201, "")
	paid, _ := f.Get(e.ctx, a.ID, uid(t, m))
	paid.Status = store.ChargeStatusSucceeded
	f.set(paid)
	e.do("POST", "/v1/charges/"+paid.ID.String()+"/cancel", a.Key, "", nil).expect(409, "not_cancellable")
	e.do("GET", "/v1/charges/"+paid.ID.String()+"/cancel", a.Key, "", nil).expect(405, "method_not_allowed")
	e.assertNoLeaks()
}

func TestChargeRoutesWithoutService(t *testing.T) {
	e := newEnv(t, nil)
	tn := e.tenant("NOSV")
	e.do("POST", "/v1/charges", tn.Key, chargeBody("R", 1, ""), hdr{"Idempotency-Key": "k"}).expect(501, "not_implemented")
	e.do("GET", "/v1/charges", tn.Key, "", nil).expect(501, "not_implemented")
	e.do("GET", "/v1/charges/"+uuid.NewString(), tn.Key, "", nil).expect(501, "not_implemented")
	e.do("POST", "/v1/charges/"+uuid.NewString()+"/cancel", tn.Key, "", nil).expect(501, "not_implemented")
}

// TestChargesRealService runs the /v1 charge routes against the real
// charge service and database (no M-Pesa).
func TestChargesRealService(t *testing.T) {
	e := newEnv(t, nil, func(d *appapi.Deps) {
		d.Charges = charge.NewService(charge.Options{Pool: d.Pool, Events: events.NewEmitter(d.View, nil), Logger: d.Logger})
	})
	a, b := e.tenant("REAL"), e.tenant("REAB")
	body := chargeBody("INV-9", 2500, `"description":"Hosting","metadata":{"order":9},"phone":"0712345678"`)
	r := e.do("POST", "/v1/charges", a.Key, body, hdr{"Idempotency-Key": "real-1"})
	m := r.expect(201, "")
	id := uid(t, m)
	acct, _ := m["account_number"].(string)
	if m["status"] != "pending" || !strings.HasPrefix(acct, "REAL-") || m["latest_stk_push"] != nil ||
		!strings.HasPrefix(m["checkout_url"].(string), "https://pay.test/") {
		t.Fatalf("created %v", m)
	}
	if pb := m["paybill"].(map[string]any); pb["business_number"] != "174379" || pb["account_number"] != acct {
		t.Fatalf("paybill %v", pb)
	}
	r = e.do("POST", "/v1/charges", a.Key, body, hdr{"Idempotency-Key": "real-1"})
	if m := r.expect(200, ""); uid(t, m) != id || r.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %s", r.Body.String())
	}
	e.do("POST", "/v1/charges", a.Key, chargeBody("INV-9", 1, ""), hdr{"Idempotency-Key": "real-1"}).expect(409, "idempotency_conflict")
	e.do("POST", "/v1/charges", a.Key, body, hdr{"Idempotency-Key": "real-2"}).expect(409, "duplicate_reference")
	// The same reference is free for another app.
	e.do("POST", "/v1/charges", b.Key, body, hdr{"Idempotency-Key": "real-1"}).expect(201, "")

	e.do("GET", "/v1/charges/"+id.String(), a.Key, "", nil).expect(200, "")
	e.do("GET", "/v1/charges/"+id.String(), b.Key, "", nil).expect(404, "not_found")
	l := e.do("GET", "/v1/charges?reference=INV-9", a.Key, "", nil).expect(200, "")
	if d := l["data"].([]any); len(d) != 1 || d[0].(map[string]any)["id"] != id.String() {
		t.Fatalf("list %v", l)
	}
	e.do("POST", "/v1/charges/"+id.String()+"/cancel", b.Key, "", nil).expect(404, "not_found")
	if c := e.do("POST", "/v1/charges/"+id.String()+"/cancel", a.Key, "", nil).expect(200, ""); c["status"] != "cancelled" {
		t.Fatal(c)
	}
	e.do("POST", "/v1/charges/"+id.String()+"/cancel", a.Key, "", nil).expect(200, "")
	// A reference is unique per app for ever, even after a cancel.
	e.do("POST", "/v1/charges", a.Key, body, hdr{"Idempotency-Key": "real-3"}).expect(409, "duplicate_reference")
	e.assertNoLeaks()
}
