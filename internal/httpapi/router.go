// Package httpapi wires HTTP routes to services. Handlers stay thin:
// decode, call a service, encode. App-scoped routes take the app id
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
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/appapi"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/reporting"
)

// Deps are the services the API needs.
type Deps struct {
	Logger       *slog.Logger
	Pool         *db.Pool
	Apps         *app.Service
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
	// PaySTKLimiter additionally throttles pay-page prompts
	// (POST /public/v1/pay/{token}/stk) per client IP; nil =
	// DefaultPaySTKPerMinute / DefaultPaySTKBurst.
	PaySTKLimiter *httpx.RateLimiter

	// M-Pesa. STK nil (a nil interface, not a typed nil *stk.Service) means
	// M-Pesa is off: stk_available is false, stk_push null, /v1 prompts 503
	// mpesa_unavailable and no /cb routes exist. The callback routes are
	// mounted only when STK.Available().
	STK STKService
	// MpesaCallbackToken is the secret path segment of the callback URLs
	// (/cb/mpesa/{token}/...). Empty rejects every callback.
	MpesaCallbackToken config.Secret
	// MpesaCallbackCIDRs are Safaricom's callback source networks; empty
	// accepts any source (development and sandbox only; config enforces).
	MpesaCallbackCIDRs []netip.Prefix
	// MpesaCallbackLimiter throttles callbacks per source IP (generous);
	// nil = DefaultCallbackPerMinute / DefaultCallbackBurst.
	MpesaCallbackLimiter *httpx.RateLimiter

	// App API (/v1). Charges is the charge service (*charge.Service); a
	// nil interface makes the /v1 charge routes answer 501.
	Charges           appapi.ChargeService
	View              apiview.View
	KeyLimiter        *httpx.RateLimiter // per API key; nil = appapi default
	APIRequestTimeout time.Duration      // 0 = appapi default
	APIMaxInFlight    int                // concurrent /v1 requests before shedding; 0 = 512
	// Readiness flips /readyz to 503 while the server drains on shutdown.
	Readiness *httpx.Readiness
}

// hostedTokenRe matches charges.hosted_token (32 random bytes,
// lowercase unpadded base32). Anything else is rejected before the database.
var hostedTokenRe = regexp.MustCompile(`^[a-z2-7]{52}$`)

// Defaults for optional rate limiters.
const (
	DefaultPaySTKPerMinute   = 6
	DefaultPaySTKBurst       = 3
	DefaultCallbackPerMinute = 3000
	DefaultCallbackBurst     = 1000
)

// logPath is the path the access log (and every log line of a request)
// shows: secret tokens in the path are replaced with [redacted].
var logPath = httpx.RedactSegments("/cb/mpesa/*", "/cb/**", "/public/v1/pay/*")

// NewRouter builds the public API handler.
func NewRouter(d Deps) http.Handler {
	if d.PayLimiter == nil {
		d.PayLimiter = httpx.NewRateLimiter(60, 30)
	}
	if d.PaySTKLimiter == nil {
		d.PaySTKLimiter = httpx.NewRateLimiter(DefaultPaySTKPerMinute, DefaultPaySTKBurst)
	}
	if d.MpesaCallbackLimiter == nil {
		d.MpesaCallbackLimiter = httpx.NewRateLimiter(DefaultCallbackPerMinute, DefaultCallbackBurst)
	}
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.ClientIP(d.TrustedProxies))
	r.Use(httpx.AccessLog(d.Logger, d.Metrics, httpx.LogPath(logPath)))
	r.Use(httpx.Recover(d.Logger))
	r.Use(httpx.SecurityHeaders)
	r.Use(httpx.MaxBody(d.MaxBodyBytes))

	r.NotFound(notFound)
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
		pub.With(httpx.LimitByIP(d.PaySTKLimiter, "pay-stk")).Post("/pay/{token}/stk", payPageSTKPush(d))
	})

	// Safaricom callbacks: no app-API auth; the secret path token and
	// the source-IP allow-list decide. Absent entirely while M-Pesa is off.
	if d.STK != nil && d.STK.Available() {
		r.Route("/cb/mpesa/{token}", newCallbacks(d).routes)
	}

	// App integration API: auth (key -> IP allow-list -> per-key rate
	// limit), strict JSON, every query scoped to the key's app.
	// Timeout outside LoadShed: a slot is held until the handler really
	// returns, so MaxInFlight truly bounds concurrent database work.
	maxInFlight := d.APIMaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = 512
	}
	apiTimeout := d.APIRequestTimeout
	if apiTimeout <= 0 {
		apiTimeout = appapi.DefaultRequestTimeout
	}
	r.With(httpx.Timeout(apiTimeout+2*time.Second), httpx.LoadShed(maxInFlight)).Mount("/v1", appapi.NewRouter(appapi.Deps{
		Logger: d.Logger, Pool: d.Pool, Apps: d.Apps, Charges: d.Charges,
		View: d.View, KeyLimiter: d.KeyLimiter, RequestTimeout: d.APIRequestTimeout, STK: appSTK(d.STK),
	}))
	return r
}

// appSTK keeps a nil STK a nil appapi.STKService (not a non-nil
// interface holding nil).
func appSTK(s STKService) appapi.STKService {
	if s == nil {
		return nil
	}
	return s
}

// notFound is the one 404 for unknown paths (and rejected callbacks, which
// must look exactly like unknown paths).
func notFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "No such resource."))
}

type paybillJSON struct {
	BusinessNumber string `json:"business_number"`
	AccountNumber  string `json:"account_number"`
}

type payPageJSON struct {
	AppName      string       `json:"app_name"`
	Reference    string       `json:"reference"`
	Description  *string      `json:"description"`
	Currency     string       `json:"currency"`
	Amount       int64        `json:"amount"`
	Status       string       `json:"status"`
	Payable      bool         `json:"payable"`
	ExpiresAt    time.Time    `json:"expires_at"`
	SucceededAt  *time.Time   `json:"succeeded_at"`
	Paybill      *paybillJSON `json:"paybill"`
	STKAvailable bool         `json:"stk_available"`
	STKPush      *payPageSTK  `json:"stk_push"`
}

// payPageSTK is the latest prompt for a charge (no ids, no phone).
type payPageSTK struct {
	Status    string    `json:"status"`
	Message   *string   `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// payPage serves the hosted pay page's data. The token is the only key;
// unknown, malformed and suspended-app tokens all get the same 404 so
// the endpoint cannot be used to probe for valid tokens. It returns only
// what the customer needs to pay: no phone, metadata or internal ids.
func payPage(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := lookupPayPage(d, w, r)
		if !ok {
			return
		}
		out := payPageJSON{
			AppName: p.AppName, Reference: p.Reference, Description: p.Description, Currency: p.Currency,
			Amount: p.Amount, Status: string(p.Status), Payable: p.Payable,
			ExpiresAt: p.ExpiresAt.UTC(), SucceededAt: p.SucceededAt,
			STKAvailable: stkAvailable(d, p),
			STKPush:      latestSTK(d, r, p),
		}
		if d.Paybill != "" && p.Payable {
			out.Paybill = &paybillJSON{BusinessNumber: d.Paybill, AccountNumber: p.AccountNumber}
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// payLinkNotFound is the pay page's single 404.
func payLinkNotFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "This payment link is not valid."))
}

// lookupPayPage resolves {token}. Malformed, unknown and suspended-app
// tokens all get the same 404 (written here; ok is then false).
func lookupPayPage(d Deps, w http.ResponseWriter, r *http.Request) (reporting.PayPage, bool) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	token := chi.URLParam(r, "token")
	if !hostedTokenRe.MatchString(token) {
		payLinkNotFound(w, r)
		return reporting.PayPage{}, false
	}
	p, err := d.Reporting.PayPageByToken(r.Context(), token)
	if errors.Is(err, reporting.ErrNotFound) {
		payLinkNotFound(w, r)
		return reporting.PayPage{}, false
	}
	if err != nil {
		httpx.InternalError(w, r, d.Logger, err)
		return reporting.PayPage{}, false
	}
	return p, true
}
