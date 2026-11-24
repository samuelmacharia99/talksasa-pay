// Package stk runs M-Pesa collections end to end: sending STK push prompts,
// storing and processing Daraja callbacks (STK and C2B), checking on pushes
// whose callback is late, and letting staff resolve payments Safaricom
// confirmed without a receipt.
//
// Money rules (confirmed 2026-11-24, see the Phase 2 plan):
//   - A transaction becomes succeeded (and is allocated) ONLY with an M-Pesa
//     receipt from a callback, or a receipt entered by finance/admin staff.
//   - An STK query that says "success" never records money: the push goes
//     to "unknown" and its transaction to needs_review.
//   - An STK push always asks for the request's full outstanding balance
//     (at most mpesa.MaxAmount).
//   - C2B money to an unknown account number is recorded unmatched
//     (suspense). Money for an inactive product/payer is still allocated,
//     with a warning.
//   - Each receipt is recorded once (DB unique constraint); processing is
//     idempotent, so callbacks and jobs may be retried freely.
package stk

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Errors returned by Send (mapped to stable API codes by the HTTP layers).
var (
	ErrUnavailable         = mpesa.ErrUnavailable                                                             // 503 mpesa_unavailable
	ErrNotFound            = errors.New("stk: not found")                                                     // 404 not_found
	ErrNotPayable          = errors.New("stk: payment request is not payable")                                // 409 not_payable
	ErrAmountTooLarge      = errors.New("stk: outstanding balance is above the M-Pesa limit; pay by paybill") // 409 amount_too_large
	ErrIdempotencyConflict = errors.New("stk: idempotency key reused with a different phone or request")      // 409 idempotency_conflict
	ErrInvalidIdempotency  = errors.New("stk: idempotency key must be 1-255 printable characters")            // 422 validation_failed
	ErrDaraja              = errors.New("stk: Safaricom did not accept the prompt")                           // 502 mpesa_error
	ErrNotResolvable       = errors.New("stk: only needs_review M-Pesa transactions can be resolved")         // 409 not_resolvable
	ErrReceiptTaken        = errors.New("stk: this receipt is already recorded on another transaction")       // 409 receipt_taken
	ErrInvalidReceipt      = errors.New("stk: receipt must be 1-64 letters or digits")                        // 422 validation_failed
	ErrReasonRequired      = errors.New("stk: a reason of at least 3 characters is required")                 // 422 validation_failed
	// ErrInvalidPhone is phone.ErrInvalid (422 validation_failed, field "phone").
)

// InProgressError: a push for this request is already live (409
// stk_in_progress). LiveID is that push.
type InProgressError struct{ LiveID uuid.UUID }

func (e *InProgressError) Error() string {
	return "stk: a prompt for this request is already in progress"
}

// Source says who asked for a push.
type Source string

const (
	SourceProduct Source = "product"  // POST /v1/payment-requests/{id}/stk-push
	SourcePayPage Source = "pay_page" // hosted pay page
)

// SendParams is one push request.
type SendParams struct {
	ProductID      uuid.UUID
	RequestID      uuid.UUID
	Phone          string // raw, normalised by Send
	Source         Source
	IdempotencyKey string // required for SourceProduct, empty for SourcePayPage
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
	Alloc  *allocation.Service
	Payers *payer.Service
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
	return &Service{o: o}
}

// Available reports whether pushes can be sent at all.
func (s *Service) Available() bool { return s.o.Client != nil }

// Send sends a prompt for the request's full outstanding balance.
//
//  1. Tx A: lock the request; check payable, amount <= mpesa.MaxAmount, no
//     live push; insert the pending mpesa_stk transaction and the push
//     (initiating). Replays (same product + idempotency key) return the
//     existing push with replayed=true.
//  2. Call Daraja OUTSIDE any transaction.
//  3. Tx B: accepted -> push sent (+ checkout ids, next_query_at), emit
//     stk_push.sent; rejected -> push failed, transaction failed, emit
//     stk_push.failed, return the push with ErrDaraja; transient (outcome
//     unknown) -> leave initiating for the sweeper, return the push.
func (s *Service) Send(ctx context.Context, actor audit.Actor, p SendParams) (push Push, replayed bool, err error) {
	return Push{}, false, errors.New("stk: Send not implemented")
}

// Get returns a product's push.
func (s *Service) Get(ctx context.Context, productID, id uuid.UUID) (Push, error) {
	return Push{}, errors.New("stk: Get not implemented")
}

// LatestForRequest returns the newest push for a request created after
// since, or nil (pay page).
func (s *Service) LatestForRequest(ctx context.Context, requestID uuid.UUID, since time.Time) (*Push, error) {
	return nil, errors.New("stk: LatestForRequest not implemented")
}

// ListForRequest lists a request's pushes, newest first (admin).
func (s *Service) ListForRequest(ctx context.Context, requestID uuid.UUID, limit int) ([]Push, error) {
	return nil, errors.New("stk: ListForRequest not implemented")
}

// CallbackKind is what a callback carries.
type CallbackKind = store.MpesaCallbackKind

// StoreCallback stores a callback body verbatim and enqueues its processing
// job in the same transaction. Called by the /cb HTTP handler BEFORE it
// acknowledges Safaricom. raw must be a JSON object (<= 16 KiB).
func (s *Service) StoreCallback(ctx context.Context, kind CallbackKind, remoteIP netip.Addr, raw []byte) (uuid.UUID, error) {
	return uuid.Nil, errors.New("stk: StoreCallback not implemented")
}

// ProcessCallback processes one stored callback (worker job). Idempotent:
// an already-processed callback is a no-op.
//
// STK success: complete the push's pending transaction with the receipt and
// allocate it (target request first); amount mismatch or a receipt already
// on another row -> needs_review + alarm; unknown CheckoutRequestID -> match
// a single orphan push by phone+amount, else record unmatched. STK failure:
// fail the push and its transaction (a late failure after "unknown" also
// fails it). C2B: RecordAndAllocate routed by BillRef (unmatched ->
// suspense; inactive -> still allocated, warning).
func (s *Service) ProcessCallback(ctx context.Context, callbackID uuid.UUID) error {
	return errors.New("stk: ProcessCallback not implemented")
}

// SweepResult counts what one sweep did.
type SweepResult struct {
	Queried, Failed, NeedsReview, Rescheduled int
}

// Sweep checks due live pushes (worker periodic job): initiating past
// InitiatingTimeout -> failed; sent and due -> STK query: succeeded ->
// unknown + needs_review; failed -> failed; pending -> reschedule with
// backoff, or needs_review past GiveUpAfter.
func (s *Service) Sweep(ctx context.Context, limit int) (SweepResult, error) {
	return SweepResult{}, errors.New("stk: Sweep not implemented")
}

// ResolveParams is a staff decision on a needs_review M-Pesa transaction.
type ResolveParams struct {
	TransactionID uuid.UUID
	// Confirm with the receipt from the M-Pesa portal (marks it succeeded
	// and allocates it), or Fail (no money arrived).
	Confirm bool
	Receipt string
	Reason  string
}

// Resolve applies a staff decision (finance or admin; audited). Returns the
// updated transaction.
func (s *Service) Resolve(ctx context.Context, actor audit.Actor, p ResolveParams) (store.Transaction, error) {
	return store.Transaction{}, errors.New("stk: Resolve not implemented")
}
