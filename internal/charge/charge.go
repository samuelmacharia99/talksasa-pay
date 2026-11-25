// Package charge manages charges: an app's request to collect an exact
// amount ("collect KES X from this phone for my reference Y"). Amounts are
// always set here, server-side, from the app's API call; nothing a
// customer's browser or phone sends can change them.
//
// Money rules (gateway plan, approved 2026-11-25):
//  1. amount is whole KES, 1 to 250,000, and never changes (cancel and
//     create a new charge instead); reference is the app's own, unique per
//     app (duplicate -> ErrDuplicateReference, 409); metadata is a JSON
//     object of at most 4 KB, echoed back; expires_in is 15 min to 7 days
//     (default 24 h).
//  2. every charge has an account number: the app's own (must start with
//     its prefix, accountref.Validate) or generated (accountref.Generate).
//  4. a failed prompt does not fail the charge.
//
// Paying a charge is ledger.SucceedCharge's job (internal/ledger), driven by
// M-Pesa callbacks (internal/stk); nothing here records money.
package charge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Limits (money rule 1).
const (
	MinAmount        = 1
	MaxAmount        = 250_000
	MaxReferenceLen  = 128
	MaxDescription   = 500
	MaxMetadataBytes = 4096
	MinExpiresIn     = 15 * time.Minute
	MaxExpiresIn     = 7 * 24 * time.Hour
	DefaultExpiresIn = 24 * time.Hour
)

// Errors (mapped to stable API problem codes by internal/appapi).
var (
	ErrNotFound             = errors.New("charge: not found")                                                     // 404 not_found
	ErrDuplicateReference   = errors.New("charge: a charge with this reference already exists")                   // 409 duplicate_reference
	ErrNotCancellable       = errors.New("charge: only pending charges can be cancelled")                         // 409 not_cancellable
	ErrIdempotencyConflict  = errors.New("charge: idempotency key reused with different parameters")              // 409 idempotency_conflict
	ErrAccountNumberInUse   = errors.New("charge: a live charge already uses this generated account number")      // retried internally
	ErrAppNotActive         = errors.New("charge: app is not active")                                             // 403 app_suspended
	ErrInvalidAmount        = errors.New("charge: amount must be a whole number of KES from 1 to 250000")         // 422 amount
	ErrInvalidReference     = errors.New("charge: reference must be 1-128 characters")                            // 422 reference
	ErrInvalidDescription   = errors.New("charge: description must be 1-500 characters")                          // 422 description
	ErrInvalidMetadata      = errors.New("charge: metadata must be a JSON object of at most 4096 bytes")          // 422 metadata
	ErrInvalidAccountNumber = errors.New("charge: account number must be PREFIX-, a digit, then A-Z/0-9, max 12") // 422 account_number
	ErrInvalidPhone         = errors.New("charge: phone must be a Kenyan Safaricom number")                       // 422 phone
	ErrInvalidExpiry        = errors.New("charge: expires_in must be from 900 to 604800 seconds")                 // 422 expires_in
	ErrInvalidIdempotency   = errors.New("charge: idempotency key must be 1-255 printable characters")            // 400 Idempotency-Key
	ErrInvalidCursor        = errors.New("charge: invalid list cursor")                                           // 400 cursor
	ErrInvalidStatus        = errors.New("charge: unknown status filter")                                         // 400 status
	errNotImplemented       = errors.New("charge: not implemented yet (gateway-core)")
)

// ErrNotImplemented reports whether err is the gateway-core stub sentinel
// (the API answers 501 until the service lands).
// TODO(gateway-core): delete once every method below is implemented.
func ErrNotImplemented(err error) bool { return errors.Is(err, errNotImplemented) }

// Charge is a stored charge.
type Charge = store.Charge

// Push is a stored STK push.
type Push = store.StkPush

// CreateParams are the app-supplied fields of POST /v1/charges.
type CreateParams struct {
	Reference     string
	Amount        int64
	Description   string          // optional
	Metadata      json.RawMessage // optional JSON object, <= 4 KB
	AccountNumber string          // optional; generated when empty
	Phone         string          // optional; when set, the first prompt is sent there
	ExpiresIn     time.Duration   // 0 = DefaultExpiresIn
	// IdempotencyKey is the Idempotency-Key header (required). The same key
	// with the same parameters replays the original charge.
	IdempotencyKey string
}

// Created is the outcome of Create.
type Created struct {
	Charge   Charge
	Replayed bool // same idempotency key and parameters: the original charge
	// Push is the first prompt when a phone was given (nil otherwise, or
	// when sending failed before a push was stored).
	Push *Push
	// PushErr is why the first prompt could not be sent (the charge itself
	// was still created and can be prompted again or paid by paybill).
	PushErr error
}

// ListParams filter GET /v1/charges. Cursor paging is by (created_at, id)
// descending.
type ListParams struct {
	Status    *store.ChargeStatus
	Reference string
	CursorAt  *time.Time
	CursorID  *uuid.UUID
	Limit     int // 1-100
}

// Cancelled is the outcome of Cancel.
type Cancelled struct {
	Charge Charge
	// AlreadyCancelled: an idempotent retry; nothing changed.
	AlreadyCancelled bool
}

// Prompter sends STK prompts (*stk.Service).
type Prompter interface {
	Send(ctx context.Context, actor audit.Actor, p stk.SendParams) (stk.Push, bool, error)
}

var _ Prompter = (*stk.Service)(nil)

// Options configure the service.
type Options struct {
	Pool   *db.Pool
	Events *events.Emitter
	// STK sends the first prompt when a charge has a phone. nil: M-Pesa off
	// (the charge is created; Created.PushErr is stk.ErrUnavailable).
	STK    Prompter
	Logger *slog.Logger
	Now    func() time.Time // nil = time.Now
}

// Service manages charges. Safe for concurrent use.
type Service struct {
	o Options
}

// NewService builds the service.
func NewService(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{o: o}
}

// Create creates a charge (idempotent on app + IdempotencyKey) and, when a
// phone is given, sends the first prompt after the charge commits.
func (s *Service) Create(ctx context.Context, actor audit.Actor, appID uuid.UUID, p CreateParams) (Created, error) {
	// TODO(gateway-core): validate (rules 1-2), request hash, InsertCharge
	// with a generated hosted_token, retry a generated account number on
	// charges_generated_account_live, ErrDuplicateReference on
	// charges_app_id_reference_key, replay/ErrIdempotencyConflict, audit,
	// then STK.Send when Phone is set.
	return Created{}, errNotImplemented
}

// Get returns an app's charge.
func (s *Service) Get(ctx context.Context, appID, id uuid.UUID) (Charge, error) {
	// TODO(gateway-core): GetChargeForApp; db.IsNotFound -> ErrNotFound.
	return Charge{}, errNotImplemented
}

// List returns an app's charges, newest first (Limit+1 rows are fetched by
// callers that need a next cursor).
func (s *Service) List(ctx context.Context, appID uuid.UUID, p ListParams) ([]Charge, error) {
	// TODO(gateway-core): ListChargesForApp.
	return nil, errNotImplemented
}

// Cancel cancels a pending charge and emits charge.cancelled. A cancelled
// charge can still be paid by late money (rule 6).
func (s *Service) Cancel(ctx context.Context, actor audit.Actor, appID, id uuid.UUID) (Cancelled, error) {
	// TODO(gateway-core): LockCharge, CancelCharge, emit charge.cancelled,
	// audit; already cancelled -> AlreadyCancelled; else ErrNotCancellable.
	return Cancelled{}, errNotImplemented
}

// Expire expires up to batch due pending charges (charge expiry job) and
// emits charge.expired for each. Returns how many expired.
func (s *Service) Expire(ctx context.Context, batch int) (int, error) {
	// TODO(gateway-core): ExpireDueCharges in one tx + emit charge.expired.
	return 0, errNotImplemented
}
