package talksasapay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type vectorFile struct {
	Signing struct {
		Secret        string `json:"secret"`
		Timestamp     int64  `json:"timestamp"`
		Payload       string `json:"payload"`
		SignedPayload string `json:"signed_payload"`
		V1            string `json:"v1"`
		Header        string `json:"header"`
	} `json:"signing_example"`
	Rotation struct {
		Secrets   []string `json:"secrets"`
		Timestamp int64    `json:"timestamp"`
		Payload   string   `json:"payload"`
		Header    string   `json:"header"`
	} `json:"rotation_signing_example"`
	Vectors []struct {
		Name      string   `json:"name"`
		Secrets   []string `json:"secrets"`
		Payload   string   `json:"payload"`
		Header    string   `json:"header"`
		Now       int64    `json:"now"`
		Tolerance int64    `json:"tolerance"`
		Expected  string   `json:"expected"`
	} `json:"vectors"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/webhook_signature_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if len(vf.Vectors) < 10 || vf.Signing.Header == "" || len(vf.Rotation.Secrets) != 2 {
		t.Fatal("incomplete vector file")
	}
	return vf
}

func TestWebhookSignatureVectors(t *testing.T) {
	vf := loadVectors(t)
	s := vf.Signing
	if s.SignedPayload != fmt.Sprintf("%d.%s", s.Timestamp, s.Payload) {
		t.Fatal("signed_payload is not <t>.<payload>")
	}
	if got := SignWebhook(time.Unix(s.Timestamp, 0), []byte(s.Payload), []byte(s.Secret)); got != s.Header || !strings.HasSuffix(got, "v1="+s.V1) {
		t.Errorf("SignWebhook = %s, want %s", got, s.Header)
	}
	r := vf.Rotation
	if got := SignWebhook(time.Unix(r.Timestamp, 0), []byte(r.Payload), []byte(r.Secrets[0]), []byte(r.Secrets[1])); got != r.Header {
		t.Errorf("rotation SignWebhook = %s, want %s", got, r.Header)
	}

	want := map[string]error{
		"missing_header": ErrWebhookNoSignature, "malformed_header": ErrWebhookMalformedSignature,
		"timestamp_out_of_tolerance": ErrWebhookTimestamp, "no_matching_signature": ErrWebhookSignatureMismatch,
	}
	for _, v := range vf.Vectors {
		// A receiver holding several secrets accepts if any one verifies.
		var err error
		for _, sec := range v.Secrets {
			if err = VerifyWebhookSignature([]byte(sec), v.Header, []byte(v.Payload), time.Unix(v.Now, 0), time.Duration(v.Tolerance)*time.Second); err == nil {
				break
			}
		}
		if v.Expected == "valid" {
			if err != nil {
				t.Errorf("%s: %v", v.Name, err)
			}
			continue
		}
		if w, ok := want[v.Expected]; !ok || !errors.Is(err, w) {
			t.Errorf("%s: got %v, want %s", v.Name, err, v.Expected)
		}
	}
}

func TestVerifyWebhook(t *testing.T) {
	secret := []byte("whsec_unit")
	now := time.Unix(1764000000, 0)
	body := []byte(`{"object":"event","id":"ev-1","type":"transaction.reversed","api_version":"2026-11-25","created_at":"2026-11-24T10:00:00Z","data":{"object":"transaction","id":"tx","kind":"reversal","amount":2000,"reverses_transaction_id":"tx0","created_at":"2026-11-24T10:00:00Z"},"new_top_level":1}`)
	hdr := func(sig, id string) http.Header {
		h := http.Header{}
		if sig != "" {
			h.Set(HeaderSignature, sig)
		}
		if id != "" {
			h.Set(HeaderEventID, id)
		}
		return h
	}
	good := SignWebhook(now, body, secret)
	ev, err := VerifyWebhook(secret, hdr(good, "ev-1"), body, now.Add(-299*time.Second), 0)
	if err != nil || ev.ID != "ev-1" || ev.Type != EventTransactionReversed {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
	tx, err := ev.Transaction()
	if err != nil || tx.Amount != 2000 || tx.Kind != KindReversal || *tx.ReversesTransactionID != "tx0" {
		t.Fatalf("tx=%+v err=%v", tx, err)
	}
	cases := []struct {
		name   string
		secret []byte
		h      http.Header
		body   []byte
		now    time.Time
		tol    time.Duration
		want   error
	}{
		{"missing header", secret, hdr("", ""), body, now, 0, ErrWebhookNoSignature},
		{"event id header mismatch", secret, hdr(good, "ev-2"), body, now, 0, ErrWebhookBadPayload},
		{"custom tolerance", secret, hdr(good, ""), body, now.Add(11 * time.Second), 10 * time.Second, ErrWebhookTimestamp},
		{"repeated t", secret, hdr(good+",t=1764000000", ""), body, now, 0, ErrWebhookMalformedSignature},
		{"element without =", secret, hdr(good+",junk", ""), body, now, 0, ErrWebhookMalformedSignature},
		{"short v1", secret, hdr("t=1764000000,v1=abcd", ""), body, now, 0, ErrWebhookMalformedSignature},
		{"negative t", secret, hdr("t=-5,v1="+strings.Repeat("ab", 32), ""), body, now, 0, ErrWebhookMalformedSignature},
		{"signed non-event", secret, hdr(SignWebhook(now, []byte(`[1]`), secret), ""), []byte(`[1]`), now, 0, ErrWebhookBadPayload},
		{"signed event without id", secret, hdr(SignWebhook(now, []byte(`{"type":"x"}`), secret), ""), []byte(`{"type":"x"}`), now, 0, ErrWebhookBadPayload},
	}
	for _, c := range cases {
		if _, err := VerifyWebhook(c.secret, c.h, c.body, c.now, c.tol); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
	if _, err := VerifyWebhook(nil, hdr(good, ""), body, now, 0); err == nil {
		t.Error("empty secret accepted")
	}
	// Any one of several v1 values may match (rotation).
	rot := SignWebhook(now, body, []byte("other"), secret)
	if _, err := VerifyWebhook(secret, hdr(rot, ""), body, now, 0); err != nil {
		t.Errorf("rotation: %v", err)
	}
}

func TestWebhookHandler(t *testing.T) {
	secret := []byte("whsec_handler")
	now := time.Unix(1764000000, 0)
	body := []byte(`{"object":"event","id":"ev-9","type":"payment.received","api_version":"2026-11-25","created_at":"2026-11-24T10:00:00Z","data":{"object":"transaction","id":"t1","kind":"payment","direction":"in","channel":"mpesa_c2b","status":"succeeded","amount":500,"currency":"KES","receipt":"TK1","phone":null,"account_number":"HOST-1","charge_id":null,"reverses_transaction_id":null,"occurred_at":null,"created_at":"2026-11-24T10:00:00Z","completed_at":null,"duplicate_of_charge":null}}`)
	var got []Event
	fail := false
	var rejections []error
	h := WebhookHandler(secret, func(_ context.Context, ev Event) error {
		if fail {
			return errors.New("busy")
		}
		got = append(got, ev)
		return nil
	}, WithWebhookClock(func() time.Time { return now }), WithWebhookErrorHook(func(_ *http.Request, err error) { rejections = append(rejections, err) }))

	send := func(method string, b []byte, sig string) int {
		r := httptest.NewRequest(method, "/hook", strings.NewReader(string(b)))
		if sig != "" {
			r.Header.Set(HeaderSignature, sig)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := send("POST", body, SignWebhook(now, body, secret)); code != 200 {
		t.Fatalf("valid: %d", code)
	}
	pr, err := got[0].PaymentReceived()
	if len(got) != 1 || err != nil || pr.Amount != 500 || *pr.AccountNumber != "HOST-1" || pr.DuplicateOfCharge != nil || pr.ChargeID != nil {
		t.Fatalf("got %+v %+v %v", got, pr, err)
	}
	if code := send("POST", body, SignWebhook(now, body, []byte("wrong"))); code != 400 {
		t.Fatalf("bad sig: %d", code)
	}
	if code := send("GET", nil, ""); code != 405 {
		t.Fatalf("GET: %d", code)
	}
	big := make([]byte, MaxWebhookBody+1)
	if code := send("POST", big, SignWebhook(now, big, secret)); code != 413 {
		t.Fatalf("oversized: %d", code)
	}
	fail = true
	if code := send("POST", body, SignWebhook(now, body, secret)); code != 500 {
		t.Fatalf("handler failure: %d", code)
	}
	if len(rejections) != 4 {
		t.Fatalf("rejections %v", rejections)
	}
	for _, e := range rejections {
		if strings.Contains(e.Error(), string(secret)) {
			t.Fatal("secret in error")
		}
	}
	// Default tolerance uses the real clock.
	h2 := WebhookHandler(secret, func(context.Context, Event) error { return nil })
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
	r.Header.Set(HeaderSignature, SignWebhook(time.Now().Add(-10*time.Minute), body, secret))
	w := httptest.NewRecorder()
	h2.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("stale: %d", w.Code)
	}
}
