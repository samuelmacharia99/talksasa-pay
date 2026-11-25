// Package apiview defines the JSON resources apps see: in /v1 responses AND
// in webhook/event payloads, so both always agree. Field names and types are
// part of the public contract (api/app.openapi.yaml); change them only
// additively.
//
// Rules: amounts are integer KES; no internal-only fields (the hosted token
// is exposed only inside checkout_url; never raw payloads, idempotency keys
// or staff data); customer phone numbers are masked.
package apiview

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// APIVersion is sent with every event so receivers can branch on it.
const APIVersion = "2026-11-25"

// View holds the settings needed to render resources.
type View struct {
	// PayBaseURL is the public pay page base, e.g. "https://pay.talksasa.co.ke/".
	PayBaseURL string
	// Paybill is the Talksasa paybill ("" hides instructions).
	Paybill string
	// PaybillFrom, when set, gives the paybill in effect instead (it
	// follows the settings saved in the admin panel).
	PaybillFrom func() string
}

// Paybill is how to pay manually from the M-Pesa menu.
type Paybill struct {
	BusinessNumber string `json:"business_number"`
	AccountNumber  string `json:"account_number"`
}

// Charge is an app's request to collect an exact amount.
//
// status: pending (waiting for money; prompts may be retried), needs_review
// (Safaricom said paid without a receipt; staff are checking), succeeded
// (paid; see transaction_id), expired, cancelled, reversed (staff reversed
// the payment).
type Charge struct {
	Object        string          `json:"object"` // "charge"
	ID            uuid.UUID       `json:"id"`
	Reference     string          `json:"reference"`
	Amount        int64           `json:"amount"`
	Currency      string          `json:"currency"`
	Description   *string         `json:"description"`
	Metadata      json.RawMessage `json:"metadata"`
	AccountNumber string          `json:"account_number"`
	Status        string          `json:"status"`
	CheckoutURL   string          `json:"checkout_url"`
	// Paybill is set while the charge can be paid (pending or needs_review).
	Paybill       *Paybill   `json:"paybill"`
	ExpiresAt     time.Time  `json:"expires_at"`
	SucceededAt   *time.Time `json:"succeeded_at"`
	TransactionID *uuid.UUID `json:"transaction_id"`
	LatestSTKPush *STKPush   `json:"latest_stk_push"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Charge renders a charge. latest is its newest prompt, or nil.
func (v View) Charge(c store.Charge, latest *store.StkPush) Charge {
	out := Charge{
		Object: "charge", ID: c.ID, Reference: c.Reference, Amount: c.Amount, Currency: c.Currency,
		Description: c.Description, Metadata: json.RawMessage(c.Metadata), AccountNumber: c.AccountNumber,
		Status: string(c.Status), CheckoutURL: v.PayBaseURL + c.HostedToken, ExpiresAt: c.ExpiresAt.UTC(),
		SucceededAt: utcPtr(c.SucceededAt), TransactionID: c.TransactionID,
		CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
	}
	if len(c.Metadata) == 0 {
		out.Metadata = json.RawMessage("null")
	}
	if pb := v.paybill(); pb != "" && (c.Status == store.ChargeStatusPending || c.Status == store.ChargeStatusNeedsReview) {
		out.Paybill = &Paybill{BusinessNumber: pb, AccountNumber: c.AccountNumber}
	}
	if latest != nil {
		p := STKPushOf(*latest)
		out.LatestSTKPush = &p
	}
	return out
}

// Transaction is money that moved.
type Transaction struct {
	Object                string     `json:"object"` // "transaction"
	ID                    uuid.UUID  `json:"id"`
	Kind                  string     `json:"kind"`
	Direction             string     `json:"direction"`
	Channel               string     `json:"channel"`
	Status                string     `json:"status"`
	Amount                int64      `json:"amount"`
	Currency              string     `json:"currency"`
	Receipt               *string    `json:"receipt"`
	Phone                 *string    `json:"phone"` // masked
	AccountNumber         *string    `json:"account_number"`
	ChargeID              *uuid.UUID `json:"charge_id"`
	ReversesTransactionID *uuid.UUID `json:"reverses_transaction_id"`
	OccurredAt            *time.Time `json:"occurred_at"`
	CreatedAt             time.Time  `json:"created_at"`
	CompletedAt           *time.Time `json:"completed_at"`
}

// TransactionRow is the row both app-API transaction queries return.
type TransactionRow = store.GetTransactionViewForAppRow

// FromTxnListRow converts a list row to the common row shape.
func FromTxnListRow(r store.ListTransactionsForAppRow) TransactionRow { return TransactionRow(r) }

// FromTxn converts a full transaction row to the common row shape.
func FromTxn(t store.Transaction) TransactionRow {
	return TransactionRow{
		ID: t.ID, Direction: t.Direction, Kind: t.Kind, Channel: t.Channel, Status: t.Status, Amount: t.Amount,
		Currency: t.Currency, ProviderTransactionID: t.ProviderTransactionID, Phone: t.Phone,
		AccountNumber: t.AccountNumber, ChargeID: t.ChargeID, ReversesTransactionID: t.ReversesTransactionID,
		OccurredAt: t.OccurredAt, CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt,
	}
}

// Transaction renders a transaction.
func (v View) Transaction(t TransactionRow) Transaction {
	out := Transaction{
		Object: "transaction", ID: t.ID, Kind: string(t.Kind), Direction: string(t.Direction), Channel: string(t.Channel),
		Status: string(t.Status), Amount: t.Amount, Currency: t.Currency, Receipt: t.ProviderTransactionID,
		AccountNumber: t.AccountNumber, ChargeID: t.ChargeID, ReversesTransactionID: t.ReversesTransactionID,
		OccurredAt: utcPtr(t.OccurredAt), CreatedAt: t.CreatedAt.UTC(), CompletedAt: utcPtr(t.CompletedAt),
	}
	if t.Phone != nil {
		m := tlog.MaskPhone(*t.Phone)
		out.Phone = &m
	}
	return out
}

// PaymentReceived is the data of a payment.received event: money routed to
// the app that did not pay one of its waiting charges (no match, or the
// charge had already succeeded). The app decides what it pays.
type PaymentReceived struct {
	Transaction
	// DuplicateOfCharge is set when the money named a charge that had
	// already succeeded (the customer paid twice). The app or staff refund it.
	DuplicateOfCharge *uuid.UUID `json:"duplicate_of_charge"`
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

// STKPush is a payment prompt sent to a customer's phone.
//
// status: "initiating" (being sent to Safaricom), "sent" (on the phone),
// "succeeded" (paid; see transaction_id), "failed" (result_code/result_desc
// say why: cancelled, timed out, insufficient funds...; the charge stays
// pending and can be prompted again), "needs_review" (Safaricom reported
// success without a receipt; staff are confirming it — do not treat as paid
// until charge.succeeded arrives).
type STKPush struct {
	Object            string     `json:"object"` // "stk_push"
	ID                uuid.UUID  `json:"id"`
	ChargeID          uuid.UUID  `json:"charge_id"`
	TransactionID     uuid.UUID  `json:"transaction_id"`
	Status            string     `json:"status"`
	Amount            int64      `json:"amount"`
	Phone             string     `json:"phone"` // masked
	CheckoutRequestID *string    `json:"checkout_request_id"`
	ResultCode        *string    `json:"result_code"`
	ResultDesc        *string    `json:"result_desc"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	CompletedAt       *time.Time `json:"completed_at"`
}

// STKPushStatus maps the stored status to the public one.
func STKPushStatus(s store.StkPushStatus) string {
	if s == store.StkPushStatusUnknown {
		return "needs_review"
	}
	return string(s)
}

// STKPushOf renders a push. transaction_id is the transaction holding the
// money: normally the push's own, but a payment that arrived after we had
// given up on the push is recorded on a new transaction (paid_transaction_id).
func STKPushOf(p store.StkPush) STKPush {
	txnID := p.TransactionID
	if p.PaidTransactionID != nil {
		txnID = *p.PaidTransactionID
	}
	return STKPush{
		Object: "stk_push", ID: p.ID, ChargeID: p.ChargeID, TransactionID: txnID,
		Status: STKPushStatus(p.Status), Amount: p.Amount, Phone: tlog.MaskPhone(p.Phone),
		CheckoutRequestID: p.CheckoutRequestID, ResultCode: p.ResultCode, ResultDesc: p.ResultDesc,
		CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(), CompletedAt: utcPtr(p.CompletedAt),
	}
}
