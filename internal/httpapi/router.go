// Package httpapi wires HTTP routes to services. Handlers stay thin:
// decode, call a service, encode. Product-scoped routes take the product id
// ONLY from the authenticated API key, never from the request.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/productapi"
	"github.com/talksasa/talksasa-pay/internal/reporting"
)

// Deps are the services the API needs.
type Deps struct {
	Logger       *slog.Logger
	Pool         *db.Pool
	Products     *product.Service
	Reporting    *reporting.Service
	Metrics      *metrics.HTTP
	MaxBodyBytes int64
	Version      string
	// TrustedProxies may forward the end user's IP (the pay Next.js server).
	TrustedProxies []netip.Prefix
	// Paybill is shown on the hosted pay page ("" hides the instructions).
	Paybill string
	// PayLimiter throttles pay-page lookups per client IP; nil = default.
	PayLimiter *httpx.RateLimiter

	// Product API (/v1).
	Payers            *payer.Service
	Requests          *paymentrequest.Service
	View              apiview.View
	KeyLimiter        *httpx.RateLimiter // per API key; nil = productapi default
	APIRequestTimeout time.Duration      // 0 = productapi default
	APIMaxInFlight    int                // concurrent /v1 requests before shedding; 0 = 512
	// Readiness flips /readyz to 503 while the server drains on shutdown.
	Readiness *httpx.Readiness
}

// hostedTokenRe matches payment_requests.hosted_token (32 random bytes,
// lowercase unpadded base32). Anything else is rejected before the database.
var hostedTokenRe = regexp.MustCompile(`^[a-z2-7]{52}$`)

// NewRouter builds the public API handler.
func NewRouter(d Deps) http.Handler {
	if d.PayLimiter == nil {
		d.PayLimiter = httpx.NewRateLimiter(60, 30)
	}
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.ClientIP(d.TrustedProxies))
	r.Use(httpx.AccessLog(d.Logger, d.Metrics))
	r.Use(httpx.Recover(d.Logger))
	r.Use(httpx.SecurityHeaders)
	r.Use(httpx.MaxBody(d.MaxBodyBytes))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "No such resource."))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed for this resource."))
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": d.Version})
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Readiness != nil && d.Readiness.Draining() {
			httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "draining", "Shutting down."))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.Pool.Ping(ctx); err != nil {
			d.Logger.WarnContext(r.Context(), "readiness check failed", slog.String("error", err.Error()))
			httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "not_ready", "Database unavailable."))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	r.Route("/public/v1", func(pub chi.Router) {
		pub.Use(httpx.LimitByIP(d.PayLimiter, "pay-page"))
		pub.Get("/pay/{token}", payPage(d))
	})

	// Product integration API: auth (key -> IP allow-list -> per-key rate
	// limit), strict JSON, every query scoped to the key's product.
	// Timeout outside LoadShed: a slot is held until the handler really
	// returns, so MaxInFlight truly bounds concurrent database work.
	maxInFlight := d.APIMaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = 512
	}
	apiTimeout := d.APIRequestTimeout
	if apiTimeout <= 0 {
		apiTimeout = productapi.DefaultRequestTimeout
	}
	r.With(httpx.Timeout(apiTimeout+2*time.Second), httpx.LoadShed(maxInFlight)).Mount("/v1", productapi.NewRouter(productapi.Deps{
		Logger: d.Logger, Pool: d.Pool, Products: d.Products, Payers: d.Payers, Requests: d.Requests,
		View: d.View, KeyLimiter: d.KeyLimiter, RequestTimeout: d.APIRequestTimeout,
	}))
	return r
}

type paybillJSON struct {
	BusinessNumber string `json:"business_number"`
	AccountNumber  string `json:"account_number"`
}

type payPageJSON struct {
	ProductName  string       `json:"product_name"`
	Reference    string       `json:"reference"`
	Description  string       `json:"description"`
	Currency     string       `json:"currency"`
	Amount       int64        `json:"amount"`
	AmountPaid   int64        `json:"amount_paid"`
	Outstanding  int64        `json:"outstanding"`
	Status       string       `json:"status"`
	Payable      bool         `json:"payable"`
	ExpiresAt    *time.Time   `json:"expires_at"`
	PaidAt       *time.Time   `json:"paid_at"`
	Paybill      *paybillJSON `json:"paybill"`
	STKAvailable bool         `json:"stk_available"`
}

// payPage serves the hosted pay page's data. The token is the only key;
// unknown, malformed and suspended-product tokens all get the same 404 so
// the endpoint cannot be used to probe for valid tokens. It returns only
// what the payer needs to pay: no name, phone, email or internal ids.
func payPage(d Deps) http.HandlerFunc {
	notFound := func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "This payment link is not valid."))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		token := chi.URLParam(r, "token")
		if !hostedTokenRe.MatchString(token) {
			notFound(w, r)
			return
		}
		p, err := d.Reporting.PayPageByToken(r.Context(), token)
		if errors.Is(err, reporting.ErrNotFound) {
			notFound(w, r)
			return
		}
		if err != nil {
			httpx.InternalError(w, r, d.Logger, err)
			return
		}
		out := payPageJSON{
			ProductName: p.ProductName, Reference: p.ProductReference, Description: p.Description, Currency: "KES",
			Amount: p.Amount, AmountPaid: p.AmountPaid, Outstanding: p.Amount - p.AmountPaid, Status: string(p.Status),
			Payable: p.Payable, ExpiresAt: p.ExpiresAt, PaidAt: p.PaidAt,
			// STK push arrives with the Daraja integration (Phase 2).
			STKAvailable: false,
		}
		if d.Paybill != "" && p.Payable {
			out.Paybill = &paybillJSON{BusinessNumber: d.Paybill, AccountNumber: p.AccountNumber}
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}
