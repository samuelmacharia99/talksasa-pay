package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
)

type ridKey struct{}

// RequestIDHeader carries the correlation id in requests and responses.
const RequestIDHeader = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// RequestIDFrom returns the request id stored by RequestID.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ridKey{}).(string)
	return id
}

// RequestID accepts a well-formed inbound X-Request-Id (so callers can
// correlate) or generates one, echoes it, and adds it to every log line.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = uuid.NewString()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ridKey{}, id)
		ctx = tlog.WithAttrs(ctx, slog.String("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recover turns panics into a logged 500 instead of a dropped connection.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}
					logger.ErrorContext(r.Context(), "panic in handler",
						slog.String("panic", fmt.Sprint(rec)), slog.String("stack", string(debug.Stack())))
					WriteProblem(w, r, NewProblem(http.StatusInternalServerError, "internal_error", "An unexpected error occurred."))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the response status and size.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLogOption customises AccessLog.
type AccessLogOption func(*accessLogConfig)

type accessLogConfig struct {
	redact PathRedactor
}

// LogPath makes AccessLog also log the request path, passed through redact
// (which must hide every secret path segment, e.g. tokens). The redacted
// path is added to the request context's log attributes too, so error and
// panic logs from inner handlers carry the same safe "path". The raw path is
// never logged.
func LogPath(redact PathRedactor) AccessLogOption {
	return func(c *accessLogConfig) { c.redact = redact }
}

// AccessLog logs one line per request and records HTTP metrics. It logs the
// route pattern (not the raw path, which may carry tokens; see LogPath for a
// redacted path) and never logs headers or bodies.
func AccessLog(logger *slog.Logger, m *metrics.HTTP, opts ...AccessLogOption) func(http.Handler) http.Handler {
	var cfg accessLogConfig
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			logCtx := r.Context() // without "path": it is added explicitly below
			var pathAttr []any
			if cfg.redact != nil {
				a := slog.String("path", truncate(cfg.redact(r.URL.Path), 200))
				pathAttr = []any{a}
				r = r.WithContext(tlog.WithAttrs(r.Context(), a))
			}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			el := time.Since(start)
			if m != nil {
				m.Observe(r.Method, route, rec.status, el)
			}
			lvl := slog.LevelInfo
			switch {
			case rec.status >= 500:
				lvl = slog.LevelError
			case rec.status >= 400:
				lvl = slog.LevelWarn
			case route == "/healthz" || route == "/readyz":
				lvl = slog.LevelDebug
			}
			attrs := append([]any{
				slog.String("method", r.Method), slog.String("route", route), slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes), slog.Float64("duration_ms", float64(el.Microseconds())/1000), slog.String("remote_ip", clientIP(r)),
				slog.String("user_agent", truncate(r.UserAgent(), 200)),
			}, pathAttr...)
			logger.Log(logCtx, lvl, "http request", attrs...)
		})
	}
}

// MaxBody caps request bodies; larger bodies fail to read with an error the
// handler turns into 413.
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > n {
				WriteProblem(w, r, NewProblem(http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("Request body must be at most %d bytes.", n)))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets conservative defaults for an API that serves JSON only.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// clientIP is the address resolved by the ClientIP middleware, falling back
// to the TCP peer.
func clientIP(r *http.Request) string {
	if ip := ClientIPFrom(r.Context()); ip.IsValid() {
		return ip.String()
	}
	return peerIP(r.RemoteAddr).String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
