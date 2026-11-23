// Package httpapi wires HTTP routes to services. Handlers stay thin:
// decode, call a service, encode. Product-scoped routes take the product id
// ONLY from the authenticated API key, never from the request.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/product"
)

// Deps are the services the API needs.
type Deps struct {
	Logger       *slog.Logger
	Pool         *db.Pool
	Products     *product.Service
	Metrics      *metrics.HTTP
	MaxBodyBytes int64
	Version      string
}

// NewRouter builds the public API handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
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
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.Pool.Ping(ctx); err != nil {
			d.Logger.WarnContext(r.Context(), "readiness check failed", slog.String("error", err.Error()))
			httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "not_ready", "Database unavailable."))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	r.Route("/v1", func(v1 chi.Router) {
		v1.Use(requireProduct(d.Products, d.Logger))
		v1.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
			p := principalFrom(r.Context())
			httpx.WriteJSON(w, http.StatusOK, map[string]string{
				"product_id": p.ProductID.String(), "product_name": p.ProductName,
				"account_prefix": p.AccountPrefix, "key_id": p.KeyID,
			})
		})
	})
	return r
}

type principalKey struct{}

func principalFrom(ctx context.Context) product.Principal {
	p, _ := ctx.Value(principalKey{}).(product.Principal)
	return p
}

// requireProduct authenticates "Authorization: Bearer tsp_..." and scopes the
// request to that key's product.
func requireProduct(products *product.Service, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(auth, "Bearer ")
			if !ok || token == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="talksasa-pay"`)
				httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, "unauthorized", "A valid API key is required."))
				return
			}
			p, err := products.Authenticate(r.Context(), strings.TrimSpace(token))
			if err != nil {
				if !errors.Is(err, product.ErrUnauthorized) {
					httpx.InternalError(w, r, logger, err)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="talksasa-pay", error="invalid_token"`)
				httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, "unauthorized", "A valid API key is required."))
				return
			}
			ctx := context.WithValue(r.Context(), principalKey{}, p)
			ctx = tlog.WithAttrs(ctx, slog.String("product_id", p.ProductID.String()), slog.String("key_id", p.KeyID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
