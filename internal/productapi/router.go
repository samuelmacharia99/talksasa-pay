// Package productapi is the product integration API (/v1), the contract in
// api/product.openapi.yaml. Talksasa products (Cloud Hosting, ISP Billing,
// ...) use it to register payers, request money and read what was paid.
//
// Every request is scoped to the product of the authenticated API key: the
// product id is NEVER taken from the request. Another product's ids are
// simply "not found" (404), never 403, so ids cannot be probed.
//
// Products can request and read money; nothing here records, moves or
// refunds it.
//
// The router expects to be mounted under a parent that already runs
// httpx.RequestID, httpx.ClientIP, httpx.AccessLog, httpx.Recover and
// httpx.SecurityHeaders (internal/httpapi does): it does not repeat them, so
// the parent's trusted-proxy client IP is the one the allow-list checks.
package productapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/product"
)

// MaxBodyBytes bounds every /v1 request body.
const MaxBodyBytes = 64 << 10

// Defaults for optional Deps.
const (
	DefaultKeyRatePerMinute = 1200
	DefaultKeyBurst         = 200
	DefaultRequestTimeout   = 15 * time.Second
)

// Deps are the services the product API needs.
type Deps struct {
	Logger   *slog.Logger
	Pool     *db.Pool
	Products *product.Service
	Payers   *payer.Service
	Requests *paymentrequest.Service
	View     apiview.View
	// STK sends M-Pesa prompts. nil (M-Pesa off): sending is 503
	// mpesa_unavailable and GET /stk-pushes/{id} is 404.
	STK STKService
	// KeyLimiter is a token bucket per API key (keyed by key id). nil uses
	// DefaultKeyRatePerMinute / DefaultKeyBurst.
	KeyLimiter *httpx.RateLimiter
	// RequestTimeout bounds each request's database work. 0 = default.
	RequestTimeout time.Duration
}

// Route is one /v1 route (exported for the contract test).
type Route struct {
	Method  string
	Pattern string
	handler http.HandlerFunc
}

type handlers struct {
	d Deps
}

// Routes lists every route, relative to /v1.
func Routes(d Deps) []Route {
	h := &handlers{d: d}
	return []Route{
		{http.MethodGet, "/whoami", h.whoami},
		{http.MethodPut, "/payers/{external_ref}", h.upsertPayer},
		{http.MethodGet, "/payers/{external_ref}", h.getPayer},
		{http.MethodPost, "/payment-requests", h.createPaymentRequest},
		{http.MethodGet, "/payment-requests", h.listPaymentRequests},
		{http.MethodGet, "/payment-requests/{id}", h.getPaymentRequest},
		{http.MethodPost, "/payment-requests/{id}/cancel", h.cancelPaymentRequest},
		{http.MethodGet, "/transactions", h.listTransactions},
		{http.MethodGet, "/transactions/{id}", h.getTransaction},
		{http.MethodGet, "/events", h.listEvents},
		{http.MethodPost, "/payment-requests/{id}/stk-push", h.sendSTKPush},
		{http.MethodGet, "/stk-pushes/{id}", h.getSTKPush},
	}
}

// NewRouter builds the /v1 handler. Mount it with r.Mount("/v1", ...).
func NewRouter(d Deps) http.Handler {
	if d.KeyLimiter == nil {
		d.KeyLimiter = httpx.NewRateLimiter(DefaultKeyRatePerMinute, DefaultKeyBurst)
	}
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = DefaultRequestTimeout
	}
	h := &handlers{d: d}
	r := chi.NewRouter()
	// Authenticate first, so unauthenticated callers learn nothing (not even
	// which paths exist: unknown paths are 401 too).
	r.Use(h.authenticate)
	r.Use(timeout(d.RequestTimeout))
	r.Use(limitBody)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		problem(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed for this resource.")
	})
	for _, rt := range Routes(d) {
		r.Method(rt.Method, rt.Pattern, rt.handler)
	}
	return r
}

type principalKey struct{}

func principalFrom(ctx context.Context) product.Principal {
	p, _ := ctx.Value(principalKey{}).(product.Principal)
	return p
}

// authenticate resolves "Authorization: Bearer tsp_..." to a product, then
// enforces the key's IP allow-list and its rate limit. The key itself is
// never logged.
func (h *handlers) authenticate(next http.Handler) http.Handler {
	unauthorized := func(w http.ResponseWriter, r *http.Request, challenge string) {
		w.Header().Set("WWW-Authenticate", challenge)
		problem(w, r, http.StatusUnauthorized, "unauthorized", "A valid API key is required.")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			unauthorized(w, r, `Bearer realm="talksasa-pay"`)
			return
		}
		p, err := h.d.Products.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, product.ErrUnauthorized) {
				unauthorized(w, r, `Bearer realm="talksasa-pay", error="invalid_token"`)
				return
			}
			h.fail(w, r, err)
			return
		}
		ctx := tlog.WithAttrs(r.Context(), slog.String("product_id", p.ProductID.String()), slog.String("key_id", p.KeyID))
		r = r.WithContext(ctx)
		if !p.Allows(httpx.ClientIPFrom(ctx)) {
			h.d.Logger.WarnContext(ctx, "api key used from an address outside its allow-list")
			problem(w, r, http.StatusForbidden, "ip_not_allowed", "This API key may not be used from your IP address.")
			return
		}
		if ok, retry := h.d.KeyLimiter.Allow(p.KeyID); !ok {
			httpx.TooManyRequests(w, r, retry)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, principalKey{}, p)))
	})
}

func timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			problem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body must be at most 64 KB.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}
