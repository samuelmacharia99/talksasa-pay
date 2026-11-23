// Package metrics exposes Prometheus metrics on an internal-only listener.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry wraps a dedicated Prometheus registry (not the global default, so
// tests and multiple servers do not collide).
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry creates a registry with Go runtime and process collectors.
func NewRegistry() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return &Registry{reg: reg}
}

// Prometheus returns the underlying registerer for domain metrics.
func (r *Registry) Prometheus() prometheus.Registerer { return r.reg }

// Handler serves /metrics.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// HTTP holds request metrics labelled by route pattern (bounded cardinality).
type HTTP struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewHTTP registers HTTP metrics for a named server ("api", "admin", ...).
func NewHTTP(r *Registry, server string) *HTTP {
	h := &HTTP{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "talksasa_pay", Name: "http_requests_total", Help: "HTTP requests by route and status.",
			ConstLabels: prometheus.Labels{"server": server},
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "talksasa_pay", Name: "http_request_duration_seconds", Help: "HTTP request latency.",
			ConstLabels: prometheus.Labels{"server": server},
			Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
	}
	r.reg.MustRegister(h.requests, h.duration)
	return h
}

// Observe records one request.
func (h *HTTP) Observe(method, route string, status int, d time.Duration) {
	h.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	h.duration.WithLabelValues(method, route).Observe(d.Seconds())
}
