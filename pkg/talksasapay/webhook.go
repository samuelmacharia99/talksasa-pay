package talksasapay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Webhook headers.
const (
	HeaderSignature = "Talksasa-Signature"
	HeaderEventID   = "Talksasa-Event-Id"
	HeaderEventType = "Talksasa-Event-Type"
)

// DefaultWebhookTolerance is the maximum accepted |now - t|.
const DefaultWebhookTolerance = 300 * time.Second

// MaxWebhookBody is the largest body WebhookHandler reads.
const MaxWebhookBody = 64 << 10

// Webhook verification errors (wrapped; test with errors.Is).
var (
	ErrWebhookNoSignature        = errors.New("talksasapay: webhook has no Talksasa-Signature header")
	ErrWebhookMalformedSignature = errors.New("talksasapay: malformed Talksasa-Signature header")
	ErrWebhookTimestamp          = errors.New("talksasapay: webhook timestamp outside tolerance")
	ErrWebhookSignatureMismatch  = errors.New("talksasapay: webhook signature does not match")
	ErrWebhookBadPayload         = errors.New("talksasapay: webhook body is not a valid event")
)

// SignWebhook returns the Talksasa-Signature header value for body signed at
// t with each secret (one v1 per secret, in order). Senders and tests use it;
// receivers use VerifyWebhook.
func SignWebhook(t time.Time, body []byte, secrets ...[]byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	parts := []string{"t=" + ts}
	for _, s := range secrets {
		parts = append(parts, "v1="+hex.EncodeToString(webhookMAC(s, ts, body)))
	}
	return strings.Join(parts, ",")
}

func webhookMAC(secret []byte, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return m.Sum(nil)
}

// VerifyWebhookSignature checks only the signature header against body.
// tolerance <= 0 means DefaultWebhookTolerance.
func VerifyWebhookSignature(secret []byte, sigHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	if len(secret) == 0 {
		return errors.New("talksasapay: empty webhook secret")
	}
	if tolerance <= 0 {
		tolerance = DefaultWebhookTolerance
	}
	sigHeader = strings.TrimSpace(sigHeader)
	if sigHeader == "" {
		return ErrWebhookNoSignature
	}
	// Parsing rules are shared with the other SDKs; see
	// testdata/webhook_signature_vectors.json ("algorithm").
	var ts string
	seenT := false
	var sigs [][]byte
	for _, part := range strings.Split(sigHeader, ",") {
		k, v, ok := strings.Cut(strings.Trim(part, " \t"), "=")
		if !ok || k == "" {
			return fmt.Errorf("%w: element without key=value", ErrWebhookMalformedSignature)
		}
		switch k {
		case "t":
			if seenT {
				return fmt.Errorf("%w: repeated t", ErrWebhookMalformedSignature)
			}
			seenT, ts = true, v
		case "v1":
			// A v1 that is not 64 hex digits is ignored (a future format).
			if b, err := hex.DecodeString(v); err == nil && len(b) == sha256.Size {
				sigs = append(sigs, b)
			}
		default:
			// Unknown schemes (a future v2) are ignored.
		}
	}
	if !seenT {
		return fmt.Errorf("%w: missing t", ErrWebhookMalformedSignature)
	}
	if !allDigits(ts) || len(ts) > 12 {
		return fmt.Errorf("%w: t is not unix seconds", ErrWebhookMalformedSignature)
	}
	if len(sigs) == 0 {
		return fmt.Errorf("%w: no well-formed v1", ErrWebhookMalformedSignature)
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: t is not unix seconds", ErrWebhookMalformedSignature)
	}
	if d := now.Sub(time.Unix(secs, 0)); d > tolerance || d < -tolerance {
		return ErrWebhookTimestamp
	}
	want := webhookMAC(secret, ts, body)
	ok := false
	for _, s := range sigs {
		if hmac.Equal(s, want) { // constant time; check all so timing doesn't reveal which matched
			ok = true
		}
	}
	if !ok {
		return ErrWebhookSignatureMismatch
	}
	return nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// VerifyWebhook verifies a webhook delivery and decodes its event.
// tolerance <= 0 means DefaultWebhookTolerance (300 s). The body must be the
// raw bytes received, before any JSON re-encoding.
func VerifyWebhook(secret []byte, header http.Header, body []byte, now time.Time, tolerance time.Duration) (Event, error) {
	if err := VerifyWebhookSignature(secret, header.Get(HeaderSignature), body, now, tolerance); err != nil {
		return Event{}, err
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrWebhookBadPayload, err)
	}
	if ev.ID == "" || ev.Type == "" {
		return Event{}, fmt.Errorf("%w: missing id or type", ErrWebhookBadPayload)
	}
	if h := header.Get(HeaderEventID); h != "" && h != ev.ID {
		return Event{}, fmt.Errorf("%w: Talksasa-Event-Id header does not match body", ErrWebhookBadPayload)
	}
	return ev, nil
}

// WebhookHandlerOption configures WebhookHandler.
type WebhookHandlerOption func(*webhookHandler)

// WithWebhookTolerance overrides the 300 s timestamp tolerance.
func WithWebhookTolerance(d time.Duration) WebhookHandlerOption {
	return func(h *webhookHandler) { h.tolerance = d }
}

// WithWebhookClock overrides time.Now (tests).
func WithWebhookClock(now func() time.Time) WebhookHandlerOption {
	return func(h *webhookHandler) { h.now = now }
}

// WithWebhookErrorHook is told why a delivery was rejected (never the secret).
func WithWebhookErrorHook(f func(r *http.Request, err error)) WebhookHandlerOption {
	return func(h *webhookHandler) { h.onError = f }
}

type webhookHandler struct {
	secret    []byte
	fn        func(context.Context, Event) error
	tolerance time.Duration
	now       func() time.Time
	onError   func(*http.Request, error)
}

// WebhookHandler returns an http.Handler that accepts POSTed webhooks, reads
// at most 64 KB, verifies the signature and calls fn. Responses: 200 when fn
// returns nil; 400 for a bad signature/body (not retried usefully, but
// harmless); 413 for oversized bodies; 500 when fn fails, so Talksasa Pay
// retries the delivery. fn may be called more than once for the same event
// id; de-duplicate. It should return quickly (deliveries time out after 10 s).
func WebhookHandler(secret []byte, fn func(ctx context.Context, ev Event) error, opts ...WebhookHandlerOption) http.Handler {
	h := &webhookHandler{secret: secret, fn: fn, now: time.Now}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *webhookHandler) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	if h.onError != nil {
		h.onError(r, err)
	}
	http.Error(w, http.StatusText(status), status)
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, r, http.StatusMethodNotAllowed, errors.New("talksasapay: webhook must be POST"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.fail(w, r, http.StatusRequestEntityTooLarge, err)
			return
		}
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	ev, err := VerifyWebhook(h.secret, r.Header, body, h.now(), h.tolerance)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.fn(r.Context(), ev); err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
