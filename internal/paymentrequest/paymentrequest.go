// Package paymentrequest manages what a product asks Talksasa Pay to collect.
// Amounts are always set here, server-side, from the product's API call;
// nothing a customer's browser or phone sends can change them.
package paymentrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	ErrNotFound            = errors.New("payment request: not found")
	ErrPayerNotFound       = errors.New("payment request: payer not found")
	ErrPayerClosed         = errors.New("payment request: payer account is closed")
	ErrIdempotencyConflict = errors.New("payment request: idempotency key reused with different parameters")
	ErrDuplicateReference  = errors.New("payment request: an open request with this product reference already exists")
	ErrInvalidReference    = errors.New("payment request: product reference must be 1-128 characters")
	ErrInvalidDescription  = errors.New("payment request: description must be 1-500 characters")
	ErrInvalidIdempotency  = errors.New("payment request: idempotency key must be 1-255 printable characters")
	ErrInvalidExpiry       = errors.New("payment request: expiry must be in the future and within 1 year")
	ErrNotCancellable      = errors.New("payment request: only open requests with nothing paid can be cancelled")
)

// MaxExpiry bounds how far ahead a request may expire.
const MaxExpiry = 366 * 24 * time.Hour

// Request is the service-level view of a payment request.
type Request struct {
	ID               uuid.UUID
	ProductID        uuid.UUID
	PayerAccountID   uuid.UUID
	ProductReference string
	Description      string
	Amount           money.KES
	AmountPaid       money.KES
	Status           allocation.Status
	ExpiresAt        *time.Time
	HostedToken      string
	CreatedAt        time.Time
	PaidAt           *time.Time
}

// Outstanding is what is still owed.
func (r Request) Outstanding() money.KES { return r.Amount - r.AmountPaid }

// CreateParams are the product-supplied fields.
type CreateParams struct {
	PayerAccountID   uuid.UUID
	ProductReference string
	Description      string
	Amount           int64
	ExpiresAt        *time.Time
	IdempotencyKey   string
}

// Created is the outcome of Create.
type Created struct {
	Request       Request
	Replayed      bool              // same idempotency key and parameters: the original request is returned
	CreditApplied allocation.Result // non-zero BatchID if existing credit was applied
}

// Service manages payment requests.
type Service struct {
	pool   *db.Pool
	alloc  *allocation.Service
	logger *slog.Logger
	now    func() time.Time
}

// NewService builds a payment request service.
func NewService(pool *db.Pool, alloc *allocation.Service, logger *slog.Logger) *Service {
	return &Service{pool: pool, alloc: alloc, logger: logger, now: time.Now}
}

func (s *Service) validate(p *CreateParams) error {
	p.ProductReference = strings.TrimSpace(p.ProductReference)
	p.Description = strings.TrimSpace(p.Description)
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	if _, err := money.Positive(p.Amount); err != nil {
		return err
	}
	if p.ProductReference == "" || len(p.ProductReference) > 128 {
		return ErrInvalidReference
	}
	if p.Description == "" || len(p.Description) > 500 {
		return ErrInvalidDescription
	}
	if p.IdempotencyKey == "" || len(p.IdempotencyKey) > 255 || strings.IndexFunc(p.IdempotencyKey, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return ErrInvalidIdempotency
	}
	if p.ExpiresAt != nil {
		now := s.now()
		if !p.ExpiresAt.After(now) || p.ExpiresAt.Sub(now) > MaxExpiry {
			return ErrInvalidExpiry
		}
		t := p.ExpiresAt.UTC().Truncate(time.Microsecond) // Postgres precision, so replays compare equal
		p.ExpiresAt = &t
	}
	return nil
}

// Create creates a payment request and immediately applies any credit the
// payer already holds. It is idempotent on (product, idempotency key).
func (s *Service) Create(ctx context.Context, actor audit.Actor, productID uuid.UUID, p CreateParams) (Created, error) {
	if err := s.validate(&p); err != nil {
		return Created{}, err
	}
	var out Created
	var err error
	// A concurrent duplicate loses on the unique index; retry once so it
	// takes the replay path and returns the winner's request.
	for attempt := 0; attempt < 2; attempt++ {
		out = Created{}
		err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
			return s.createTx(ctx, q, actor, productID, p, &out)
		})
		if err == nil || !db.IsUniqueViolation(err, "payment_requests_product_id_idempotency_key_key") {
			break
		}
	}
	if err != nil {
		return Created{}, err
	}
	if !out.Replayed {
		s.logger.InfoContext(ctx, "payment request created",
			slog.String("product_id", productID.String()), slog.String("payment_request_id", out.Request.ID.String()),
			slog.Int64("amount", int64(out.Request.Amount)), slog.Int64("credit_applied", int64(-out.CreditApplied.CreditDelta)))
	}
	return out, nil
}

func (s *Service) createTx(ctx context.Context, q *store.Queries, actor audit.Actor, productID uuid.UUID, p CreateParams, out *Created) error {
	existing, err := q.GetPaymentRequestByIdempotencyKey(ctx, store.GetPaymentRequestByIdempotencyKeyParams{
		ProductID: productID, IdempotencyKey: p.IdempotencyKey,
	})
	switch {
	case err == nil:
		if !sameParams(existing, p) {
			return ErrIdempotencyConflict
		}
		out.Request, out.Replayed = fromRow(existing), true
		return nil
	case !db.IsNotFound(err):
		return err
	}

	payer, err := q.GetPayer(ctx, store.GetPayerParams{ProductID: productID, ID: p.PayerAccountID})
	if db.IsNotFound(err) {
		return ErrPayerNotFound
	}
	if err != nil {
		return err
	}
	if payer.Status != store.PayerStatusActive {
		return ErrPayerClosed
	}

	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	token, err := crypto.RandomToken(32)
	if err != nil {
		return err
	}
	_, err = q.CreatePaymentRequest(ctx, store.CreatePaymentRequestParams{
		ID: id, ProductID: productID, PayerAccountID: payer.ID, ProductReference: p.ProductReference,
		Description: p.Description, Amount: p.Amount, ExpiresAt: p.ExpiresAt, HostedToken: token,
		IdempotencyKey: p.IdempotencyKey,
	})
	if db.IsUniqueViolation(err, "payment_requests_live_reference") {
		return ErrDuplicateReference
	}
	if err != nil {
		return fmt.Errorf("payment request: insert: %w", err)
	}

	out.CreditApplied, err = s.alloc.ApplyCredit(ctx, q, actor, productID, id)
	if err != nil {
		return fmt.Errorf("payment request: apply credit: %w", err)
	}
	row, err := q.GetPaymentRequest(ctx, store.GetPaymentRequestParams{ProductID: productID, ID: id})
	if err != nil {
		return err
	}
	out.Request = fromRow(row)
	return nil
}

// Get returns a request of the given product.
func (s *Service) Get(ctx context.Context, productID, id uuid.UUID) (Request, error) {
	row, err := store.New(s.pool).GetPaymentRequest(ctx, store.GetPaymentRequestParams{ProductID: productID, ID: id})
	if db.IsNotFound(err) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, err
	}
	return fromRow(row), nil
}

// Cancel closes an open request on which nothing has been paid. A partially
// paid request cannot be cancelled: the money already allocated would need a
// refund or a move to credit, which is an admin decision.
func (s *Service) Cancel(ctx context.Context, actor audit.Actor, productID, id uuid.UUID) (Request, error) {
	var out Request
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		row, err := q.CancelPaymentRequest(ctx, store.CancelPaymentRequestParams{ProductID: productID, ID: id})
		if db.IsNotFound(err) {
			if _, gerr := q.GetPaymentRequest(ctx, store.GetPaymentRequestParams{ProductID: productID, ID: id}); db.IsNotFound(gerr) {
				return ErrNotFound
			}
			return ErrNotCancellable
		}
		if err != nil {
			return err
		}
		out = fromRow(row)
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "payment_request.cancelled", EntityType: "payment_request", EntityID: id.String(), ProductID: &productID,
		})
	})
	return out, err
}

func sameParams(r store.PaymentRequest, p CreateParams) bool {
	sameExpiry := (r.ExpiresAt == nil && p.ExpiresAt == nil) ||
		(r.ExpiresAt != nil && p.ExpiresAt != nil && r.ExpiresAt.Equal(*p.ExpiresAt))
	return r.PayerAccountID == p.PayerAccountID && r.ProductReference == p.ProductReference &&
		r.Description == p.Description && r.Amount == p.Amount && sameExpiry
}

func fromRow(r store.PaymentRequest) Request {
	return Request{
		ID: r.ID, ProductID: r.ProductID, PayerAccountID: r.PayerAccountID, ProductReference: r.ProductReference,
		Description: r.Description, Amount: money.KES(r.Amount), AmountPaid: money.KES(r.AmountPaid),
		Status: allocation.Status(r.Status), ExpiresAt: r.ExpiresAt, HostedToken: r.HostedToken,
		CreatedAt: r.CreatedAt, PaidAt: r.PaidAt,
	}
}
