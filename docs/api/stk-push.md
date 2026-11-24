# M-Pesa payment prompts (STK push)

Instead of sending the customer to the pay page, your product can ask
Talksasa Pay to show the payment straight on the customer's phone: M-Pesa
pops up "Pay KES 1,500 to Talksasa", the customer enters their PIN, done.
Safaricom calls this an STK push; customers know it as the M-Pesa prompt.

The hosted pay page offers the same prompt (a phone field and a **Send
payment prompt** button), so if you redirect to `pay_url` you get this for
free. Use the API when you want a "Pay invoice" button inside your product.

## The one rule

**Mark a bill paid only when `payment_request.paid` arrives** (or when a
re-read payment request says `status: paid`). Never on the `202` from
`stk-push`, never on `stk_push.sent`, and never on `stk_push.needs_review`.
A bill is paid only when Safaricom has confirmed it with an M-Pesa receipt.

## The flow

```
 Your product            Talksasa Pay                   Safaricom (Daraja)        Customer's phone
 ------------            ------------                   ------------------        ----------------
 POST /payment-requests/{id}/stk-push {phone}
   ---------------------->  check the bill is payable
                            record the prompt (initiating)
                            --------------------------->  STK push request
                            <---------------------------  accepted (CheckoutRequestID)
   <---------------------- 202 {status: "sent"}            ------------------------>  "Pay KES 1,500?"
   <~~ stk_push.sent                                                                   customer enters PIN
                                                        <------------------------
                            <---------------------------  callback: ResultCode 0 + receipt
                            record receipt, apply money
   <~~ stk_push.succeeded
   <~~ transaction.allocated
   <~~ payment_request.paid      <- mark your invoice paid HERE

 If the customer cancels / times out / has no funds:
                            <---------------------------  callback: ResultCode 1032 / 1037 / 1 ...
   <~~ stk_push.failed {result_code, result_desc}          (nothing was charged)

 If Safaricom never calls back:
                            after ~60 s: STK query  ---->
                            <---------------------------  "failed"   -> stk_push.failed
                            <---------------------------  "success" but no receipt
   <~~ stk_push.needs_review                               staff confirm it from the M-Pesa portal,
                                                           then stk_push.succeeded + payment_request.paid
                                                           (or stk_push.failed)
```

`<~~` is a webhook (and an entry in the [events feed](events-feed.md)).

## Send a prompt

```sh
curl -sS -X POST "$TSP_API/payment-requests/0193.../stk-push" \
  -H "Authorization: Bearer $TSP_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: invoice-INV-2026-0042-stk-1" \
  -d '{"phone": "0712345678"}'
```

- `phone`: the Safaricom number to prompt, as `07XXXXXXXX`, `01XXXXXXXX`,
  `2547XXXXXXXX` or `+2547XXXXXXXX`. It does not have to be one of the
  payer's saved phones.
- The amount is always the request's **full outstanding balance**. You
  can't choose it. A balance above KES 250,000 can't be prompted
  (`409 amount_too_large`); show the paybill instructions instead.
- **Only one prompt per request can be live.** While a prompt is on the
  phone, another call gets `409 stk_in_progress` (the live prompt's id is in
  `detail`). This stops a double click from sending two prompts.
- `Idempotency-Key` is required.

Response `202 Accepted`:

```json
{
  "object": "stk_push",
  "id": "0193a5f2-0000-7000-8000-0000000000cc",
  "payment_request_id": "0193...",
  "transaction_id": "0193...",
  "status": "sent",
  "amount": 1500,
  "phone": "2547****5678",
  "checkout_request_id": "ws_CO_241120261015001234",
  "result_code": null,
  "result_desc": null,
  "created_at": "2026-11-24T07:15:00Z",
  "updated_at": "2026-11-24T07:15:01Z",
  "completed_at": null
}
```

`202` means the prompt was sent, nothing more. Tell the customer "Check your
phone and enter your M-Pesa PIN", then wait for events.

Read a prompt at any time with `GET /stk-pushes/{id}`.

### Statuses

| `status` | Meaning | What your product shows |
|---|---|---|
| `initiating` | Being sent to Safaricom. | "Check your phone" |
| `sent` | On the customer's phone. | "Check your phone" |
| `succeeded` | Paid: Safaricom sent a receipt. `payment_request.paid` (or `partially_paid`) follows. | Wait for `payment_request.paid`, then "Paid" |
| `failed` | Not paid. `result_code` and `result_desc` say why. Nothing was charged. | `result_desc`, a **Try again** button and the paybill steps |
| `needs_review` | Safaricom said "success" but sent no receipt. Talksasa staff check the M-Pesa portal and confirm or fail it, usually within a working day. | "We're confirming your payment with Safaricom. Please don't pay again." Not paid yet. |

### Safaricom result codes you will see

| `result_code` | Usual meaning |
|---|---|
| `1032` | The customer cancelled the prompt. |
| `1037` | The phone could not be reached, or the customer did not answer in time. |
| `1` | Insufficient M-Pesa balance. |
| `2001` | Wrong PIN. |
| `1001` | Another M-Pesa transaction was in progress on the phone. |
| `1019` | The prompt expired before it was answered. |
| other | Show `result_desc`; offer the paybill. |

Treat the code as information for support. Show `result_desc` (or your own
wording) to the customer, and never parse it to decide money.

## When the customer pays by paybill instead

If the customer pays the paybill by hand (or Safaricom also sends a paybill
notice for the prompt payment) for exactly the prompt's amount while the
prompt is waiting, Talksasa Pay treats it as the prompt's payment: you get
`stk_push.succeeded` and `payment_request.paid` for the prompt's bill as
usual. If the customer ends up paying twice, both payments are recorded and
the second goes to the customer's next unpaid bill or credit
(`transaction.allocated`, `payer.credit_changed`).

## Events

| Type | `data` | When |
|---|---|---|
| `stk_push.sent` | STKPush | The prompt reached Safaricom and is on the phone. |
| `stk_push.succeeded` | STKPush | Safaricom confirmed it with a receipt. Comes with `transaction.allocated` and `payment_request.paid` / `partially_paid`. |
| `stk_push.failed` | STKPush | The prompt failed: see `result_code` / `result_desc`. |
| `stk_push.needs_review` | STKPush | Success without a receipt; staff are checking. **Not paid.** |

Events can arrive out of order: `payment_request.paid` may come before
`stk_push.succeeded`. Act on the payment request's status, as always.

A customer can also ignore the prompt and pay at the paybill. That payment
pays the bill normally (`payment_request.paid`), and the prompt later fails
on its own. Your code needs no special case for this.

## Errors

| Status | `code` | Meaning | What to do |
|---|---|---|---|
| 409 | `stk_in_progress` | A prompt for this request is already live. | Show "Check your phone". Don't send another. |
| 409 | `not_payable` | The request is paid, cancelled or expired. | Re-read the request; show its status. |
| 409 | `amount_too_large` | Outstanding balance above KES 250,000. | Show the paybill instructions. |
| 409 | `idempotency_conflict` | The key was used before with a different phone. | Use a new key for a new phone. |
| 422 | `validation_failed` | Not a valid Safaricom number (`errors[].field = "phone"`). | Ask for the number again. |
| 429 | `rate_limited` | Too many calls. | Wait `Retry-After`, then retry with the same key. |
| 502 | `mpesa_error` | Safaricom refused or failed to send the prompt. Nothing was charged. | Don't retry automatically. Offer the paybill, or let the customer try again later (with a new key). |
| 503 | `mpesa_unavailable` | M-Pesa prompts are not enabled in this environment. | Show the paybill instructions. |
| 503 | `overloaded` / `timeout` | Talksasa Pay was busy, or the request timed out. | Retry with the **same** key (see below). |

## Idempotency

- Every call needs an `Idempotency-Key`. The same key returns the same
  prompt (`200`, `Idempotent-Replayed: true`) instead of sending another.
- After a timeout or `503 timeout`, **retry with the same key**. A new key
  could put a second prompt on the customer's phone (the one-live-prompt
  rule catches most of these, but don't rely on it).
- To try again after a prompt **failed**, use a **new** key: the old key
  just returns the failed prompt. A good pattern is
  `invoice-<id>-stk-<attempt>`, or let the SDK generate one per click.
- The PHP and Go SDKs send a key automatically and reuse it on their own
  retries; they never retry `mpesa_error` or `mpesa_unavailable`.

## SDKs

```php
$push = TalksasaPay::stkPush($request->id, '0712345678');   // Laravel facade
```

```go
push, _, err := client.STKPush(ctx, requestID, "0712345678", talksasapay.STKPushOptions{})
if talksasapay.IsCode(err, talksasapay.CodeSTKInProgress) { /* already on the phone */ }
```

A complete Laravel "Pay invoice" button (controller, listeners, retry and
paybill fallback) is in the [Laravel guide](laravel.md#pay-invoice-button).

## Testing

**Locally (fake Daraja).** The development stack (`make up`) runs a fake
Daraja that answers by the phone's last digit, so you can test every path
without Safaricom: a number ending in `0` is paid shortly after the prompt
(with a fake receipt), `1` is cancelled by the customer (`1032`), `2` ends
in `needs_review`, `4` is refused (`502 mpesa_error`). The full list is in
the [M-Pesa runbook](../runbooks/mpesa.md#development-the-fake-daraja).

**Safaricom sandbox.** Talksasa staff run a staging environment connected to
the Daraja sandbox (shortcode `174379`). Use your `tsp_test_` key against
staging and:

1. Create a payer and a request for a small amount (for example KES 1).
2. Send a prompt to the test phone number listed on the Daraja portal's
   test credentials page, or to your own Safaricom line if the sandbox
   prompts it. Sandbox prompts don't move real money.
3. Approve it on the phone. Then, on new requests, cancel one prompt and
   ignore another until it times out. Check that
   your product shows paid, "try again" and the paybill fallback, and that
   it only marks the invoice paid on `payment_request.paid`.

Sandbox behaviour changes from time to time (Safaricom sometimes stops
sending sandbox callbacks; then you will see `needs_review` or `failed`
after the status check instead of an immediate result). That is exactly the
path your product must handle anyway.
