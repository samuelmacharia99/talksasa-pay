# Talksasa Pay product API

Talksasa Pay collects money for Talksasa products (Cloud Hosting, ISP
Billing, and whatever comes next) through one Talksasa M-Pesa paybill. Your
product tells Talksasa Pay who your customers are and what they owe. Talksasa
Pay collects the money, works out which bill it pays, and tells your product.

You never handle M-Pesa yourself, and you can never record, move or refund
money through this API. You ask for money and you read what happened.

| Guide | Read it when |
|---|---|
| [Authentication](authentication.md) | You get your first key, rotate a key, or restrict a key to your servers |
| [Idempotency and retries](idempotency-and-retries.md) | You write the code that calls the API |
| [Webhooks](webhooks.md) | You build the endpoint that hears about payments |
| [Events feed](events-feed.md) | You want a backstop so no payment is ever missed |
| [Money rules](money-rules.md) | You need to know how payments are applied to bills |
| [Errors](errors.md) | You see an error `code` and want to know what to do |
| [Laravel](laravel.md) | Your product is a Laravel app (the PHP SDK) |

The contract itself is [`api/product.openapi.yaml`](../../api/product.openapi.yaml).
If a guide and the contract disagree, the contract wins; please tell us.

## Concepts in one minute

- **Payer**: one of your customers. You identify them by your own id, the
  `external_ref`. Each payer gets a permanent M-Pesa account number such as
  `HOST-1042`, which the customer can type at the paybill at any time.
- **Payment request**: a bill you want paid (an invoice, a renewal). It has
  your `reference` (your invoice number), an `amount` in whole Kenya
  shillings, and a `pay_url` you send the customer to.
- **Transaction**: money that moved, usually an M-Pesa payment. Its
  `allocations` say which bills it paid and how much went to the payer's
  credit balance.
- **Event**: a record of something that changed. Delivered to your webhook
  URL and also kept in a feed you can read at any time.

All amounts are integers in KES. There are no decimals anywhere.

## Quick start (10 minutes)

You need an API key (`tsp_test_...` for staging, `tsp_live_...` for
production) from Talksasa staff, and the API base URL for your environment.
Locally the API runs at `http://localhost:8088/v1`.

```sh
export TSP_API=https://<api-host>/v1      # ask Talksasa staff for the host
export TSP_KEY=tsp_test_...               # never commit this
```

### 1. Check your key

```sh
curl -sS "$TSP_API/whoami" -H "Authorization: Bearer $TSP_KEY"
```

```json
{ "product_id": "0193...", "product_name": "Cloud Hosting", "account_prefix": "HOST", "key_id": "abcd..." }
```

A `401` means the key is wrong, revoked or expired. See
[authentication](authentication.md).

### 2. Create (or update) the payer

Use your own customer id in the path. This call is an upsert, so you can run
it every time the customer's details change.

```sh
curl -sS -X PUT "$TSP_API/payers/cust_1042" \
  -H "Authorization: Bearer $TSP_KEY" \
  -H "Content-Type: application/json" \
  -d '{"display_name": "Wanjiku Kamau", "email": "wanjiku@example.co.ke", "phones": ["0712345678"]}'
```

The response (`201` the first time, `200` after that) includes
`account_number`, for example `HOST-1042`. Show it in your customer's
billing page: they can pay to it at any time, even without a bill.

### 3. Ask for money

```sh
curl -sS -X POST "$TSP_API/payment-requests" \
  -H "Authorization: Bearer $TSP_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: invoice-INV-2026-0042" \
  -d '{"payer": "cust_1042", "reference": "INV-2026-0042", "description": "Cloud Hosting, December 2026", "amount": 1500}'
```

```json
{
  "object": "payment_request",
  "id": "0193...",
  "reference": "INV-2026-0042",
  "amount": 1500,
  "amount_paid": 0,
  "outstanding": 1500,
  "status": "open",
  "pay_url": "https://pay.../k3j...",
  "paybill": { "business_number": "123456", "account_number": "HOST-1042" },
  "...": "..."
}
```

Store the `id` against your invoice. If the payer already had credit, it is
applied straight away, so the request may come back `partially_paid` or even
`paid`. That is normal; see [money rules](money-rules.md).

If the call times out, send exactly the same request again with the same
`Idempotency-Key`. You get the original request back, never a second bill.

### 4. Send the customer to `pay_url`

Redirect the customer, or email or SMS them the link. The hosted page shows
the bill, the amount still owed and how to pay it by M-Pesa. You don't build
any payment screens. Customers who prefer the M-Pesa menu can also pay to the
paybill with their account number directly.

### 5. Receive the webhook

When the money arrives, Talksasa Pay `POST`s events to your webhook URL (set
by Talksasa staff), for example:

```
POST /hooks/talksasa-pay
Talksasa-Event-Id: 0193...
Talksasa-Event-Type: payment_request.paid
Talksasa-Signature: t=1795510800,v1=65d85ffe...

{"object":"event","id":"0193...","type":"payment_request.paid","api_version":"2026-11-24","created_at":"...","data":{ ...the payment request... }}
```

Verify the signature, store the event id, answer `200` quickly, then process
it. [Webhooks](webhooks.md) has the verification code for PHP and Go.

### 6. Confirm by reading the resource

Before you mark the invoice paid, read the request back. This protects you
from events that arrive late or out of order.

```sh
curl -sS "$TSP_API/payment-requests/0193..." -H "Authorization: Bearer $TSP_KEY"
```

Act on `status` and `amount_paid` from this response, not on the event type
alone.

### 7. Add the backstop

Webhooks can be delayed when your server is down. Run a small scheduled job
that reads [`GET /events`](events-feed.md) and processes anything you have
not seen. With both in place you never miss a payment.

## Ground rules

- Keys are per product. A key can only ever see and change its own
  product's payers and bills.
- Products ask for money and read money. Only M-Pesa and Talksasa finance
  staff record, reverse or refund it.
- Times are RFC 3339 in UTC (`2026-11-24T09:00:00Z`). Show them to Kenyan
  customers in Africa/Nairobi time.
- Every response carries `X-Request-Id`. Quote it when you ask Talksasa for
  help.
