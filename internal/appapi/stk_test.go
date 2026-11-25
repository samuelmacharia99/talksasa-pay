package appapi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talksasa/talksasa-pay/internal/appapi"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// fakeSTK stands in for *stk.Service.
type fakeSTK struct {
	mu        sync.Mutex
	available bool
	calls     []sendCall
	// result decides Send's outcome; nil = success with a new sent push.
	result func(p stk.SendParams) (stk.Push, bool, error)
	pushes map[uuid.UUID]stk.Push
	// getAny makes Get return pushes of any app (to test the handler's
	// own app check).
	getAny bool
}

type sendCall struct {
	actor audit.Actor
	p     stk.SendParams
}

func newFakeSTK() *fakeSTK { return &fakeSTK{available: true, pushes: map[uuid.UUID]stk.Push{}} }

func (f *fakeSTK) Available() bool { return f.available }

func (f *fakeSTK) Send(_ context.Context, actor audit.Actor, p stk.SendParams) (stk.Push, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sendCall{actor, p})
	if f.result != nil {
		return f.result(p)
	}
	push := testPush(p.AppID, p.ChargeID, store.StkPushStatusSent)
	f.pushes[push.ID] = push
	return push, false, nil
}

func (f *fakeSTK) Get(_ context.Context, appID, id uuid.UUID) (stk.Push, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pushes[id]
	if !ok || (!f.getAny && p.AppID != appID) {
		return stk.Push{}, stk.ErrNotFound
	}
	return p, nil
}

func (f *fakeSTK) sent() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.calls...)
}

func testPush(appID, chargeID uuid.UUID, status store.StkPushStatus) stk.Push {
	now := time.Now().UTC()
	co := "ws_CO_" + uuid.NewString()[:8]
	return stk.Push{
		ID: uuid.New(), AppID: appID, ChargeID: chargeID, TransactionID: uuid.New(),
		Source: "app", Phone: fullPhone, Amount: 1500, Status: status, CheckoutRequestID: &co,
		CreatedAt: now, UpdatedAt: now,
	}
}

func withSTK(s appapi.STKService) func(*appapi.Deps) {
	return func(d *appapi.Deps) { d.STK = s }
}

const phoneBody = `{"phone":"0712345678"}`

func stkPath(id uuid.UUID) string { return "/v1/charges/" + id.String() + "/stk-push" }

func TestSTKPushUnavailable(t *testing.T) {
	e := newEnv(t, nil) // STK nil: M-Pesa off
	tn := e.tenant("SOFF")
	e.do("POST", stkPath(uuid.New()), tn.Key, phoneBody, hdr{"Idempotency-Key": "k1"}).expect(503, "mpesa_unavailable")
	e.do("GET", "/v1/stk-pushes/"+uuid.NewString(), tn.Key, "", nil).expect(404, "not_found")

	f := newFakeSTK()
	f.available = false
	e2 := newEnv(t, nil, withSTK(f))
	tn2 := e2.tenant("SOFB")
	e2.do("POST", stkPath(uuid.New()), tn2.Key, phoneBody, hdr{"Idempotency-Key": "k1"}).expect(503, "mpesa_unavailable")
	if len(f.sent()) != 0 {
		t.Fatal("sent while unavailable")
	}
	// Unauthenticated callers learn nothing.
	e2.do("POST", stkPath(uuid.New()), "", phoneBody, hdr{"Idempotency-Key": "k1"}).expect(401, "unauthorized")
}

func TestSTKPushValidation(t *testing.T) {
	f := newFakeSTK()
	e := newEnv(t, nil, withSTK(f))
	tn := e.tenant("SVAL")
	id := uuid.New()
	e.do("POST", stkPath(id), tn.Key, phoneBody, nil).expect(400, "idempotency_key_required")
	r := e.do("POST", stkPath(id), tn.Key, phoneBody, hdr{"Idempotency-Key": "bad\x01key"})
	r.expect(400, "validation_failed")
	if fe := r.fieldErrors(); len(fe) != 1 || fe[0] != "Idempotency-Key" {
		t.Fatal(fe)
	}
	e.do("POST", stkPath(id), tn.Key, phoneBody, hdr{"Idempotency-Key": strings.Repeat("k", 256)}).expect(400, "validation_failed")
	e.do("POST", "/v1/charges/not-a-uuid/stk-push", tn.Key, phoneBody, hdr{"Idempotency-Key": "k"}).expect(404, "not_found")
	e.do("POST", stkPath(id), tn.Key, phoneBody, hdr{"Idempotency-Key": "k", "Content-Type": "text/plain"}).expect(415, "unsupported_media_type")
	e.do("POST", stkPath(id), tn.Key, `{"phone":`, hdr{"Idempotency-Key": "k"}).expect(400, "invalid_json")
	for body, field := range map[string]string{
		`{}`:                                  "phone",
		`{"phone":null}`:                      "phone",
		`{"phone":712345678}`:                 "phone",
		`{"phone":"0812345678"}`:              "phone",
		`{"phone":"07123456789012345678901"}`: "phone",
		`{"phone":"0712345678","amount":5}`:   "amount",
	} {
		r := e.do("POST", stkPath(id), tn.Key, body, hdr{"Idempotency-Key": "k"})
		r.expect(422, "validation_failed")
		if fe := r.fieldErrors(); len(fe) != 1 || fe[0] != field {
			t.Errorf("%s: field errors %v", body, fe)
		}
	}
	if len(f.sent()) != 0 {
		t.Fatalf("invalid requests reached the service: %d", len(f.sent()))
	}
	e.assertNoLeaks()
}

func TestSTKPushSendAndReplay(t *testing.T) {
	f := newFakeSTK()
	e := newEnv(t, nil, withSTK(f))
	tn := e.tenant("SSND")
	reqID := uuid.New()
	key := "stk-idem-1"
	r := e.do("POST", stkPath(reqID), tn.Key, `{"phone":"+254 712 345 678"}`, hdr{"Idempotency-Key": key})
	m := r.expect(202, "")
	if m["object"] != "stk_push" || m["status"] != "sent" || m["charge_id"] != reqID.String() || m["phone"] != "2547*****678" {
		t.Fatalf("%s", r.Body.String())
	}
	if r.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("first send marked replayed")
	}
	calls := f.sent()
	if len(calls) != 1 {
		t.Fatal(calls)
	}
	c := calls[0]
	want := stk.SendParams{AppID: tn.ID, ChargeID: reqID, Phone: "+254 712 345 678", Source: stk.SourceApp, IdempotencyKey: key}
	if c.p != want || c.actor != audit.App(tn.ID) {
		t.Fatalf("send params %+v actor %+v", c.p, c.actor)
	}

	// Replay: 200 + Idempotent-Replayed, the same push.
	first := uid(t, m)
	f.result = func(stk.SendParams) (stk.Push, bool, error) { return f.pushes[first], true, nil }
	r = e.do("POST", stkPath(reqID), tn.Key, phoneBody, hdr{"Idempotency-Key": key})
	if m := r.expect(200, ""); uid(t, m) != first || r.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %s %v", r.Body.String(), r.Header())
	}

	// GET the push; another app cannot see it.
	g := e.do("GET", "/v1/stk-pushes/"+first.String(), tn.Key, "", nil).expect(200, "")
	if g["id"] != first.String() || g["phone"] != "2547*****678" {
		t.Fatal(g)
	}
	other := e.tenant("SOTH")
	e.do("GET", "/v1/stk-pushes/"+first.String(), other.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/stk-pushes/"+uuid.NewString(), tn.Key, "", nil).expect(404, "not_found")
	e.do("GET", "/v1/stk-pushes/nope", tn.Key, "", nil).expect(404, "not_found")
	// Defence in depth: even if the service returned another app's
	// push, the handler would not show it.
	f.getAny = true
	e.do("GET", "/v1/stk-pushes/"+first.String(), other.Key, "", nil).expect(404, "not_found")
	e.assertNoLeaks()
}

func TestSTKPushErrorMapping(t *testing.T) {
	f := newFakeSTK()
	e := newEnv(t, nil, withSTK(f))
	tn := e.tenant("SERR")
	live := uuid.New()
	failedPush := testPush(tn.ID, uuid.New(), store.StkPushStatusFailed)
	cases := []struct {
		name   string
		push   stk.Push
		err    error
		status int
		code   string
		detail string
		field  string
	}{
		{"unavailable", stk.Push{}, stk.ErrUnavailable, 503, "mpesa_unavailable", "", ""},
		{"not found", stk.Push{}, stk.ErrNotFound, 404, "not_found", "", ""},
		{"not payable", stk.Push{}, stk.ErrNotPayable, 409, "not_payable", "", ""},
		{"too large", stk.Push{}, stk.ErrAmountTooLarge, 409, "amount_too_large", "250,000", ""},
		{"in progress", stk.Push{}, fmt.Errorf("wrapped: %w", &stk.InProgressError{LiveID: live}), 409, "stk_in_progress", live.String(), ""},
		{"idem conflict", stk.Push{}, stk.ErrIdempotencyConflict, 409, "idempotency_conflict", "", ""},
		{"idem invalid", stk.Push{}, stk.ErrInvalidIdempotency, 400, "validation_failed", "", "Idempotency-Key"},
		{"phone", stk.Push{}, phone.ErrInvalid, 422, "validation_failed", "", "phone"},
		{"daraja", failedPush, stk.ErrDaraja, 502, "mpesa_error", failedPush.ID.String(), ""},
		{"daraja no push", stk.Push{}, stk.ErrDaraja, 502, "mpesa_error", "nothing was charged", ""},
		{"busy", stk.Push{}, &pgconn.PgError{Code: "55P03"}, 503, "busy", "", ""},
		{"internal", stk.Push{}, errors.New("db exploded: secret detail"), 500, "internal_error", "", ""},
	}
	for _, tc := range cases {
		f.result = func(stk.SendParams) (stk.Push, bool, error) { return tc.push, false, tc.err }
		r := e.do("POST", stkPath(uuid.New()), tn.Key, phoneBody, hdr{"Idempotency-Key": "k-" + tc.name})
		if r.Code != tc.status {
			t.Errorf("%s: status %d: %s", tc.name, r.Code, r.Body.String())
			continue
		}
		m := r.json()
		if m["code"] != tc.code {
			t.Errorf("%s: code %v", tc.name, m["code"])
		}
		if tc.detail != "" && !strings.Contains(fmt.Sprint(m["detail"]), tc.detail) {
			t.Errorf("%s: detail %v lacks %q", tc.name, m["detail"], tc.detail)
		}
		if tc.field != "" {
			if fe := r.fieldErrors(); len(fe) != 1 || fe[0] != tc.field {
				t.Errorf("%s: fields %v", tc.name, fe)
			}
		}
		if strings.Contains(r.Body.String(), "secret detail") {
			t.Errorf("%s: internal error leaked", tc.name)
		}
		if tc.code == "busy" && r.Header().Get("Retry-After") == "" {
			t.Errorf("busy without Retry-After")
		}
	}
	e.assertNoLeaks()
}
