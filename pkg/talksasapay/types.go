package talksasapay

import (
	"encoding/json"
	"fmt"
	"time"
)

// Amounts are integer Kenyan shillings (KES); there are no decimals.
// IDs are UUID strings. Unknown JSON fields are ignored so newer servers
// can add fields without breaking this client.

// WhoAmI is the app an API key belongs to.
type WhoAmI struct {
	AppID         string `json:"app_id"`
	AppName       string `json:"app_name"`
	AccountPrefix string `json:"account_prefix"` // your paybill account-number prefix, e.g. HOST
	KeyID         string `json:"key_id"`
}

// ChargeInput is the body of CreateCharge. Zero optional fields are omitted.
type ChargeInput struct {
	Amount    int64  `json:"amount"`    // KES, 1..250,000
	Reference string `json:"reference"` // your invoice number / order id; unique among your charges

	Description string `json:"description,omitempty"` // shown on the pay page
	// Phone, when set, prompts this Safaricom number right away
	// (07XXXXXXXX, 01XXXXXXXX, 2547XXXXXXXX or +2547XXXXXXXX).
	Phone string `json:"phone,omitempty"`
	// AccountNumber is your own paybill account number for this charge
	// (your prefix, "-", a digit, then A-Z/0-9; max 12). Empty: generated.
	AccountNumber string `json:"account_number,omitempty"`
	// Metadata is any JSON object up to 4 KB, echoed back on the charge and
	// its events.
	Metadata map[string]any `json:"metadata,omitempty"`
	// ExpiresIn is how long a pending charge lives (15 min..7 days; whole
	// seconds; server default 24 h). Zero: server default.
	ExpiresIn time.Duration `json:"-"`

	// IdempotencyKey makes the create safe to retry, also across process
	// restarts if you store it (e.g. "invoice-42-charge-1"). Empty: a random
	// key is generated per call; the client's own retries always reuse it.
	IdempotencyKey string `json:"-"`
}

// MarshalJSON adds expires_in (seconds) when ExpiresIn is set.
func (in ChargeInput) MarshalJSON() ([]byte, error) {
	type plain ChargeInput
	out := struct {
		plain
		ExpiresIn int64 `json:"expires_in,omitempty"`
	}{plain: plain(in), ExpiresIn: int64(in.ExpiresIn / time.Second)}
	return json.Marshal(out)
}

// Charge statuses.
const (
	ChargePending     = "pending"      // waiting for money; may be prompted (again)
	ChargeNeedsReview = "needs_review" // success without a receipt; staff are checking; NOT paid
	ChargeSucceeded   = "succeeded"    // paid: see TransactionID
	ChargeExpired     = "expired"      // not paid in time; late money still succeeds it
	ChargeCancelled   = "cancelled"    // you cancelled it; late money still succeeds it
	ChargeReversed    = "reversed"     // staff reversed the payment
)

// Paybill is how to pay manually from the M-Pesa menu.
type Paybill struct {
	BusinessNumber string `json:"business_number"`
	AccountNumber  string `json:"account_number"`
}

// Charge is "collect exactly Amount KES for my Reference". Its amount,
// reference and account number never change.
type Charge struct {
	Object        string         `json:"object"`
	ID            string         `json:"id"`
	Reference     string         `json:"reference"`
	Amount        int64          `json:"amount"`
	Currency      string         `json:"currency"`
	Description   *string        `json:"description"`
	Metadata      map[string]any `json:"metadata"` // as you sent it
	AccountNumber string         `json:"account_number"`
	Status        string         `json:"status"`       // ChargePending ... ChargeReversed
	CheckoutURL   string         `json:"checkout_url"` // hosted pay page for this charge
	Paybill       *Paybill       `json:"paybill"`      // nil unless pending or needs_review
	ExpiresAt     time.Time      `json:"expires_at"`
	SucceededAt   *time.Time     `json:"succeeded_at"`
	TransactionID *string        `json:"transaction_id"` // the payment that made it succeed
	LatestSTKPush *STKPush       `json:"latest_stk_push"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// Succeeded reports a paid charge.
func (c *Charge) Succeeded() bool { return c.Status == ChargeSucceeded }

// Pending reports a charge still waiting for money (it can be prompted and cancelled).
func (c *Charge) Pending() bool { return c.Status == ChargePending }

// Transaction kinds.
const (
	KindPayment  = "payment"
	KindReversal = "reversal"
)

// Transaction is money received (or reversed) for your app. A reversal is
// its own transaction whose ReversesTransactionID names the payment.
type Transaction struct {
	Object                string     `json:"object"`
	ID                    string     `json:"id"`
	Kind                  string     `json:"kind"`      // payment | refund | reversal | adjustment
	Direction             string     `json:"direction"` // in | out
	Channel               string     `json:"channel"`   // mpesa_stk | mpesa_c2b | mpesa_b2c | manual
	Status                string     `json:"status"`    // pending | succeeded | failed | cancelled | needs_review
	Amount                int64      `json:"amount"`
	Currency              string     `json:"currency"`
	Receipt               *string    `json:"receipt"`
	Phone                 *string    `json:"phone"`          // masked
	AccountNumber         *string    `json:"account_number"` // as typed at the paybill, or sent with the prompt
	ChargeID              *string    `json:"charge_id"`
	ReversesTransactionID *string    `json:"reverses_transaction_id"`
	OccurredAt            *time.Time `json:"occurred_at"`
	CreatedAt             time.Time  `json:"created_at"`
	CompletedAt           *time.Time `json:"completed_at"`
}

// PaymentReceived is the data of payment.received: money routed to your
// app that paid no waiting charge. DuplicateOfCharge is set when the
// customer paid an already-succeeded charge again (refund it); otherwise it
// is a paybill payment with your prefix that matched no waiting charge, and
// you decide what it pays from AccountNumber and Amount.
type PaymentReceived struct {
	Transaction
	DuplicateOfCharge *string `json:"duplicate_of_charge"`
}

// Event types. New types may be added; ignore ones you don't handle.
const (
	// Mark an invoice paid only on charge.succeeded, never on
	// stk_push.sent or *.needs_review.
	EventChargeSucceeded   = "charge.succeeded"    // paid; may follow charge.cancelled/expired (late money)
	EventChargeNeedsReview = "charge.needs_review" // success without a receipt; staff are confirming; not paid
	EventChargeExpired     = "charge.expired"
	EventChargeCancelled   = "charge.cancelled"

	EventSTKPushSent        = "stk_push.sent"         // prompt on the customer's phone; not paid
	EventSTKPushFailed      = "stk_push.failed"       // cancelled, timed out, insufficient funds...: the charge stays pending
	EventSTKPushSucceeded   = "stk_push.succeeded"    // receipt recorded; charge.succeeded comes with it
	EventSTKPushNeedsReview = "stk_push.needs_review" // success without a receipt; staff are confirming; not paid

	EventPaymentReceived     = "payment.received"     // money that paid no waiting charge (PaymentReceived)
	EventTransactionReversed = "transaction.reversed" // staff reversed a payment (the reversal Transaction)
)

// EventTypes lists every event type this version knows.
var EventTypes = []string{
	EventChargeSucceeded, EventChargeNeedsReview, EventChargeExpired, EventChargeCancelled,
	EventSTKPushSent, EventSTKPushFailed, EventSTKPushSucceeded, EventSTKPushNeedsReview,
	EventPaymentReceived, EventTransactionReversed,
}

// STK push statuses.
const (
	STKInitiating  = "initiating"   // being sent to Safaricom
	STKSent        = "sent"         // on the customer's phone
	STKSucceeded   = "succeeded"    // paid: receipt recorded
	STKFailed      = "failed"       // see ResultCode / ResultDesc; the charge stays pending
	STKNeedsReview = "needs_review" // success without a receipt; staff are confirming; NOT paid
)

// STKPushOptions are the optional settings of STKPush.
type STKPushOptions struct {
	// IdempotencyKey makes the push safe to retry: the same key returns the
	// same push instead of sending a second prompt. Empty: a random key is
	// generated, which protects only this call's own retries. Reuse a key
	// only for the same phone; after a failed prompt use a new key.
	IdempotencyKey string
}

// STKPush is an M-Pesa payment prompt sent to a customer's phone for a
// charge's exact amount.
type STKPush struct {
	Object            string     `json:"object"`
	ID                string     `json:"id"`
	ChargeID          string     `json:"charge_id"`
	TransactionID     string     `json:"transaction_id"` // pending until Safaricom answers
	Status            string     `json:"status"`         // STKInitiating ... STKNeedsReview
	Amount            int64      `json:"amount"`
	Phone             string     `json:"phone"`               // masked
	CheckoutRequestID *string    `json:"checkout_request_id"` // Safaricom's id; quote it to support
	ResultCode        *string    `json:"result_code"`         // e.g. "1032" = cancelled by the customer
	ResultDesc        *string    `json:"result_desc"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	CompletedAt       *time.Time `json:"completed_at"`
}

// Pending reports a prompt still waiting for the customer or Safaricom.
func (p *STKPush) Pending() bool { return p.Status == STKInitiating || p.Status == STKSent }

// Event is a webhook body and an item of the events feed. Data holds the
// subject as it was when the event happened; decode it with Charge,
// STKPush, PaymentReceived or Transaction according to Type.
type Event struct {
	Object     string          `json:"object"`
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	APIVersion string          `json:"api_version"`
	CreatedAt  time.Time       `json:"created_at"`
	Data       json.RawMessage `json:"data"`
}

// Charge decodes Data of a charge.* event.
func (e Event) Charge() (*Charge, error) {
	var v Charge
	return &v, e.decode(&v, EventChargeSucceeded, EventChargeNeedsReview, EventChargeExpired, EventChargeCancelled)
}

// STKPush decodes Data of a stk_push.* event.
func (e Event) STKPush() (*STKPush, error) {
	var v STKPush
	return &v, e.decode(&v, EventSTKPushSent, EventSTKPushFailed, EventSTKPushSucceeded, EventSTKPushNeedsReview)
}

// PaymentReceived decodes Data of a payment.received event.
func (e Event) PaymentReceived() (*PaymentReceived, error) {
	var v PaymentReceived
	return &v, e.decode(&v, EventPaymentReceived)
}

// Transaction decodes Data of a transaction.reversed event (the reversal
// transaction: ReversesTransactionID names the payment, ChargeID the charge
// that is now reversed). It also accepts payment.received.
func (e Event) Transaction() (*Transaction, error) {
	var v Transaction
	return &v, e.decode(&v, EventTransactionReversed, EventPaymentReceived)
}

func (e Event) decode(v any, types ...string) error {
	ok := false
	for _, t := range types {
		ok = ok || e.Type == t
	}
	if !ok {
		return fmt.Errorf("talksasapay: event %s has type %q, not one of %v", e.ID, e.Type, types)
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
