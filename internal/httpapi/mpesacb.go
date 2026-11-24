package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// MaxCallbackBody bounds a Daraja callback body.
const MaxCallbackBody = 16 << 10

// callbackStoreTimeout bounds storing one callback (Safaricom waits on us).
const callbackStoreTimeout = 10 * time.Second

// callbackAck is what Daraja expects once a callback is safely stored.
var callbackAck = []byte(`{"ResultCode":0,"ResultDesc":"Accepted"}` + "\n")

// callbacks serves /cb/mpesa/{token}/... for Safaricom.
//
// Daraja does not sign callbacks, so two checks tell a genuine one from a
// forgery: the secret token in the path (compared in constant time) and the
// source IP (Safaricom's published networks). Any failure looks exactly like
// an unknown path (404) and raises an alarm log; the token itself is never
// logged (the access log shows /cb/mpesa/[redacted]/...).
//
// A callback is stored verbatim (with its processing job) BEFORE it is
// acknowledged; processing happens in the worker. If storing fails we
// answer 500 so Safaricom retries.
type callbacks struct {
	stk     STKService
	logger  *slog.Logger
	limiter *httpx.RateLimiter
	cidrs   []netip.Prefix
	// want is SHA-256 of the configured token; nil rejects everything.
	want []byte
}

func newCallbacks(d Deps) *callbacks {
	c := &callbacks{stk: d.STK, logger: d.Logger, limiter: d.MpesaCallbackLimiter, cidrs: d.MpesaCallbackCIDRs}
	if tok := d.MpesaCallbackToken.Reveal(); tok != "" {
		sum := sha256.Sum256([]byte(tok))
		c.want = sum[:]
	} else {
		d.Logger.Error("M-Pesa is on but no callback token is configured: every callback will be rejected", slog.Bool("alarm", true))
	}
	return c
}

func (c *callbacks) routes(r chi.Router) {
	// Order: cheap per-IP limit, then token + IP. Everything after verify,
	// including unknown sub-paths and wrong methods, needs a valid token.
	r.Use(httpx.LimitByIP(c.limiter, "mpesa-cb"))
	r.Use(c.verify)
	r.NotFound(notFound)
	r.MethodNotAllowed(notFound)
	r.Post("/stk", c.store(store.MpesaCallbackKindStk))
	r.Post("/c2b/confirmation", c.store(store.MpesaCallbackKindC2bConfirmation))
	r.Post("/c2b/validation", c.validation)
}

// callbackKind names the endpoint for logs (never from the token segment).
func callbackKind(path string) string {
	switch {
	case strings.HasSuffix(path, "/stk"):
		return "stk"
	case strings.HasSuffix(path, "/c2b/confirmation"):
		return "c2b_confirmation"
	case strings.HasSuffix(path, "/c2b/validation"):
		return "c2b_validation"
	}
	return "unknown"
}

func (c *callbacks) tokenOK(got string) bool {
	if c.want == nil {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(sum[:], c.want) == 1
}

func (c *callbacks) verify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := httpx.ClientIPFrom(r.Context())
		reason := ""
		switch {
		case !c.tokenOK(chi.URLParam(r, "token")):
			reason = "bad_token"
		case len(c.cidrs) > 0 && !httpx.InPrefixes(ip, c.cidrs):
			reason = "ip_not_allowed"
		}
		if reason != "" {
			remote := "unknown"
			if ip.IsValid() {
				remote = ip.String()
			}
			c.logger.WarnContext(r.Context(), "mpesa callback rejected", slog.Bool("alarm", true),
				slog.String("reason", reason), slog.String("kind", callbackKind(r.URL.Path)), slog.String("remote_ip", remote))
			notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ack(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(callbackAck)
}

// readCallback reads a body of at most MaxCallbackBody bytes that must be a
// JSON object. It writes the problem and returns false otherwise.
func readCallback(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	tooLarge := func() ([]byte, bool) {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusRequestEntityTooLarge, "payload_too_large", "Callback body must be at most 16 KiB."))
		return nil, false
	}
	if r.ContentLength > MaxCallbackBody {
		return tooLarge()
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxCallbackBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return tooLarge()
	}
	if err != nil {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "invalid_json", "Could not read the callback body."))
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "invalid_json", "Callback body must be a JSON object."))
		return nil, false
	}
	return raw, true
}

func (c *callbacks) store(kind store.MpesaCallbackKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := readCallback(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), callbackStoreTimeout)
		defer cancel()
		id, err := c.stk.StoreCallback(ctx, kind, httpx.ClientIPFrom(r.Context()), raw)
		if err != nil {
			// 500 so Safaricom retries; nothing was acknowledged.
			c.logger.ErrorContext(r.Context(), "mpesa callback not stored", slog.Bool("alarm", true),
				slog.String("kind", string(kind)), slog.String("error", err.Error()))
			httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusInternalServerError, "internal_error", "The callback could not be stored. Retry."))
			return
		}
		c.logger.InfoContext(r.Context(), "mpesa callback stored", slog.String("kind", string(kind)), slog.String("callback_id", id.String()))
		ack(w)
	}
}

// validation: Talksasa never rejects a C2B payment (unknown accounts go to
// suspense), so external validation always accepts and stores nothing.
func (c *callbacks) validation(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, MaxCallbackBody))
	ack(w)
}
