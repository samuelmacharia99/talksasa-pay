// Package log builds the service's structured logger.
//
// Output is JSON (one object per line) for ingestion by the log pipeline.
// Two layers protect against leaking sensitive data:
//
//  1. Attribute keys that look sensitive (password, secret, passkey, token,
//     credential, authorization, ...) are replaced with "[REDACTED]".
//  2. Phone numbers (keys phone, msisdn, ...) are masked to 2547****123.
//
// This is a safety net, not a licence: code must still never pass secrets or
// raw provider payloads to the logger. Wrap secrets in config.Secret.
package log

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

const redacted = "[REDACTED]"

// sensitiveKeyParts are matched case-insensitively against attribute keys.
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "passkey", "token", "credential",
	"authorization", "api_key", "apikey", "cookie", "pepper", "private",
	"dsn", "database_url", "signature", "otp", "totp", "consumer_key", "consumerkey",
}

var phoneKeys = map[string]struct{}{
	"phone": {}, "msisdn": {}, "phone_number": {}, "phonenumber": {}, "partyb": {}, "partya": {},
}

// Options configures New.
type Options struct {
	Level   slog.Leveler
	Service string
	Version string
	Env     string
}

// New returns a JSON logger with redaction and context-attribute support.
func New(w io.Writer, opts Options) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       opts.Level,
		AddSource:   false,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(&contextHandler{Handler: h}).With(
		slog.String("service", opts.Service),
		slog.String("version", opts.Version),
		slog.String("env", opts.Env),
	)
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	key := strings.ToLower(a.Key)
	if _, ok := phoneKeys[key]; ok {
		return slog.String(a.Key, MaskPhone(a.Value.String()))
	}
	if IsSensitiveKey(key) {
		return slog.String(a.Key, redacted)
	}
	return a
}

// IsSensitiveKey reports whether a field name should never be logged.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, p := range sensitiveKeyParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// MaskPhone keeps the first 4 and last 3 characters: 254712345678 -> 2547*****678.
func MaskPhone(p string) string {
	if len(p) <= 7 {
		return strings.Repeat("*", len(p))
	}
	return p[:4] + strings.Repeat("*", len(p)-7) + p[len(p)-3:]
}

type ctxKey struct{}

// WithAttrs returns a context whose log records carry the given attributes
// (request_id, app_id, transaction_id, job_id, ...).
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// contextHandler adds attributes stored in the context by WithAttrs.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// Err is a convenience attribute for errors.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", err.Error())
}
