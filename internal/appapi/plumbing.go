package appapi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
)

// ---------------------------------------------------------------------------
// Problems
// ---------------------------------------------------------------------------

func problem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	httpx.WriteProblem(w, r, httpx.NewProblem(status, code, detail))
}

// invalid writes a validation_failed problem. Body, path and header errors
// are 422; query-string errors are 400.
func invalid(w http.ResponseWriter, r *http.Request, status int, errs ...httpx.FieldError) {
	p := httpx.NewProblem(status, "validation_failed", "The request is not valid; see errors.")
	p.Errors = errs
	httpx.WriteProblem(w, r, p)
}

func fieldErr(field, msg string) httpx.FieldError {
	return httpx.FieldError{Field: field, Message: msg}
}

// errCrossApp: a service returned a row of another app. Never rendered;
// answered as a generic 500.
var errCrossApp = errors.New("appapi: service returned another app's resource")

// fail maps service errors to problems. Anything unexpected is logged and
// returned as a generic 500, so no internal detail leaks.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	unprocessable := func(field, msg string) { invalid(w, r, http.StatusUnprocessableEntity, fieldErr(field, msg)) }
	switch {
	case charge.ErrNotImplemented(err):
		notImplemented(w, r)
	case errors.Is(err, charge.ErrNotFound):
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
	case errors.Is(err, charge.ErrIdempotencyConflict):
		problem(w, r, http.StatusConflict, "idempotency_conflict",
			"This Idempotency-Key was already used with different parameters. Use a new key for a new request.")
	case errors.Is(err, charge.ErrDuplicateReference):
		problem(w, r, http.StatusConflict, "duplicate_reference",
			"A charge with this reference already exists. Use another reference.")
	case errors.Is(err, charge.ErrNotCancellable):
		problem(w, r, http.StatusConflict, "not_cancellable", "Only pending charges can be cancelled.")
	case errors.Is(err, charge.ErrAppNotActive):
		problem(w, r, http.StatusForbidden, "app_suspended", "This app is not active.")
	case errors.Is(err, charge.ErrInvalidAmount):
		unprocessable("amount", "must be a whole number of KES from 1 to 250000")
	case errors.Is(err, charge.ErrInvalidReference):
		unprocessable("reference", "must be 1-128 characters")
	case errors.Is(err, charge.ErrInvalidDescription):
		unprocessable("description", "must be 1-500 characters")
	case errors.Is(err, charge.ErrInvalidMetadata):
		unprocessable("metadata", "must be a JSON object of at most 4096 bytes")
	case errors.Is(err, charge.ErrInvalidAccountNumber):
		unprocessable("account_number", "must be your prefix, a hyphen, a digit, then A-Z or 0-9, at most 12 characters")
	case errors.Is(err, charge.ErrInvalidExpiry):
		unprocessable("expires_in", "must be from 900 to 604800 seconds")
	case errors.Is(err, charge.ErrInvalidPhone), errors.Is(err, phone.ErrInvalid):
		unprocessable("phone", phoneMessage)
	case errors.Is(err, charge.ErrAccountNumberInUse):
		unprocessable("account_number", "is already used by another pending charge")
	case errors.Is(err, charge.ErrInvalidIdempotency):
		invalid(w, r, http.StatusBadRequest, fieldErr("Idempotency-Key", "must be 1-255 printable characters"))
	case errors.Is(err, charge.ErrInvalidCursor):
		invalid(w, r, http.StatusBadRequest, fieldErr("cursor", "is not a cursor returned by this endpoint"))
	case errors.Is(err, charge.ErrInvalidStatus):
		invalid(w, r, http.StatusBadRequest, fieldErr("status", "must be one of pending, needs_review, succeeded, expired, cancelled, reversed"))
	case db.IsBusy(err):
		// Lock/statement timeout or retries exhausted: nothing was committed.
		httpx.Busy(w, r, time.Second)
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "1")
		problem(w, r, http.StatusServiceUnavailable, "overloaded", "The service is busy. Retry shortly.")
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client went away; nobody reads this.
		problem(w, r, http.StatusServiceUnavailable, "overloaded", "Request cancelled.")
	default:
		httpx.InternalError(w, r, h.d.Logger, err)
	}
}

// ---------------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------------

// decode reads exactly one JSON object with no unknown fields. It writes the
// problem and returns false on any error.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		problem(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send Content-Type: application/json.")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("trailing data")
		}
	}
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooBig):
		problem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body must be at most 64 KB.")
	case errors.As(err, &typeErr) && typeErr.Field != "":
		invalid(w, r, http.StatusUnprocessableEntity, fieldErr(typeErr.Field, "has the wrong type (expected "+jsonKind(typeErr.Type.Kind().String())+")"))
	case strings.HasPrefix(err.Error(), `json: unknown field "`):
		name := strings.TrimSuffix(strings.TrimPrefix(err.Error(), `json: unknown field "`), `"`)
		if len(name) > 64 || !utf8.ValidString(name) {
			name = "(unknown)"
		}
		invalid(w, r, http.StatusUnprocessableEntity, fieldErr(name, "unknown field"))
	default:
		problem(w, r, http.StatusBadRequest, "invalid_json", "Request body must be a single JSON object with the documented fields.")
	}
	return false
}

func jsonKind(goKind string) string {
	switch goKind {
	case "int64", "int", "int32":
		return "integer"
	case "slice":
		return "array"
	case "ptr", "string":
		return "string"
	default:
		return goKind
	}
}

// cleanText reports whether s is valid UTF-8 without control characters.
func cleanText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, unicode.IsControl) < 0
}

// cleanMultiline is cleanText that also allows line breaks and tabs.
func cleanMultiline(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) < 0
}

// ---------------------------------------------------------------------------
// Path and query parameters
// ---------------------------------------------------------------------------

// pathParam returns a decoded path parameter. chi matches on r.URL.RawPath
// when it is set (the path had escapes that Path cannot represent, such as
// %2F) and on r.URL.Path otherwise, so the parameter is escaped exactly when
// RawPath is set.
//
//nolint:unused // TODO(gateway-core): kept for path parameters that are not UUIDs.
func pathParam(r *http.Request, name string) (string, bool) {
	v := chi.URLParam(r, name)
	if r.URL.RawPath == "" {
		return v, true
	}
	out, err := url.PathUnescape(v)
	return out, err == nil
}

func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
		return uuid.UUID{}, false
	}
	return id, true
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

func queryLimit(q url.Values) (int, *httpx.FieldError) {
	v := q.Get("limit")
	if v == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxLimit {
		fe := fieldErr("limit", "must be an integer from 1 to 200")
		return 0, &fe
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Cursors
// ---------------------------------------------------------------------------

// A cursor is opaque to clients: base64url(version | kind | int64 | uuid).
// The int64 is created_at in Unix microseconds (lists) or the writing
// transaction id (events). Cursors are only positions inside the caller's
// own app (every query is app-scoped), so a forged cursor can at most
// skip the caller's own rows; parsing still rejects anything malformed.
type cursorKind byte

const (
	cursorVersion                  = 1
	cursorCharges       cursorKind = 'c'
	cursorTransactions  cursorKind = 't'
	cursorEvents        cursorKind = 'e'
	cursorLen                      = 2 + 8 + 16
	maxCursorTextLength            = 200
)

var errBadCursor = errors.New("invalid cursor")

func encodeCursor(kind cursorKind, n int64, id uuid.UUID) string {
	var b [cursorLen]byte
	b[0], b[1] = cursorVersion, byte(kind)
	binary.BigEndian.PutUint64(b[2:10], uint64(n))
	copy(b[10:], id[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func decodeCursor(kind cursorKind, s string) (int64, uuid.UUID, error) {
	if len(s) > maxCursorTextLength {
		return 0, uuid.Nil, errBadCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != cursorLen || b[0] != cursorVersion || cursorKind(b[1]) != kind {
		return 0, uuid.Nil, errBadCursor
	}
	n := binary.BigEndian.Uint64(b[2:10])
	if n > 1<<62 {
		return 0, uuid.Nil, errBadCursor
	}
	id, err := uuid.FromBytes(b[10:])
	if err != nil {
		return 0, uuid.Nil, errBadCursor
	}
	return int64(n), id, nil
}
