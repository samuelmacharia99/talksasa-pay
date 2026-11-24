package talksasapay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Stable error codes (Problem.code). New codes may appear; treat unknown
// codes by HTTP status.
const (
	CodeUnauthorized           = "unauthorized"
	CodeIPNotAllowed           = "ip_not_allowed"
	CodeProductSuspended       = "product_suspended"
	CodeRateLimited            = "rate_limited"
	CodeOverloaded             = "overloaded"
	CodeNotFound               = "not_found"
	CodeValidationFailed       = "validation_failed"
	CodeInvalidJSON            = "invalid_json"
	CodeUnsupportedMediaType   = "unsupported_media_type"
	CodeIdempotencyKeyRequired = "idempotency_key_required"
	CodeIdempotencyConflict    = "idempotency_conflict"
	CodeDuplicateReference     = "duplicate_reference"
	CodePayerClosed            = "payer_closed"
	CodeNotCancellable         = "not_cancellable"
	CodePayloadTooLarge        = "payload_too_large"
	CodeInternalError          = "internal_error"
)

// FieldError is one entry of a validation_failed problem.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// APIError is a non-2xx response (RFC 9457 problem details).
type APIError struct {
	Status      int          `json:"status"`
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Code        string       `json:"code"`
	Detail      string       `json:"detail"`
	RequestID   string       `json:"request_id"`
	FieldErrors []FieldError `json:"errors"`
	// RetryAfter is the parsed Retry-After header (0 if absent).
	RetryAfter time.Duration `json:"-"`
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "talksasapay: %d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	switch {
	case e.Detail != "":
		b.WriteString(": " + e.Detail)
	case e.Title != "":
		b.WriteString(": " + e.Title)
	}
	for i, fe := range e.FieldErrors {
		if i == 0 {
			b.WriteString(" [")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(fe.Field + ": " + fe.Message)
		if i == len(e.FieldErrors)-1 {
			b.WriteString("]")
		}
	}
	if e.RequestID != "" {
		b.WriteString(" (request_id " + e.RequestID + ")")
	}
	return b.String()
}

// Temporary reports whether retrying later may succeed.
func (e *APIError) Temporary() bool { return retryableStatus(e.Status) || e.Status >= 500 }

// AsAPIError returns the *APIError in err's chain, if any.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// IsCode reports whether err is an *APIError with the given code.
func IsCode(err error, code string) bool {
	ae, ok := AsAPIError(err)
	return ok && ae.Code == code
}

// IsNotFound reports a 404 (unknown id/external_ref, or another product's).
func IsNotFound(err error) bool {
	ae, ok := AsAPIError(err)
	return ok && ae.Status == http.StatusNotFound
}

// StatusCode returns the HTTP status of an *APIError in err's chain, or 0.
func StatusCode(err error) int {
	if ae, ok := AsAPIError(err); ok {
		return ae.Status
	}
	return 0
}

func decodeAPIError(hr *http.Response, raw []byte) *APIError {
	e := &APIError{}
	if len(raw) == 0 || json.Unmarshal(raw, e) != nil {
		*e = APIError{Detail: snippet(raw)} // not a problem document (e.g. a proxy's HTML page)
	}
	e.Status = hr.StatusCode // the transport status wins over the body
	if e.RequestID == "" {
		e.RequestID = hr.Header.Get("X-Request-Id")
	}
	if e.Title == "" {
		e.Title = http.StatusText(hr.StatusCode)
	}
	e.RetryAfter = parseRetryAfter(hr.Header.Get("Retry-After"), time.Now())
	return e
}

// snippet keeps a non-JSON error body short and printable (proxies return HTML).
func snippet(raw []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(raw))
	if !utf8.ValidString(s) {
		return ""
	}
	if len(s) > limit {
		s = s[:limit]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "..."
	}
	return s
}
