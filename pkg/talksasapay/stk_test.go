package talksasapay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

var sampleSTK = map[string]any{
	"object": "stk_push", "id": "0193a5f2-0000-7000-8000-0000000000cc",
	"payment_request_id": "0193a5f2-0000-7000-8000-000000000001", "transaction_id": "0193a5f2-0000-7000-8000-00000000000a",
	"status": "sent", "amount": 1500, "phone": "2547****5678", "checkout_request_id": "ws_CO_1",
	"result_code": nil, "result_desc": nil,
	"created_at": "2026-11-24T10:00:00Z", "updated_at": "2026-11-24T10:00:01Z", "completed_at": nil, "future": 1,
}

func TestSTKPush(t *testing.T) {
	replay := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Idempotent-Replayed", "true")
		jsonResp(200, sampleSTK)(w, r)
	}
	f := newFake(t, jsonResp(202, sampleSTK), replay)
	c, _ := newTestClient(t, f)
	ctx := context.Background()

	push, replayed, err := c.STKPush(ctx, "0193a5f2-0000-7000-8000-000000000001", " 0712345678 ", STKPushOptions{})
	if err != nil || replayed {
		t.Fatalf("replayed=%v err=%v", replayed, err)
	}
	if push.Status != STKSent || !push.Pending() || push.Amount != 1500 || push.Phone != "2547****5678" ||
		push.CheckoutRequestID == nil || *push.CheckoutRequestID != "ws_CO_1" || push.ResultCode != nil || push.CompletedAt != nil {
		t.Fatalf("%+v", push)
	}
	if _, replayed, err = c.STKPush(ctx, "a/b", "+254712345678", STKPushOptions{IdempotencyKey: "invoice-1-stk-1"}); err != nil || !replayed {
		t.Fatalf("replayed=%v err=%v", replayed, err)
	}

	reqs := f.requests()
	checkCommon(t, reqs[0], "POST", "/v1/payment-requests/0193a5f2-0000-7000-8000-000000000001/stk-push")
	if reqs[0].Body != `{"phone":"0712345678"}` {
		t.Fatalf("body %s", reqs[0].Body)
	}
	if k := reqs[0].Header.Get("Idempotency-Key"); !strings.HasPrefix(k, "tspgo_") {
		t.Fatalf("auto key %q", k)
	}
	if reqs[1].RawPath != "/v1/payment-requests/a%2Fb/stk-push" || reqs[1].Header.Get("Idempotency-Key") != "invoice-1-stk-1" {
		t.Fatalf("%s key %q", reqs[1].RawPath, reqs[1].Header.Get("Idempotency-Key"))
	}
}

func TestSTKPushValidatesLocally(t *testing.T) {
	f := newFake(t, jsonResp(202, sampleSTK))
	c, _ := newTestClient(t, f)
	ctx := context.Background()
	cases := []struct{ id, phone, key string }{
		{"", "0712345678", ""},
		{"id", "  ", ""},
		{"id", strings.Repeat("7", 21), ""},
		{"id", "0712345678", strings.Repeat("k", 256)},
	}
	for _, tc := range cases {
		if _, _, err := c.STKPush(ctx, tc.id, tc.phone, STKPushOptions{IdempotencyKey: tc.key}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestSTKPushRetriesWithSameKey(t *testing.T) {
	f := newFake(t, dropConn, problem(503, CodeTimeout, nil), problem(429, CodeRateLimited, map[string]string{"Retry-After": "1"}), jsonResp(202, sampleSTK))
	c, _ := newTestClient(t, f)
	if _, _, err := c.STKPush(context.Background(), "id", "0712345678", STKPushOptions{}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, r := range reqs[1:] {
		if r.Header.Get("Idempotency-Key") != reqs[0].Header.Get("Idempotency-Key") || r.Body != reqs[0].Body {
			t.Fatalf("retry changed key or body: %q %s", r.Header.Get("Idempotency-Key"), r.Body)
		}
	}
}

func TestSTKPushFinalErrorsAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{503, CodeMpesaUnavailable},
		{502, CodeMpesaError},
		{409, CodeSTKInProgress},
		{409, CodeNotPayable},
		{409, CodeAmountTooLarge},
		{422, CodeValidationFailed},
	} {
		f := newFake(t, problem(tc.status, tc.code, nil), jsonResp(202, sampleSTK))
		c, slept := newTestClient(t, f)
		_, _, err := c.STKPush(context.Background(), "id", "0712345678", STKPushOptions{})
		if !IsCode(err, tc.code) || StatusCode(err) != tc.status {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if len(f.requests()) != 1 || len(*slept) != 0 {
			t.Fatalf("%s: retried (%d requests)", tc.code, len(f.requests()))
		}
	}
	if (&APIError{Status: 503, Code: CodeMpesaUnavailable}).Temporary() {
		t.Fatal("mpesa_unavailable reported as temporary")
	}
}

func TestGetSTKPushAndEvents(t *testing.T) {
	failed := map[string]any{}
	for k, v := range sampleSTK {
		failed[k] = v
	}
	failed["status"], failed["result_code"], failed["result_desc"], failed["completed_at"] = "failed", "1032", "Request cancelled by user", "2026-11-24T10:01:00Z"
	f := newFake(t, jsonResp(200, failed))
	c, _ := newTestClient(t, f)
	p, err := c.GetSTKPush(context.Background(), "0193a5f2-0000-7000-8000-0000000000cc")
	if err != nil {
		t.Fatal(err)
	}
	checkCommon(t, f.requests()[0], "GET", "/v1/stk-pushes/0193a5f2-0000-7000-8000-0000000000cc")
	if p.Status != STKFailed || p.Pending() || *p.ResultCode != "1032" || p.CompletedAt == nil {
		t.Fatalf("%+v", p)
	}
	if _, err := c.GetSTKPush(context.Background(), ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}

	data, _ := json.Marshal(failed)
	for _, typ := range []string{EventSTKPushSent, EventSTKPushSucceeded, EventSTKPushFailed, EventSTKPushNeedsReview} {
		ev := Event{ID: "e", Type: typ, Data: data}
		got, err := ev.STKPush()
		if err != nil || got.ResultDesc == nil || *got.ResultDesc != "Request cancelled by user" {
			t.Fatalf("%s: %v %+v", typ, err, got)
		}
		if _, err := ev.PaymentRequest(); err == nil {
			t.Fatalf("%s decoded as a payment request", typ)
		}
	}
	if _, err := (Event{ID: "e", Type: EventPaymentRequestPaid, Data: data}).STKPush(); err == nil {
		t.Fatal("payment_request.paid decoded as an STK push")
	}
}
