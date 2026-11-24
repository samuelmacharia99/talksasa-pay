// Package webhook delivers events to product webhook URLs: signing, an
// SSRF-safe HTTP client, the River worker with its retry policy, a
// per-product circuit breaker and concurrency limit, and the admin-side
// delivery queries (list, detail, redeliver).
//
// Delivery is at least once. Receivers de-duplicate on Talksasa-Event-Id
// and verify Talksasa-Signature (see api/product.openapi.yaml, `webhooks:`).
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Header names sent with every delivery.
const (
	HeaderEventID   = "Talksasa-Event-Id"
	HeaderEventType = "Talksasa-Event-Type"
	HeaderSignature = "Talksasa-Signature"
	UserAgent       = "TalksasaPay-Webhooks/1"
)

// SignatureTolerance is how far a receiver may let t drift from its clock.
const SignatureTolerance = 5 * time.Minute

// Sign returns the hex v1 signature: HMAC-SHA256(secret, "<t>.<body>").
// secret is the full secret string as handed to the product ("whsec_...").
func Sign(secret []byte, t int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(t, 10)))
	m.Write([]byte{'.'})
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// SignatureHeader builds "t=<t>,v1=<hex>[,v1=<hex>...]", one v1 per non-empty
// secret (current first, then the previous one during a rotation grace).
func SignatureHeader(t int64, body []byte, secrets ...[]byte) string {
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(t, 10))
	for _, s := range secrets {
		if len(s) == 0 {
			continue
		}
		b.WriteString(",v1=")
		b.WriteString(Sign(s, t, body))
	}
	return b.String()
}

// Verify is the receiver-side check and the reference for the SDKs (the
// canonical rules are in pkg/talksasapay/testdata/webhook_signature_vectors.json):
// items are comma-separated and trimmed; an item without "=" (incl. a
// trailing comma) is malformed; exactly one t of 1-12 digits; v1 values that
// are not 64 hex digits are ignored but at least one valid v1 is required;
// unknown keys are ignored; accept when ANY v1 matches (case-insensitive hex)
// and |now - t| <= tolerance.
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) bool {
	var t int64
	var sigs []string
	tCount := 0
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" {
			return false
		}
		switch k {
		case "t":
			if len(v) < 1 || len(v) > 12 || strings.Trim(v, "0123456789") != "" {
				return false
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return false
			}
			t = n
			tCount++
		case "v1":
			if len(v) == 64 && strings.Trim(strings.ToLower(v), "0123456789abcdef") == "" {
				sigs = append(sigs, strings.ToLower(v))
			}
		}
	}
	if tCount != 1 || len(sigs) == 0 {
		return false
	}
	if d := now.Sub(time.Unix(t, 0)); d > tolerance || d < -tolerance {
		return false
	}
	want := []byte(Sign(secret, t, body))
	ok := false
	for _, s := range sigs {
		if hmac.Equal(want, []byte(s)) {
			ok = true // keep looping: same work whichever signature matches
		}
	}
	return ok
}
