// Package stk runs M-Pesa collections end to end: sending STK push prompts,
// storing and processing Daraja callbacks (STK and C2B), checking on pushes
// whose callback is late, and letting staff resolve payments Safaricom
// confirmed without a receipt.
//
// Money rules (gateway plan, approved 2026-11-25):
//   - A transaction becomes succeeded (and pays its charge) ONLY with an
//     M-Pesa receipt from a callback, or a receipt entered by finance/admin
//     staff.
//   - An STK query that says "success" never records money: the push goes
//     to "unknown" and its transaction to needs_review.
//   - An STK push always asks for the charge's exact amount (at most
//     mpesa.MaxAmount). A failed prompt does not fail the charge.
//   - C2B money is routed to an app by account-number prefix; it pays a
//     live charge with the same account number and exact amount, else it is
//     forwarded as payment.received. An unknown prefix is recorded
//     unmatched. Money for an inactive app is still recorded, with a warning.
//   - Each receipt is recorded once (DB unique constraint); processing is
//     idempotent, so callbacks and jobs may be retried freely.
package stk

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Errors returned by Send (mapped to stable API codes by the HTTP layers).
var (
	ErrUnavailable         = mpesa.ErrUnavailable                                                       // 503 mpesa_unavailable
	ErrNotFound            = errors.New("stk: not found")                                               // 404 not_found
	ErrNotPayable          = errors.New("stk: charge is not payable")                                   // 409 not_payable
	ErrAmountTooLarge      = errors.New("stk: charge amount is above the M-Pesa limit; pay by paybill") // 409 amount_too_large
	ErrIdempotencyConflict = errors.New("stk: idempotency key reused with a different phone or charge") // 409 idempotency_conflict
	ErrInvalidIdempotency  = errors.New("stk: idempotency key must be 1-255 printable characters")      // 422 validation_failed
	ErrDaraja              = errors.New("stk: Safaricom did not accept the prompt")                     // 502 mpesa_error
	ErrNotResolvable       = errors.New("stk: only needs_review M-Pesa transactions can be resolved")   // 409 not_resolvable
	ErrReceiptTaken        = errors.New("stk: this receipt is already recorded on another transaction") // 409 receipt_taken
	ErrInvalidReceipt      = errors.New("stk: receipt must be 1-64 letters or digits")                  // 422 validation_failed
	ErrReasonRequired      = errors.New("stk: a reason of at least 3 characters is required")           // 422 validation_failed
	ErrInvalidCallback     = errors.New("stk: callback body must be a JSON object of at most 16 KiB")   // 400
	// ErrInvalidPhone is phone.ErrInvalid (422 validation_failed, field "phone").
)

// InProgressError: a push for this charge is already live (409
// stk_in_progress). LiveID is that push.
type InProgressError struct{ LiveID uuid.UUID }

func (e *InProgressError) Error() string {
	return "stk: a prompt for this charge is already in progress"
}

// Source says who asked for a push.
type Source string

const (
	SourceApp     Source = "app"      // POST /v1/charges/{id}/stk-push
	SourcePayPage Source = "pay_page" // hosted pay page
)

// SendParams is one push request.
type SendParams struct {
	AppID          uuid.UUID
	ChargeID       uuid.UUID
	Phone          string // raw, normalised by Send
	Source         Source
	IdempotencyKey string // required for SourceApp, empty for SourcePayPage
}

// Push is a stored STK push.
type Push = store.StkPush

// CallbackEnqueuer inserts the processing job for a stored callback inside
// tx (queue.Enqueuer implements it).
type CallbackEnqueuer interface {
	EnqueueCallback(ctx context.Context, tx pgx.Tx, callbackID uuid.UUID) error
}

// Options configure the service.
type Options struct {
	Pool   *db.Pool
	Client mpesa.Client // nil: M-Pesa off (Send returns ErrUnavailable)
	Ledger *ledger.Service
	Events *events.Emitter
	// Callbacks enqueues processing jobs; required to store callbacks.
	Callbacks CallbackEnqueuer
	Logger    *slog.Logger
	// QueryAfter: first status check this long after a push was sent with
	// no callback. GiveUpAfter: a push with no definite answer by then goes
	// to staff review. InitiatingTimeout: a push stuck before Daraja
	// answered (crash) is failed after this long.
	QueryAfter        time.Duration
	GiveUpAfter       time.Duration
	InitiatingTimeout time.Duration
	// Now is the clock (tests). nil = time.Now.
	Now func() time.Time
	// ShortCode is the configured paybill. A C2B confirmation for any other
	// BusinessShortCode is not recorded (alarm). Empty: not checked.
	ShortCode string
	// SweepLease: how long a sweeper holds a due push while it asks
	// Daraja (default 2 minutes). SweepConcurrency bounds parallel status
	// queries per sweep (default 8).
	SweepLease       time.Duration
	SweepConcurrency int
}

// Service is the STK / callback service. Safe for concurrent use.
type Service struct {
	o Options
}

// NewService builds the service.
func NewService(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.QueryAfter <= 0 {
		o.QueryAfter = time.Minute
	}
	if o.GiveUpAfter <= 0 {
		o.GiveUpAfter = 30 * time.Minute
	}
	if o.InitiatingTimeout <= 0 {
		o.InitiatingTimeout = 2 * time.Minute
	}
	if o.SweepLease <= 0 {
		o.SweepLease = 2 * time.Minute
	}
	if o.SweepConcurrency <= 0 {
		o.SweepConcurrency = 8
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{o: o}
}

// Available reports whether pushes can be sent at all.
func (s *Service) Available() bool { return s.o.Client != nil }
