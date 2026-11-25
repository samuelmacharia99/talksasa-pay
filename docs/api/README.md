# Talksasa Pay app API

Talksasa Pay is an M-Pesa payment gateway on the Talksasa paybill. Your app
makes one call: "collect KES 1,500 for my invoice INV-2026-0042". Talksasa
Pay sends the M-Pesa prompt to the customer's phone (or gives you a hosted
checkout link and paybill instructions). It confirms the payment with
Safaricom, then tells your app the result, with the M-Pesa receipt, by
webhook and in an events feed.

Talksasa Pay knows nothing about your customers or invoices. You keep your
own records and link them to a charge by your `reference` (and, if you
like, `metadata`). You never handle M-Pesa yourself, and you can never
record, move or refund money through this API: you ask for money and you
read what happened.

| Guide | Read it when |
|---|---|
| [Authentication](authentication.md) | You get your first key, rotate a key, or restrict a key to your servers |
| [Charges](charges.md) | You need the money rules: what a charge is, when it is paid, what happens to late or double payments |
| [Idempotency and retries](idempotency-and-retries.md) | You write the code that calls the API |
| [M-Pesa payment prompts](stk-push.md) | You want a "Pay now" button that sends the M-Pesa prompt to the customer's phone |
| [Paybill payments](paybill.md) | Your customers pay from the M-Pesa menu with an account number |
| [Webhooks](webhooks.md) | You build the endpoint that hears about payments |
| [Events feed](events-feed.md) | You want a backstop so no payment is ever missed |
| [Errors](errors.md) | You see an error `code` and want to know what to do |
| [Laravel](laravel.md) | Your app is a Laravel app (the PHP SDK) |

The contract itself is [`api/app.openapi.yaml`](../../api/app.openapi.yaml).
If a guide and the contract disagree, the contract wins; please tell us.

## Concepts in one minute

- **App**: your system, as Talksasa Pay sees it. It has API keys, a webhook
  URL and an **account prefix** such as `HOST`. Paybill payments whose
  account number starts with your prefix are yours.
- **Charge**: one request to collect an exact amount, for example "KES
  1,500 for INV-2026-0042". It has your `reference`, an `amount` in whole
  Kenya shillings, an `account_number` for paying at the paybill and a
  `checkout_url` you can send the customer to. It is `succeeded` only when
  Safaricom confirms the payment with an M-Pesa receipt.
- **STK push**: an M-Pesa prompt on the customer's phone ("Pay KES 1,500 to
  Talksasa?"). A charge can be prompted again after a prompt fails.
- **Transaction**: money that moved, with its M-Pesa receipt. The one that
  paid a charge is the charge's `transaction_id`.
- **Event**: a record of something that changed. Delivered to your webhook
  URL and also kept in a feed you can read at any time.

All amounts are integers in KES. There are no decimals anywhere.

## Quick start (10 minutes)

You need an API key (`tsp_test_...` for staging, `tsp_live_...` for
production) and a webhook signing secret (`whsec_...`) from Talksasa staff,
plus the API base URL for your environment. Locally the API runs at
`http://localhost:8088/v1`.

```sh
export TSP_API=https://<api-host>/v1      # ask Talksasa staff for the host
export TSP_KEY=tsp_test_...               # never commit this
```

### 1. Check your key

```sh
curl -sS "$TSP_API/whoami" -H "Authorization: Bearer $TSP_KEY"
```

```json
{ "app_id": "0193...", "app_name": "Cloud Hosting", "account_prefix": "HOST", "key_id": "abcd..." }
```

A `401` means the key is wrong, revoked or expired. See
[authentication](authentication.md).

### 2. Create a charge

```sh
curl -sS -X POST "$TSP_API/charges" \
  -H "Authorization: Bearer $TSP_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: invoice-INV-2026-0042" \
  -d '{"reference": "INV-2026-0042", "amount": 1500, "description": "Cloud Hosting, December 2026",
       "metadata": {"customer_id": "1042"}}'
```

```json
{
  "object": "charge",
  "id": "0193a5f2-0000-7000-8000-0000000000aa",
  "reference": "INV-2026-0042",
  "amount": 1500,
  "currency": "KES",
  "description": "Cloud Hosting, December 2026",
  "metadata": { "customer_id": "1042" },
  "account_number": "HOST-4K7QZ9M",
  "status": "pending",
  "checkout_url": "https://pay.../k3j...",
  "paybill": { "business_number": "123456", "account_number": "HOST-4K7QZ9M" },
  "expires_at": "2026-11-26T09:00:00Z",
  "succeeded_at": null,
  "transaction_id": null,
  "latest_stk_push": null,
  "created_at": "2026-11-25T09:00:00Z",
  "updated_at": "2026-11-25T09:00:00Z"
}
```

Store the `id` against your invoice. If the call times out, send exactly
the same request again with the same `Idempotency-Key`: you get the
original charge back, never a second one.

### 3. Get the customer to pay

Pick one, or offer all three:

- **Checkout link.** Redirect the customer to `checkout_url`, or send it by
  SMS or email. The hosted page shows the amount and description, lets the
  customer type their number to get the M-Pesa prompt, and shows the
  paybill steps. You build no payment screens.
- **Prompt from your own screen.** Add `"phone": "0712345678"` when you
  create the charge, and the first M-Pesa prompt is sent at once
  (`latest_stk_push` in the response). Or prompt later with
  `POST /charges/{id}/stk-push`. See [M-Pesa payment prompts](stk-push.md).
- **Paybill.** Show `paybill.business_number` and `paybill.account_number`.
  The customer pays from the M-Pesa menu. See [paybill payments](paybill.md).

### 4. Receive the webhook

When Safaricom confirms the payment, Talksasa Pay `POST`s an event to your
webhook URL (set by Talksasa staff):

```
POST /hooks/talksasa-pay
Talksasa-Event-Id: 0193...
Talksasa-Event-Type: charge.succeeded
Talksasa-Signature: t=1795597200,v1=65d85ffe...

{"object":"event","id":"0193...","type":"charge.succeeded","api_version":"2026-11-25","created_at":"...","data":{ ...the charge, status "succeeded", with transaction_id... }}
```

Verify the signature, store the event id, answer `200` quickly, then
process it. [Webhooks](webhooks.md) has the verification code for PHP and
Go.

**Mark your invoice paid only on `charge.succeeded`** (or when a re-read
charge says `status: succeeded`). Never on the `201` from create, never on
`stk_push.sent`, and never on `charge.needs_review`.

### 5. Confirm by reading the charge

Before you mark the invoice paid, read the charge back. This protects you
from events that arrive late or out of order.

```sh
curl -sS "$TSP_API/charges/0193a5f2-0000-7000-8000-0000000000aa" -H "Authorization: Bearer $TSP_KEY"
```

Act on `status` from this response, not on the event type alone. The
receipt is on the transaction: `GET /transactions/{transaction_id}`.

### 6. Handle money that paid no charge

Some money arrives for your app without paying a charge: a customer who
paid at the paybill with the wrong amount or an old account number, or who
paid the same charge twice. You get `payment.received` with the account
number, amount, phone and receipt, and you decide what it pays. See
[charges](charges.md#money-that-pays-no-charge).

### 7. Add the backstop

Webhooks can be delayed while your server is down. Run a small scheduled
job that reads [`GET /events`](events-feed.md) and processes anything you
have not seen. With both in place you never miss a payment.

## Ground rules

- Keys are per app. A key can only ever see its own app's charges,
  transactions and events.
- Apps ask for money and read money. Only M-Pesa and Talksasa staff record
  or reverse it.
- Times are RFC 3339 in UTC (`2026-11-25T09:00:00Z`). Show them to Kenyan
  customers in Africa/Nairobi time.
- Every response carries `X-Request-Id`. Quote it when you ask Talksasa for
  help.
