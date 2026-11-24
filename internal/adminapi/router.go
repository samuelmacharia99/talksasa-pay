// Package adminapi is the staff admin API (/admin/v1). It is served on its
// own listener (TSP_ADMIN_ADDR) that only the admin Next.js server can reach;
// browsers never call it directly.
//
// Authentication: the Next.js server forwards the staff member's session
// token as "Authorization: Bearer <token>" (it keeps the token in an httpOnly
// cookie). Every route declares the permission it needs; routes are declared
// in one table so the RBAC test can check every route against every role.
package adminapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/webhook"
)

// Deps are the services the admin API needs.
type Deps struct {
	Logger    *slog.Logger
	Admin     *admin.Service
	Products  *product.Service
	Reporting *reporting.Service
	// Webhooks serves the webhook-delivery screens and redelivery
	// (webhook.NewDeliveries(pool, riverClient); an insert-only River
	// client is enough).
	Webhooks       *webhook.Deliveries
	Metrics        *metrics.HTTP
	TrustedProxies []netip.Prefix
	MaxBodyBytes   int64
	// AuthLimiter throttles sign-in, 2FA and setup attempts per client IP.
	// nil uses the default (10 per minute, burst 10).
	AuthLimiter *httpx.RateLimiter
	// STK resolves needs_review M-Pesa payments and lists a request's STK
	// pushes (*stk.Service). nil: resolve answers 503 mpesa_unavailable and
	// push lists are empty.
	STK STKService
}

// Route is one authenticated admin route and the permission it requires.
type Route struct {
	Method  string
	Pattern string
	Perm    admin.Permission
	handler http.HandlerFunc
}

type handlers struct {
	d Deps
	// rd overrides d.Reporting for the M-Pesa screens (tests).
	rd reads
}

func (h *handlers) reads() reads {
	if h.rd != nil {
		return h.rd
	}
	return h.d.Reporting
}

// Routes lists every authenticated route (exported for the RBAC test).
func Routes(d Deps) []Route {
	h := &handlers{d: d}
	return []Route{
		{http.MethodGet, "/auth/me", admin.PermRead, h.me},
		{http.MethodPost, "/auth/logout", admin.PermRead, h.logout},
		{http.MethodPost, "/auth/password", admin.PermRead, h.changePassword},

		{http.MethodGet, "/dashboard", admin.PermRead, h.dashboard},
		{http.MethodGet, "/integrity", admin.PermRead, h.integrity},

		{http.MethodGet, "/products", admin.PermRead, h.listProducts},
		{http.MethodPost, "/products", admin.PermManageProducts, h.createProduct},
		{http.MethodGet, "/products/{id}", admin.PermRead, h.getProduct},
		{http.MethodPut, "/products/{id}/webhook-url", admin.PermManageProducts, h.setWebhookURL},
		{http.MethodPost, "/products/{id}/rotate-webhook-secret", admin.PermManageProducts, h.rotateWebhookSecret},
		{http.MethodPost, "/products/{id}/status", admin.PermManageProducts, h.setProductStatus},
		{http.MethodGet, "/products/{id}/api-keys", admin.PermRead, h.listAPIKeys},
		{http.MethodPost, "/products/{id}/api-keys", admin.PermManageProducts, h.issueAPIKey},
		{http.MethodPost, "/products/{id}/api-keys/{keyID}/revoke", admin.PermManageProducts, h.revokeAPIKey},
		{http.MethodPut, "/products/{id}/api-keys/{keyID}/allowed-ips", admin.PermManageProducts, h.setAllowedIPs},

		{http.MethodGet, "/webhook-deliveries", admin.PermRead, h.listWebhookDeliveries},
		{http.MethodGet, "/webhook-deliveries/{eventID}", admin.PermRead, h.getWebhookDelivery},
		{http.MethodPost, "/webhook-deliveries/{eventID}/redeliver", admin.PermManageProducts, h.redeliverWebhook},

		{http.MethodGet, "/payers", admin.PermRead, h.listPayers},
		{http.MethodGet, "/payers/{id}", admin.PermRead, h.getPayer},
		{http.MethodGet, "/payment-requests", admin.PermRead, h.listRequests},
		{http.MethodGet, "/payment-requests/{id}", admin.PermRead, h.getRequest},
		{http.MethodGet, "/transactions", admin.PermRead, h.listTransactions},
		{http.MethodGet, "/transactions/{id}", admin.PermRead, h.getTransaction},
		{http.MethodPost, "/transactions/{id}/resolve", admin.PermResolvePayments, h.resolveTransaction},
		{http.MethodGet, "/payment-requests/{id}/stk-pushes", admin.PermRead, h.listSTKPushes},
		{http.MethodGet, "/mpesa/callbacks", admin.PermRead, h.listMpesaCallbacks},
		{http.MethodGet, "/audit", admin.PermRead, h.listAudit},

		{http.MethodGet, "/users", admin.PermManageUsers, h.listUsers},
		{http.MethodPost, "/users", admin.PermManageUsers, h.inviteUser},
		{http.MethodPost, "/users/{id}/role", admin.PermManageUsers, h.setUserRole},
		{http.MethodPost, "/users/{id}/disable", admin.PermManageUsers, h.disableUser},
		{http.MethodPost, "/users/{id}/reset", admin.PermManageUsers, h.resetUser},
	}
}

// NewRouter builds the admin API handler.
func NewRouter(d Deps) http.Handler {
	if d.AuthLimiter == nil {
		d.AuthLimiter = httpx.NewRateLimiter(10, 10)
	}
	h := &handlers{d: d}
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

	r.Route("/admin/v1", func(v chi.Router) {
		// Unauthenticated: sign-in and invitation setup, rate limited per IP.
		v.Group(func(pub chi.Router) {
			pub.Use(httpx.LimitByIP(d.AuthLimiter, "admin-auth"))
			pub.Post("/auth/login", h.login)
			pub.Post("/auth/mfa", h.verifyMFA)
			pub.Get("/auth/setup/{token}", h.beginSetup)
			pub.Post("/auth/setup/{token}", h.completeSetup)
		})
		v.Group(func(auth chi.Router) {
			auth.Use(h.requireSession)
			for _, rt := range Routes(d) {
				auth.With(requirePerm(rt.Perm)).Method(rt.Method, rt.Pattern, rt.handler)
			}
		})
	})
	return r
}

type principalKey struct{}

func principalFrom(ctx context.Context) admin.Principal {
	p, _ := ctx.Value(principalKey{}).(admin.Principal)
	return p
}

func bearer(r *http.Request) string {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(t)
}

func (h *handlers) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.d.Admin.Authenticate(r.Context(), bearer(r))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = tlog.WithAttrs(ctx, slog.String("admin_user_id", p.UserID.String()))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requirePerm(perm admin.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !principalFrom(r.Context()).Can(perm) {
				httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusForbidden, "forbidden", "Your role does not allow this action."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
