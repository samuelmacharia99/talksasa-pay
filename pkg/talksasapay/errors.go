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

// Stable error codes (Problem.code), one per code in api/app.openapi.yaml.
// New codes may appear; treat unknown codes by HTTP status.
const (
	CodeUnauthorized           = "unauthorized"           // 401: key missing, malformed, revoked or expired
	CodeIPNotAllowed           = "ip_not_allowed"         // 403: key restricted to an IP allow-list
	CodeAppSuspended           = "app_suspended"          // 403: Talksasa suspended this app
	CodeRateLimited            = "rate_limited"           // 429: retried automatically (Retry-After)
	CodeOverloaded             = "overloaded"             // 503: nothing changed; retried automatically
	CodeBusy                   = "busy"                   // 503: nothing changed; retried automatically
	CodeTimeout                = "timeout"                // 503: outcome unknown; retried with the SAME key
	CodeNotFound               = "not_found"              // 404 (another app's ids are 404 too)
	CodeMethodNotAllowed       = "method_not_allowed"     // 405
	CodeValidationFailed       = "validation_failed"      // 422: see APIError.FieldErrors
	CodeInvalidJSON            = "invalid_json"           // 400
	CodeUnsupportedMediaType   = "unsupported_media_type" // 415
	CodePayloadTooLarge        = "payload_too_large"      // 413
	CodeIdempotencyKeyRequired = "idempotency_key_required"
	CodeIdempotencyConflict    = "idempotency_conflict" // 409: key reused with a different body
	CodeDuplicateReference     = "duplicate_reference"  // 409: you already have a charge with this reference
	CodeNotCancellable         = "not_cancellable"      // 409: only pending charges can be cancelled
	CodeNotPayable             = "not_payable"          // 409 STK push: the charge is not pending
	CodeAmountTooLarge         = "amount_too_large"     // 409 STK push: above the M-Pesa prompt limit; use the paybill
	CodeSTKInProgress          = "stk_in_progress"      // 409 STK push: a prompt for this charge is already live
	CodeMpesaUnavailable       = "mpesa_unavailable"    // 503: M-Pesa prompts are off here; never retried
	CodeMpesaError             = "mpesa_error"          // 502: Safaricom could not send the prompt; never retried
	CodeNotImplemented         = "not_implemented"      // 501
	CodeInternalError          = "internal_error"       // 500: not retried
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
func (e *APIError) Temporary() bool {
	if e.Code == CodeMpesaUnavailable {
		return false
	}
	return retryableStatus(e.Status) || e.Status >= 500
}

// finalCode reports codes that arrive with a retryable status (502/503) but
// are final answers: retrying inline would not help.
func finalCode(code string) bool {
	return code == CodeMpesaUnavailable || code == CodeMpesaError
}

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
