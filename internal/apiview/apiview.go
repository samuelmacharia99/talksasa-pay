// Package apiview defines the JSON resources products see: in /v1 responses
// AND in webhook/event payloads, so both always agree. Field names and types
// are part of the public contract (api/product.openapi.yaml); change them
// only additively.
//
// Rules: amounts are integer KES; no internal-only fields (hosted token is
// exposed only as pay_url, never raw payloads, idempotency keys or staff
// data); payer phone numbers are masked.
package apiview

import (
	"time"

	"github.com/google/uuid"

	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// APIVersion is sent with every event so receivers can branch on it.
const APIVersion = "2026-11-24"

// View holds the settings needed to render resources.
type View struct {
	// PayBaseURL is the public pay page base, e.g. "https://pay.talksasa.co.ke/".
	PayBaseURL string
	// Paybill is the Talksasa paybill ("" hides instructions).
	Paybill string
}

// PayerRef identifies the payer on embedded resources.
type PayerRef struct {
	ExternalRef   string `json:"external_ref"`
	AccountNumber string `json:"account_number"`
}

// Paybill is how to pay manually from the M-Pesa menu.
type Paybill struct {
	BusinessNumber string `json:"business_number"`
	AccountNumber  string `json:"account_number"`
}

// PaymentRequest is a bill a product asked us to collect.
type PaymentRequest struct {
	Object      string     `json:"object"` // "payment_request"
	ID          uuid.UUID  `json:"id"`
	Reference   string     `json:"reference"`
	Description string     `json:"description"`
	Currency    string     `json:"currency"`
	Amount      int64      `json:"amount"`
	AmountPaid  int64      `json:"amount_paid"`
	Outstanding int64      `json:"outstanding"`
	Status      string     `json:"status"`
	Payer       PayerRef   `json:"payer"`
	PayURL      string     `json:"pay_url"`
	Paybill     *Paybill   `json:"paybill"`
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PaidAt      *time.Time `json:"paid_at"`
	ClosedAt    *time.Time `json:"closed_at"`
}

// PaymentRequestRow is the joined row both product-API queries return.
type PaymentRequestRow = store.GetPaymentRequestForProductRow

// PaymentRequest renders a request.
func (v View) PaymentRequest(r PaymentRequestRow) PaymentRequest {
	out := PaymentRequest{
		Object: "payment_request", ID: r.ID, Reference: r.ProductReference, Description: r.Description,
		Currency: r.Currency, Amount: r.Amount, AmountPaid: r.AmountPaid, Outstanding: r.Amount - r.AmountPaid,
		Status: string(r.Status), Payer: PayerRef{ExternalRef: r.PayerExternalRef, AccountNumber: r.AccountNumber},
		PayURL: v.PayBaseURL + r.HostedToken, ExpiresAt: utcPtr(r.ExpiresAt), CreatedAt: r.CreatedAt.UTC(),
		UpdatedAt: r.UpdatedAt.UTC(), PaidAt: utcPtr(r.PaidAt), ClosedAt: utcPtr(r.ClosedAt),
	}
	if v.Paybill != "" && (r.Status == store.PaymentRequestStatusOpen || r.Status == store.PaymentRequestStatusPartiallyPaid) {
		out.Paybill = &Paybill{BusinessNumber: v.Paybill, AccountNumber: r.AccountNumber}
	}
	return out
}

// FromListRow converts a list row to the common row shape.
func FromListRow(r store.ListPaymentRequestsForProductRow) PaymentRequestRow {
	return PaymentRequestRow(r)
}

// Allocation is one line of where a payment's money went.
type Allocation struct {
	// PaymentRequestID is null when the money went to the payer's credit balance.
	PaymentRequestID *uuid.UUID `json:"payment_request_id"`
	Amount           int64      `json:"amount"`
	Kind             string     `json:"kind"` // payment | credit_application | reversal
}

// Transaction is money that moved.
type Transaction struct {
	Object                string       `json:"object"` // "transaction"
	ID                    uuid.UUID    `json:"id"`
	Kind                  string       `json:"kind"`
	Direction             string       `json:"direction"`
	Channel               string       `json:"channel"`
	Status                string       `json:"status"`
	Amount                int64        `json:"amount"`
	Currency              string       `json:"currency"`
	Receipt               *string      `json:"receipt"`
	Phone                 *string      `json:"phone"` // masked
	Payer                 *PayerRef    `json:"payer"`
	PaymentRequestID      *uuid.UUID   `json:"payment_request_id"`
	ReversesTransactionID *uuid.UUID   `json:"reverses_transaction_id"`
	Allocations           []Allocation `json:"allocations"`
	OccurredAt            *time.Time   `json:"occurred_at"`
	CreatedAt             time.Time    `json:"created_at"`
	CompletedAt           *time.Time   `json:"completed_at"`
}

// TransactionRow is the joined row both product-API queries return.
type TransactionRow = store.GetTransactionViewForProductRow

// FromTxnListRow converts a list row to the common row shape.
func FromTxnListRow(r store.ListTransactionsForProductRow) TransactionRow { return TransactionRow(r) }

// Transaction renders a transaction with its allocation lines.
func (v View) Transaction(t TransactionRow, allocs []store.ListAllocationsForTransactionProductRow) Transaction {
	out := Transaction{
		Object: "transaction", ID: t.ID, Kind: string(t.Kind), Direction: string(t.Direction), Channel: string(t.Channel),
		Status: string(t.Status), Amount: t.Amount, Currency: t.Currency, Receipt: t.ProviderTransactionID,
		PaymentRequestID: t.PaymentRequestID, ReversesTransactionID: t.ReversesTransactionID,
		OccurredAt: utcPtr(t.OccurredAt), CreatedAt: t.CreatedAt.UTC(), CompletedAt: utcPtr(t.CompletedAt),
		Allocations: make([]Allocation, 0, len(allocs)),
	}
	if t.Phone != nil {
		m := tlog.MaskPhone(*t.Phone)
		out.Phone = &m
	}
	if t.PayerExternalRef != nil && t.AccountNumber != nil {
		out.Payer = &PayerRef{ExternalRef: *t.PayerExternalRef, AccountNumber: *t.AccountNumber}
	}
	for _, a := range allocs {
		out.Allocations = append(out.Allocations, Allocation{PaymentRequestID: a.PaymentRequestID, Amount: a.Amount, Kind: string(a.BatchKind)})
	}
	return out
}

// Payer is a customer account of a product.
type Payer struct {
	Object        string    `json:"object"` // "payer"
	ID            uuid.UUID `json:"id"`
	ExternalRef   string    `json:"external_ref"`
	AccountNumber string    `json:"account_number"`
	DisplayName   string    `json:"display_name"`
	Email         *string   `json:"email"`
	Phones        []string  `json:"phones"` // masked
	Status        string    `json:"status"`
	CreditBalance int64     `json:"credit_balance"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Payer renders a payer account.
func (v View) Payer(p store.PayerAccount, phones []string) Payer {
	out := Payer{
		Object: "payer", ID: p.ID, ExternalRef: p.ExternalRef, AccountNumber: p.AccountNumber, DisplayName: p.DisplayName,
		Email: p.Email, Status: string(p.Status), CreditBalance: p.CreditBalance,
		CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(), Phones: make([]string, 0, len(phones)),
	}
	for _, ph := range phones {
		out.Phones = append(out.Phones, tlog.MaskPhone(ph))
	}
	return out
}

// CreditChange is the data of a payer.credit_changed event.
type CreditChange struct {
	Payer         PayerRef   `json:"payer"`
	Delta         int64      `json:"delta"`
	CreditBalance int64      `json:"credit_balance"`
	TransactionID *uuid.UUID `json:"transaction_id"`
}

// Event is the envelope of a webhook body and of each /v1/events item.
type Event struct {
	Object     string    `json:"object"` // "event"
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	APIVersion string    `json:"api_version"`
	CreatedAt  time.Time `json:"created_at"`
	Data       any       `json:"data"`
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
