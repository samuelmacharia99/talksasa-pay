package talksasapay

import (
	"encoding/json"
	"fmt"
	"time"
)

// Amounts are integer Kenyan shillings (KES); there are no decimals.
// IDs are UUID strings. Unknown JSON fields are ignored so newer servers
// can add fields without breaking this client.

// WhoAmI is the product an API key belongs to.
type WhoAmI struct {
	ProductID     string `json:"product_id"`
	ProductName   string `json:"product_name"`
	AccountPrefix string `json:"account_prefix"`
	KeyID         string `json:"key_id"`
}

// PayerInput is the body of UpsertPayer. PUT replaces the payer's details:
// a nil Email is sent as null and nil Phones as an empty list.
type PayerInput struct {
	DisplayName string   `json:"display_name"`
	Email       *string  `json:"email"`
	Phones      []string `json:"phones"` // Kenyan mobiles, any common format; first = primary; max 5
}

// Payer statuses.
const (
	PayerActive = "active"
	PayerClosed = "closed"
)

// Payer is one of your customers.
type Payer struct {
	Object        string    `json:"object"`
	ID            string    `json:"id"`
	ExternalRef   string    `json:"external_ref"`
	AccountNumber string    `json:"account_number"` // what the customer types at the paybill, e.g. HOST-1042
	DisplayName   string    `json:"display_name"`
	Email         *string   `json:"email"`
	Phones        []string  `json:"phones"` // masked
	Status        string    `json:"status"`
	CreditBalance int64     `json:"credit_balance"` // KES overpaid and not yet applied
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PayerRef identifies the payer on embedded resources.
type PayerRef struct {
	ExternalRef   string `json:"external_ref"`
	AccountNumber string `json:"account_number"`
}

// PaymentRequestInput is the body of CreatePaymentRequest.
type PaymentRequestInput struct {
	Payer       string     `json:"payer"`     // payer external_ref (upsert the payer first)
	Reference   string     `json:"reference"` // your invoice number
	Description string     `json:"description"`
	Amount      int64      `json:"amount"` // KES, 1..1,000,000,000
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`

	// IdempotencyKey makes the create safe to retry, also across process
	// restarts if you store it (e.g. "INV-1001-v1"). Empty: a random key is
	// generated, which protects only this call's own retries.
	IdempotencyKey string `json:"-"`
}

// Payment request statuses.
const (
	StatusOpen          = "open"
	StatusPartiallyPaid = "partially_paid"
	StatusPaid          = "paid"
	StatusCancelled     = "cancelled"
	StatusExpired       = "expired"
)

// Paybill is how to pay manually from the M-Pesa menu.
type Paybill struct {
	BusinessNumber string `json:"business_number"`
	AccountNumber  string `json:"account_number"`
}

// PaymentRequest is an amount a payer has been asked to pay.
type PaymentRequest struct {
	Object      string     `json:"object"`
	ID          string     `json:"id"`
	Reference   string     `json:"reference"`
	Description string     `json:"description"`
	Currency    string     `json:"currency"`
	Amount      int64      `json:"amount"`
	AmountPaid  int64      `json:"amount_paid"`
	Outstanding int64      `json:"outstanding"`
	Status      string     `json:"status"`
	Payer       PayerRef   `json:"payer"`
	PayURL      string     `json:"pay_url"`
	Paybill     *Paybill   `json:"paybill"` // nil once closed
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PaidAt      *time.Time `json:"paid_at"`
	ClosedAt    *time.Time `json:"closed_at"`
}

// Allocation kinds.
const (
	AllocationPayment           = "payment"
	AllocationCreditApplication = "credit_application" //nolint:gosec // allocation kind, not a credential
	AllocationReversal          = "reversal"
)

// Allocation is one line of where a transaction's money went.
type Allocation struct {
	PaymentRequestID *string `json:"payment_request_id"` // nil: the payer's credit balance
	Amount           int64   `json:"amount"`             // negative on reversals
	Kind             string  `json:"kind"`
}

// Transaction is money received (or reversed) for one of your payers.
type Transaction struct {
	Object                string       `json:"object"`
	ID                    string       `json:"id"`
	Kind                  string       `json:"kind"`      // payment | refund | reversal | adjustment
	Direction             string       `json:"direction"` // in | out
	Channel               string       `json:"channel"`   // mpesa_stk | mpesa_c2b | mpesa_b2c | manual
	Status                string       `json:"status"`    // pending | succeeded | failed | cancelled | needs_review
	Amount                int64        `json:"amount"`
	Currency              string       `json:"currency"`
	Receipt               *string      `json:"receipt"`
	Phone                 *string      `json:"phone"` // masked
	Payer                 *PayerRef    `json:"payer"`
	PaymentRequestID      *string      `json:"payment_request_id"`
	ReversesTransactionID *string      `json:"reverses_transaction_id"`
	Allocations           []Allocation `json:"allocations"`
	OccurredAt            *time.Time   `json:"occurred_at"`
	CreatedAt             time.Time    `json:"created_at"`
	CompletedAt           *time.Time   `json:"completed_at"`
}

// CreditChange is the data of a payer.credit_changed event.
type CreditChange struct {
	Payer         PayerRef `json:"payer"`
	Delta         int64    `json:"delta"`
	CreditBalance int64    `json:"credit_balance"`
	TransactionID *string  `json:"transaction_id"`
}

// Event types. New types may be added; ignore ones you don't handle.
const (
	EventPaymentRequestCreated       = "payment_request.created"
	EventPaymentRequestPartiallyPaid = "payment_request.partially_paid"
	EventPaymentRequestPaid          = "payment_request.paid"
	EventPaymentRequestReopened      = "payment_request.reopened"
	EventPaymentRequestCancelled     = "payment_request.cancelled"
	EventPaymentRequestExpired       = "payment_request.expired"
	EventTransactionAllocated        = "transaction.allocated"
	EventTransactionReversed         = "transaction.reversed"
	EventPayerCreditChanged          = "payer.credit_changed" //nolint:gosec // event name, not a credential
)

// Event is a webhook body and an item of the events feed. Data holds the
// resource as it was when the event happened; decode it with
// PaymentRequest, Transaction or CreditChange according to Type.
type Event struct {
	Object     string          `json:"object"`
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	APIVersion string          `json:"api_version"`
	CreatedAt  time.Time       `json:"created_at"`
	Data       json.RawMessage `json:"data"`
}

// PaymentRequest decodes Data of a payment_request.* event.
func (e Event) PaymentRequest() (*PaymentRequest, error) {
	var v PaymentRequest
	return &v, e.decode("payment_request.", &v)
}

// Transaction decodes Data of a transaction.* event.
func (e Event) Transaction() (*Transaction, error) {
	var v Transaction
	return &v, e.decode("transaction.", &v)
}

// CreditChange decodes Data of a payer.credit_changed event.
func (e Event) CreditChange() (*CreditChange, error) {
	var v CreditChange
	return &v, e.decode(EventPayerCreditChanged, &v)
}

func (e Event) decode(prefix string, v any) error {
	if len(e.Type) < len(prefix) || e.Type[:len(prefix)] != prefix {
		return fmt.Errorf("talksasapay: event %s has type %q, not %s*", e.ID, e.Type, prefix)
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("talksasapay: decode event %s data: %w", e.ID, err)
	}
	return nil
}

// Page is one page of a cursor-paginated list.
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"` // nil on the last page
}

// EventPage is one page of the events feed.
type EventPage struct {
	Data []Event `json:"data"`
	// NextCursor is what to pass as After next time; nil when Data is empty
	// (keep your previous cursor).
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}
