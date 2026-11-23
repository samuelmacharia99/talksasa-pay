// Package payer manages payer accounts: the product's customers or tenants
// as Talksasa Pay sees them. Each payer gets a permanent account number
// (e.g. HOST-4471) that customers type when paying from the M-Pesa menu.
package payer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	ErrNotFound           = errors.New("payer: not found")
	ErrInvalidExternalRef = errors.New("payer: external_ref must be 1-128 characters")
	ErrInvalidName        = errors.New("payer: display name must be 1-200 characters")
	ErrInvalidEmail       = errors.New("payer: invalid email address")
	ErrTooManyPhones      = errors.New("payer: at most 5 phone numbers")
	ErrProductNotActive   = errors.New("payer: product is not active")
)

const maxPhones = 5

// Payer is the service-level view of a payer account.
type Payer struct {
	ID            uuid.UUID
	ProductID     uuid.UUID
	ExternalRef   string
	AccountNumber string
	DisplayName   string
	Email         string
	Phones        []string // normalised; first is primary
	Status        store.PayerStatus
	CreditBalance money.KES
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UpsertParams creates or updates a payer identified by (product, external_ref).
type UpsertParams struct {
	ExternalRef string
	DisplayName string
	Email       string
	Phones      []string // raw; normalised here. First becomes primary.
}

// Service manages payer accounts.
type Service struct {
	pool   *db.Pool
	logger *slog.Logger
}

// NewService builds a payer service.
func NewService(pool *db.Pool, logger *slog.Logger) *Service {
	return &Service{pool: pool, logger: logger}
}

type validated struct {
	externalRef, name string
	email             *string
	phones            []string
}

func validate(p UpsertParams) (validated, error) {
	v := validated{externalRef: strings.TrimSpace(p.ExternalRef), name: strings.TrimSpace(p.DisplayName)}
	if v.externalRef == "" || len(v.externalRef) > 128 {
		return v, ErrInvalidExternalRef
	}
	if v.name == "" || len(v.name) > 200 {
		return v, ErrInvalidName
	}
	if e := strings.TrimSpace(p.Email); e != "" {
		addr, err := mail.ParseAddress(e)
		if err != nil || addr.Address != e || len(e) > 254 || !strings.Contains(e[strings.LastIndexByte(e, '@'):], ".") {
			return v, ErrInvalidEmail
		}
		lower := strings.ToLower(e)
		v.email = &lower
	}
	if len(p.Phones) > maxPhones {
		return v, ErrTooManyPhones
	}
	seen := map[string]bool{}
	for _, raw := range p.Phones {
		n, err := phone.Normalize(raw)
		if err != nil {
			return v, err
		}
		if !seen[n] {
			seen[n] = true
			v.phones = append(v.phones, n)
		}
	}
	return v, nil
}

// Upsert creates the payer (issuing a new account number) or updates its
// profile. The account number never changes once issued. created reports
// whether a new account was created.
func (s *Service) Upsert(ctx context.Context, actor audit.Actor, productID uuid.UUID, p UpsertParams) (payer Payer, created bool, err error) {
	v, err := validate(p)
	if err != nil {
		return Payer{}, false, err
	}
	// Two concurrent first-time upserts for the same external_ref race on the
	// unique index; the loser retries once and takes the update path.
	for attempt := 0; attempt < 2; attempt++ {
		err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
			payer, created, err = s.upsertTx(ctx, q, actor, productID, v)
			return err
		})
		if err == nil || !db.IsUniqueViolation(err, "payer_accounts_product_id_external_ref_key") {
			break
		}
	}
	if err != nil {
		return Payer{}, false, err
	}
	if created {
		s.logger.InfoContext(ctx, "payer created", slog.String("product_id", productID.String()),
			slog.String("payer_id", payer.ID.String()), slog.String("account_number", payer.AccountNumber))
	}
	return payer, created, nil
}

func (s *Service) upsertTx(ctx context.Context, q *store.Queries, actor audit.Actor, productID uuid.UUID, v validated) (Payer, bool, error) {
	prod, err := q.GetProduct(ctx, productID)
	if err != nil {
		return Payer{}, false, fmt.Errorf("payer: load product: %w", err)
	}
	if prod.Status != store.ProductStatusActive {
		return Payer{}, false, ErrProductNotActive
	}

	row, err := q.GetPayerByExternalRef(ctx, store.GetPayerByExternalRefParams{ProductID: productID, ExternalRef: v.externalRef})
	created := false
	switch {
	case db.IsNotFound(err):
		base, err := q.NextAccountBase(ctx, productID)
		if err != nil {
			return Payer{}, false, fmt.Errorf("payer: next account number: %w", err)
		}
		acct, err := FormatAccountNumber(prod.AccountPrefix, base)
		if err != nil {
			return Payer{}, false, err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return Payer{}, false, err
		}
		row, err = q.CreatePayer(ctx, store.CreatePayerParams{
			ID: id, ProductID: productID, ExternalRef: v.externalRef, AccountNumber: acct,
			AccountNumberNorm: NormalizeAccountNumber(acct), DisplayName: v.name, Email: v.email,
		})
		if err != nil {
			return Payer{}, false, err
		}
		created = true
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "payer.created", EntityType: "payer", EntityID: id.String(), ProductID: &productID,
			Details: map[string]any{"account_number": acct, "external_ref": v.externalRef},
		}); err != nil {
			return Payer{}, false, err
		}
	case err != nil:
		return Payer{}, false, fmt.Errorf("payer: lookup: %w", err)
	default:
		if row.DisplayName != v.name || !strPtrEq(row.Email, v.email) {
			row, err = q.UpdatePayerProfile(ctx, store.UpdatePayerProfileParams{
				ProductID: productID, ID: row.ID, DisplayName: v.name, Email: v.email,
			})
			if err != nil {
				return Payer{}, false, err
			}
		}
	}

	if err := s.syncPhones(ctx, q, productID, row.ID, v.phones); err != nil {
		return Payer{}, false, err
	}
	phones, err := q.ListPayerPhones(ctx, store.ListPayerPhonesParams{ProductID: productID, PayerAccountID: row.ID})
	if err != nil {
		return Payer{}, false, err
	}
	return fromRow(row, phones), created, nil
}

// syncPhones makes the stored phone set equal the given list; the first is primary.
func (s *Service) syncPhones(ctx context.Context, q *store.Queries, productID, payerID uuid.UUID, phones []string) error {
	keep := phones
	if keep == nil {
		keep = []string{}
	}
	if err := q.DeletePayerPhonesExcept(ctx, store.DeletePayerPhonesExceptParams{ProductID: productID, PayerAccountID: payerID, Keep: keep}); err != nil {
		return err
	}
	if err := q.ClearPrimaryPhone(ctx, store.ClearPrimaryPhoneParams{ProductID: productID, PayerAccountID: payerID}); err != nil {
		return err
	}
	for i, ph := range phones {
		if err := q.UpsertPayerPhone(ctx, store.UpsertPayerPhoneParams{
			ProductID: productID, PayerAccountID: payerID, Phone: ph, IsPrimary: i == 0,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Get returns a payer of the given product.
func (s *Service) Get(ctx context.Context, productID, id uuid.UUID) (Payer, error) {
	q := store.New(s.pool)
	row, err := q.GetPayer(ctx, store.GetPayerParams{ProductID: productID, ID: id})
	if db.IsNotFound(err) {
		return Payer{}, ErrNotFound
	}
	if err != nil {
		return Payer{}, err
	}
	phones, err := q.ListPayerPhones(ctx, store.ListPayerPhonesParams{ProductID: productID, PayerAccountID: id})
	if err != nil {
		return Payer{}, err
	}
	return fromRow(row, phones), nil
}

// RouteResult says which product and payer an account reference belongs to.
type RouteResult struct {
	PayerID       uuid.UUID
	ProductID     uuid.UUID
	AccountNumber string
	Active        bool // payer and product both active
}

// Route resolves what a customer typed as the M-Pesa account number.
// ok=false means it is not a valid account number of any product (the
// payment goes to suspense).
func (s *Service) Route(ctx context.Context, q *store.Queries, typed string) (RouteResult, bool, error) {
	parsed, err := ParseAccountReference(typed)
	if err != nil {
		return RouteResult{}, false, nil
	}
	row, err := q.RouteAccountNumber(ctx, parsed.Normalized)
	if db.IsNotFound(err) {
		return RouteResult{}, false, nil
	}
	if err != nil {
		return RouteResult{}, false, err
	}
	return RouteResult{
		PayerID: row.ID, ProductID: row.ProductID, AccountNumber: row.AccountNumber,
		Active: row.Status == store.PayerStatusActive && row.ProductStatus == store.ProductStatusActive,
	}, true, nil
}

func fromRow(r store.PayerAccount, phones []store.PayerPhone) Payer {
	p := Payer{
		ID: r.ID, ProductID: r.ProductID, ExternalRef: r.ExternalRef, AccountNumber: r.AccountNumber,
		DisplayName: r.DisplayName, Status: r.Status, CreditBalance: money.KES(r.CreditBalance),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.Email != nil {
		p.Email = *r.Email
	}
	for _, ph := range phones {
		p.Phones = append(p.Phones, ph.Phone)
	}
	return p
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
