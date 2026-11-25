# talksasapay (Go)

Go client for the Talksasa Pay app API (`/v1`, `api/app.openapi.yaml`):
charges, M-Pesa prompts (STK push), transactions, the gap-free events feed
and webhook verification. Standard library only (Go 1.23+ for the range-over-func iterators).

```go
import "github.com/talksasa/talksasa-pay/pkg/talksasapay"
```

## How it works

Your app creates a **charge** ("collect KES 1,500 for my reference
INV-1042"). The gateway prompts the customer's phone and/or the customer
pays at the paybill with the charge's account number; the gateway confirms
with Safaricom and reports the result, with the M-Pesa receipt, by webhook
and in the events feed. It knows nothing about your customers or invoices.

- Amounts are integer KES, 1 to 250,000. Amount, reference and account
  number never change; to charge a different amount, cancel and create a
  new charge. `Reference` is unique among your charges
  (`duplicate_reference`).
- **Mark an invoice paid only on `charge.succeeded`.** Never on
  `stk_push.sent` or `*.needs_review`.
- A failed prompt does not fail the charge: it stays `pending`; prompt again.
- Late money is never dropped: a cancelled or expired charge can still
  succeed.
- `payment.received`: money that paid no waiting charge (a duplicate of an
  already-succeeded charge, `DuplicateOfCharge` set; or a paybill payment
  with your prefix that matched no charge: you decide what it pays).
- `transaction.reversed`: staff reversed a payment; the charge is `reversed`.

## Use

```go
c, err := talksasapay.New("https://pay-api.talksasa.co.ke", os.Getenv("TALKSASA_PAY_API_KEY"),
	talksasapay.WithUserAgent("cloud-hosting/3.2"))

charge, replayed, err := c.CreateCharge(ctx, talksasapay.ChargeInput{
	Amount:         1500,
	Reference:      "INV-1042",
	Description:    "Cloud hosting, December",
	Metadata:       map[string]any{"invoice_id": 42},
	Phone:          "0712345678",        // optional: prompt right away (see charge.LatestSTKPush)
	ExpiresIn:      2 * time.Hour,       // optional: 15 min..7 days, default 24 h
	IdempotencyKey: "invoice-42-charge", // optional: random per call otherwise
})
switch {
case talksasapay.IsCode(err, talksasapay.CodeDuplicateReference):
	// you already have a charge for this reference: c.ListCharges(ctx, ListChargesParams{Reference: ...})
case err != nil:
	return err
}
// Send the customer to charge.CheckoutURL, or show charge.Paybill.

// Prompt (again) later:
push, _, err := c.STKPush(ctx, charge.ID, "0712345678", talksasapay.STKPushOptions{})
switch {
case talksasapay.IsCode(err, talksasapay.CodeSTKInProgress): // a prompt is already on the phone
case talksasapay.IsCode(err, talksasapay.CodeMpesaUnavailable),
	talksasapay.IsCode(err, talksasapay.CodeMpesaError),
	talksasapay.IsCode(err, talksasapay.CodeAmountTooLarge): // show the paybill instead
}
```

| Method | API |
|---|---|
| `Whoami(ctx) (*WhoAmI, error)` | `GET /whoami` |
| `CreateCharge(ctx, ChargeInput) (*Charge, replayed bool, error)` | `POST /charges` |
| `GetCharge(ctx, id) (*Charge, error)` | `GET /charges/{id}` |
| `ListCharges(ctx, ListChargesParams) iter.Seq2[Charge, error]` / `ListChargesPage` | `GET /charges` |
| `CancelCharge(ctx, id) (*Charge, error)` | `POST /charges/{id}/cancel` |
| `STKPush(ctx, chargeID, phone, STKPushOptions) (*STKPush, replayed bool, error)` | `POST /charges/{id}/stk-push` |
| `GetSTKPush(ctx, id) (*STKPush, error)` | `GET /stk-pushes/{id}` |
| `ListTransactions(ctx, ListTransactionsParams{ChargeID, Since}) iter.Seq2[Transaction, error]` / `ListTransactionsPage` | `GET /transactions` |
| `GetTransaction(ctx, id) (*Transaction, error)` | `GET /transactions/{id}` |
| `ListEvents(ctx, ListEventsParams) (*EventPage, error)`; `EventStream` polls it | `GET /events` |

```go
for ch, err := range c.ListCharges(ctx, talksasapay.ListChargesParams{Status: talksasapay.ChargePending}) {
	if err != nil {
		return err
	}
	_ = ch
}
```

## Events and webhooks

```go
http.Handle("/webhooks/talksasa-pay", talksasapay.WebhookHandler(secret, func(ctx context.Context, ev talksasapay.Event) error {
	switch ev.Type {
	case talksasapay.EventChargeSucceeded:
		ch, err := ev.Charge()
		if err != nil {
			return err
		}
		return markPaid(ctx, ch.Reference, *ch.TransactionID) // idempotent
	case talksasapay.EventPaymentReceived:
		p, err := ev.PaymentReceived()
		if err != nil {
			return err
		}
		if p.DuplicateOfCharge != nil {
			return queueRefund(ctx, p)
		}
		return routePaybill(ctx, *p.AccountNumber, p.Amount, p.ID)
	case talksasapay.EventTransactionReversed:
		t, err := ev.Transaction() // the reversal; ReversesTransactionID = the payment
		if err != nil {
			return err
		}
		return reopen(ctx, *t.ReversesTransactionID)
	}
	return nil // stk_push.*, charge.needs_review/expired/cancelled, future types
}))
```

`WebhookHandler` verifies `Talksasa-Signature` on the raw body (any `v1`
of any secret, `|now - t| <= 300 s`, constant time), answers 400 on
failure and 500 when your function errors (Talksasa redelivers).
`VerifyWebhook` / `VerifyWebhookSignature` do the same without HTTP;
`SignWebhook` signs test bodies. Delivery is at least once and unordered:
de-duplicate by `Event.ID`. Typed accessors: `Charge()` (charge.\*),
`STKPush()` (stk_push.\*), `PaymentReceived()` (payment.received),
`Transaction()` (transaction.reversed, also payment.received).
`EventTypes` lists every known type.

`EventStream` polls `GET /events` from a stored cursor (saved after each
fully handled page) as a backstop or instead of webhooks.

## Errors and retries

API errors are `*APIError` (`Status`, `Code`, `Detail`, `RequestID`,
`FieldErrors`, `RetryAfter`); test with `IsCode(err, CodeX)`, `IsNotFound`,
`AsAPIError`. There is a `Code*` constant for every problem code in the
spec (`CodeDuplicateReference`, `CodeNotCancellable`, `CodeNotPayable`,
`CodeSTKInProgress`, `CodeAmountTooLarge`, `CodeIdempotencyConflict`,
`CodeMpesaUnavailable`, `CodeMpesaError`, `CodeValidationFailed`,
`CodeRateLimited`, `CodeUnauthorized`, `CodeIPNotAllowed`,
`CodeAppSuspended`, ...).

Only safe requests are retried: GETs, `CreateCharge` and `STKPush` (they
always carry an Idempotency-Key and every retry reuses it, so one call
never creates two charges or sends two prompts) and `CancelCharge`
(idempotent). Retries happen on network errors and 429/502/503/504 with
exponential backoff and jitter, honouring `Retry-After` (up to 60 s).
`mpesa_error` and `mpesa_unavailable` are final and never retried. Options:
`WithMaxRetries`, `WithRetryBackoff`, `WithTimeout`, `WithHTTPClient`,
`WithLogger` (never sees the key or bodies).

## Tests

```sh
scripts/go.sh test -race ./pkg/...
```

`contract_test.go` checks the SDK against `api/app.openapi.yaml`;
`testdata/webhook_signature_vectors.json` holds the signature vectors shared
with the PHP SDK.
