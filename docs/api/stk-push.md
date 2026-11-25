# M-Pesa payment prompts (STK push)

Instead of sending the customer to the checkout page, your app can ask
Talksasa Pay to show the payment straight on the customer's phone: M-Pesa
pops up "Pay KES 1,500 to Talksasa", the customer enters their PIN, done.
Safaricom calls this an STK push; customers know it as the M-Pesa prompt.

The hosted checkout page offers the same prompt (a phone field and a **Send
payment prompt** button), so if you redirect to `checkout_url` you get this
for free. Use the API when you want a "Pay now" button inside your app.

## The one rule

**Mark an invoice paid only when `charge.succeeded` arrives** (or when a
re-read charge says `status: succeeded`). Never on the response to a
prompt, never on `stk_push.sent`, and never on `stk_push.needs_review` or
`charge.needs_review`. A charge is paid only when Safaricom has confirmed it
with an M-Pesa receipt.

## The flow

```
 Your app                Talksasa Pay                   Safaricom (Daraja)        Customer's phone
 --------                ------------                   ------------------        ----------------
 POST /charges {reference, amount, phone}
   (or POST /charges/{id}/stk-push {phone})
   ---------------------->  check the charge is pending
                            record the prompt (initiating)
                            --------------------------->  STK push request
                            <---------------------------  accepted (CheckoutRequestID)
   <---------------------- 201 charge, latest_stk_push     ------------------------>  "Pay KES 1,500?"
                           {status: "sent"}                                            customer enters PIN
   <~~ stk_push.sent
                                                        <------------------------
                            <---------------------------  callback: ResultCode 0 + receipt
                            record the receipt
   <~~ stk_push.succeeded
   <~~ charge.succeeded          <- mark your invoice paid HERE

 If the customer cancels / times out / has no funds:
                            <---------------------------  callback: ResultCode 1032 / 1037 / 1 ...
   <~~ stk_push.failed {result_code, result_desc}          (nothing was charged; the charge stays pending)

 If Safaricom never calls back:
                            after ~60 s: STK query  ---->
                            <---------------------------  "failed"   -> stk_push.failed
                            <---------------------------  "success" but no receipt
   <~~ stk_push.needs_review
   <~~ charge.needs_review                                 staff confirm it from the M-Pesa portal,
                                                           then charge.succeeded
                                                           (or the charge goes back to pending)
```

`<~~` is a webhook (and an entry in the [events feed](events-feed.md)).

## Prompt when you create the charge

Add `phone` to `POST /charges`. The charge is created and the first prompt
is sent in the same call:

```sh
curl -sS -X POST "$TSP_API/charges" \
  -H "Authorization: Bearer $TSP_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: invoice-INV-2026-0042" \
  -d '{"reference": "INV-2026-0042", "amount": 1500, "phone": "0712345678"}'
```

The response is the charge (`201`), with the prompt in `latest_stk_push`.
The charge is created even if the prompt could not be sent:
`latest_stk_push` is then the `failed` prompt (see its `result_desc`), or
`null` when M-Pesa prompts are not available. Either way, prompt again later
or show the paybill steps.

## Prompt an existing charge

```sh
curl -sS -X POST "$TSP_API/charges/0193.../stk-push" \
  -H "Authorization: Bearer $TSP_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: invoice-INV-2026-0042-stk-2" \
  -d '{"phone": "0712345678"}'
```

- `phone`: the Safaricom number to prompt, as `07XXXXXXXX`, `01XXXXXXXX`,
  `2547XXXXXXXX` or `+2547XXXXXXXX`. It need not be the phone you gave when
  you created the charge.
- The amount is always the **charge's amount**. You can't choose it.
- Only `pending` charges can be prompted (`409 not_payable` otherwise).
- **Only one prompt per charge can be live.** While a prompt is on the
  phone, another call gets `409 stk_in_progress` (the live prompt's id is
  in `detail`). This stops a double click from sending two prompts.
- `Idempotency-Key` is required.

Response `202 Accepted`:

```json
{
  "object": "stk_push",
  "id": "0193a5f2-0000-7000-8000-0000000000cc",
  "charge_id": "0193a5f2-0000-7000-8000-0000000000aa",
  "transaction_id": "0193a5f2-0000-7000-8000-0000000000dd",
  "status": "sent",
  "amount": 1500,
  "phone": "2547****5678",
  "checkout_request_id": "ws_CO_251120261015001234",
  "result_code": null,
  "result_desc": null,
  "created_at": "2026-11-25T07:15:00Z",
  "updated_at": "2026-11-25T07:15:01Z",
  "completed_at": null
}
```

`202` means the prompt was sent, nothing more. Tell the customer "Check
your phone and enter your M-Pesa PIN", then wait for events.

Read a prompt at any time with `GET /stk-pushes/{id}`, or read the charge:
its `latest_stk_push` is the newest prompt.

### Statuses

| `status` | Meaning | What your app shows |
|---|---|---|
| `initiating` | Being sent to Safaricom. | "Check your phone" |
| `sent` | On the customer's phone. | "Check your phone" |
| `succeeded` | Paid: Safaricom sent a receipt. `charge.succeeded` comes with it. | Wait for `charge.succeeded`, then "Paid" |
| `failed` | Not paid. `result_code` and `result_desc` say why. Nothing was charged, and the charge is still `pending`. | `result_desc`, a **Try again** button and the paybill steps |
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

## Trying again after a failure

A failed prompt doesn't fail the charge. Prompt it again with a **new**
`Idempotency-Key` (the old key just returns the failed prompt), for example
`invoice-INV-2026-0042-stk-2`, `-stk-3`... Or let the customer pay at the
paybill with the charge's account number.

## When the customer pays by paybill instead

If the customer pays at the paybill with the charge's account number and
exactly the charge's amount while a prompt is waiting, that payment pays
the charge: you get `charge.succeeded`, and the prompt is marked
`succeeded`. Your code needs no special case: act on `charge.succeeded`.

If the customer ends up paying twice (the prompt and the paybill, with
different receipts), both payments are recorded. The first pays the charge;
the second reaches you as `payment.received` with `duplicate_of_charge`
set. See [charges](charges.md#6-late-money-is-never-dropped).

## Events

| Type | `data` | When |
|---|---|---|
| `stk_push.sent` | STKPush | The prompt reached Safaricom and is on the phone. **Not paid.** |
| `stk_push.succeeded` | STKPush | Safaricom confirmed it with a receipt. Comes with `charge.succeeded`; act on that. |
| `stk_push.failed` | STKPush | The prompt failed: see `result_code` / `result_desc`. The charge stays `pending`. |
| `stk_push.needs_review` | STKPush | Success without a receipt; staff are checking. Comes with `charge.needs_review`. **Not paid.** |

Events can arrive out of order: `charge.succeeded` may come before
`stk_push.succeeded`. Act on the charge's status, as always.

## Errors

| Status | `code` | Meaning | What to do |
|---|---|---|---|
| 409 | `stk_in_progress` | A prompt for this charge is already live. | Show "Check your phone". Don't send another. |
| 409 | `not_payable` | The charge is succeeded, in review, cancelled or expired. | Re-read the charge; show its status. |
| 409 | `idempotency_conflict` | The key was used before with a different phone or charge. | Use a new key for a new prompt. |
| 422 | `validation_failed` | Not a valid Safaricom number (`errors[].field = "phone"`). | Ask for the number again. |
| 429 | `rate_limited` | Too many calls. | Wait `Retry-After`, then retry with the same key. |
| 502 | `mpesa_error` | Safaricom refused or failed to send the prompt. Nothing was charged. | Don't retry automatically. Offer the paybill, or let the customer try again later (with a new key). |
| 503 | `mpesa_unavailable` | M-Pesa prompts are not enabled in this environment. | Show the paybill instructions. |
| 503 | `overloaded` / `timeout` | Talksasa Pay was busy, or the request timed out. | Retry with the **same** key (see below). |

## Idempotency

- Every prompt call needs an `Idempotency-Key`. The same key returns the
  same prompt (`200`, `Idempotent-Replayed: true`) instead of sending
  another.
- After a timeout or `503 timeout`, **retry with the same key**. A new key
  could put a second prompt on the customer's phone (the one-live-prompt
  rule catches most of these, but don't rely on it).
- To try again after a prompt **failed**, use a **new** key.
- The PHP and Go SDKs send a key automatically and reuse it on their own
  retries; they never retry `mpesa_error` or `mpesa_unavailable`.

## SDKs

```php
$charge = TalksasaPay::createCharge(reference: $invoice->number, amount: 1500, phone: '0712345678',
    idempotencyKey: "invoice-{$invoice->id}");
$push = TalksasaPay::stkPush($charge->id, '0712345678');   // prompt again later
```

The method names follow the PHP SDK; see the `clients/php` README for the
exact signatures. A complete Laravel "Pay now" button (controller,
listeners, retry and paybill fallback) is in the
[Laravel guide](laravel.md#pay-now-button).

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

1. Create a charge for a small amount (for example KES 1) with the test
   phone number listed on the Daraja portal's test credentials page, or
   your own Safaricom line if the sandbox prompts it. Sandbox prompts don't
   move real money.
2. Approve it on the phone. Then, on new charges, cancel one prompt and
   ignore another until it times out. Check that your app shows paid, "try
   again" and the paybill fallback, and that it only marks the invoice paid
   on `charge.succeeded`.

Sandbox behaviour changes from time to time (Safaricom sometimes stops
sending sandbox callbacks; then you will see `needs_review` or `failed`
after the status check instead of an immediate result). That is exactly the
path your app must handle anyway.
