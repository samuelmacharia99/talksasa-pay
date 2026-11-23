// Package httpx holds HTTP plumbing shared by every listener: RFC 9457
// problem responses, middleware (request id, recovery, access log, body
// limits, security headers) and graceful server lifecycle.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Problem is an RFC 9457 problem details response. Detail is shown to API
// clients, so it must never contain internal error text, SQL or secrets.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Code      string       `json:"code,omitempty"` // stable machine-readable code
	RequestID string       `json:"request_id,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`
}

// FieldError points at one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

const problemBase = "https://pay.talksasa.com/problems/"

// NewProblem builds a problem with a stable code.
func NewProblem(status int, code, detail string) Problem {
	return Problem{Type: problemBase + code, Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

// WriteProblem writes p as application/problem+json.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	p.RequestID = RequestIDFrom(r.Context())
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// InternalError logs err with the request context and returns a generic 500.
func InternalError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	logger.ErrorContext(r.Context(), "internal error", slog.String("error", err.Error()))
	WriteProblem(w, r, NewProblem(http.StatusInternalServerError, "internal_error", "An unexpected error occurred. Quote the request_id when contacting support."))
}

// WriteJSON writes v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
