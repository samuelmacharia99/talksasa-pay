# Charges and the money rules

A charge is one request to collect an exact amount: "KES 1,500 for my
invoice INV-2026-0042". These rules decide when a charge counts as paid and
what happens to every shilling that arrives. They are the same for every
app, and Talksasa Pay applies them, not your code. Your job is to read the
result and update your own records.

## What apps can and can't do

Apps can:

- create charges, prompt a customer's phone for them, and cancel them;
- read charges, prompts, transactions and events.

Apps can **never** record a payment, mark a charge paid, change an amount,
move money or refund. Money only enters the ledger from M-Pesa itself or
from Talksasa staff, and every staff action is audited. If a customer says
they paid and Talksasa Pay doesn't show it, send them to Talksasa support
with their M-Pesa receipt number; don't mark the invoice paid in your app.

## The rules

### 1. A charge is fixed when you create it

`POST /charges` takes:

| Field | Required | Rule |
|---|---|---|
| `reference` | yes | Your own reference (invoice number, order id), 1-128 characters. **Unique among all your charges, for ever**: a second charge with the same reference gets `409 duplicate_reference`, even if the first was cancelled or expired. |
| `amount` | yes | Whole KES, 1 to 250,000. |
| `description` | no | Up to 500 characters, shown to the customer on the checkout page. |
| `phone` | no | A Safaricom number to prompt at once (see rule 4). |
| `account_number` | no | Your own account number for this charge (see rule 2). |
| `metadata` | no | Any JSON object up to 4 KB. Echoed back on the charge and in its events. Use it for your own ids. |
| `expires_in` | no | Seconds until a pending charge expires: 900 (15 minutes) to 604800 (7 days). Default 86400 (24 hours). |

The amount, reference and account number never change. To charge a
different amount, cancel the charge and create a new one with a new
reference (for example `INV-2026-0042-2`) and a new `Idempotency-Key`.

### 2. Every charge has an account number

The account number is what the customer types at the paybill, and it is
sent to Safaricom as the prompt's AccountReference.

- **Generated** (the default): 12 characters, your prefix, a hyphen, a
  digit, random characters and a check character, for example
  `HOST-4K7QZ9M`. It identifies exactly one charge, and the check character
  catches a mistyped character or two swapped neighbours.
- **Your own**: send `account_number`, for example your customer number
  `HOST-1042`. It must be your prefix, a hyphen, a digit, then capital
  letters and digits, at most 12 characters. You may use the same account
  number on several charges (one per invoice for the same customer, say).
  A paybill payment then pays a charge only when exactly one waiting charge
  has that account number and amount; otherwise it reaches you as
  `payment.received` (rule 7).

Customers may type the number in any case, with or without the hyphen or
spaces: `host4k7qz9m`, `HOST 4K7QZ9M` and `Host-4K7QZ9M` all match.

### 3. Paid means an M-Pesa receipt, once

A charge becomes `succeeded` only when Safaricom confirms a payment **with
an M-Pesa receipt for exactly the charge amount**. Then:

- `status` is `succeeded`, `succeeded_at` is set, and `transaction_id`
  names the payment (`GET /transactions/{id}` has the receipt);
- you get `charge.succeeded`.

Each receipt is recorded once. If Safaricom notifies Talksasa Pay twice
about the same payment, the second notice changes nothing, so a customer is
never counted twice for one payment.

If Safaricom confirms a prompt for a different amount than the charge
(which should never happen), nothing is recorded and Talksasa staff are
alerted.

### 4. Prompts can fail; the charge stays payable

A prompt (STK push) is one attempt to get the customer to pay. If the
customer cancels it, ignores it or has no funds, you get `stk_push.failed`
and **the charge stays `pending`**. You can prompt it again with
`POST /charges/{id}/stk-push`, or the customer can pay at the paybill. Only
one prompt per charge is live at a time (`409 stk_in_progress`). See
[M-Pesa payment prompts](stk-push.md).

### 5. When Safaricom's answer is lost: needs review

If Safaricom never sends a prompt's result, Talksasa Pay asks Safaricom
after about a minute. If Safaricom then says "paid" but gives no receipt,
the charge goes to `needs_review` and you get `charge.needs_review` and
`stk_push.needs_review`. **This is not paid.** Talksasa finance staff look
the payment up on the M-Pesa portal and either:

- enter the receipt: the charge becomes `succeeded` (`charge.succeeded`),
  or
- mark it not paid: the charge goes back to `pending`.

A charge in `needs_review` never expires. Show the customer "We're
confirming your payment with Safaricom. Please don't pay again."

### 6. Late money is never dropped

- Money for a charge that is `pending`, `expired` or `cancelled` makes it
  `succeeded`, and you get `charge.succeeded`. This happens when a
  customer approves a prompt that was already on their phone when you
  cancelled the charge or it expired. **Your app must handle a charge succeeding after you
  cancelled it**: for example, mark the invoice paid anyway, or refund the
  customer. (A paybill payment typed against a closed charge's account
  number does not reopen it: it reaches you as `payment.received`, see
  rule 7.)
- Money for a charge that has **already succeeded** (the customer paid
  twice) is recorded as its own transaction and reported as
  `payment.received` with `duplicate_of_charge` set to the charge's id. The
  charge itself does not change. You (or Talksasa staff) refund the
  customer; refunds through Talksasa Pay come in a later phase.

### 7. Paybill payments

When a customer pays at the paybill from the M-Pesa menu, Talksasa Pay
looks at the account number they typed:

| What they typed | What happens |
|---|---|
| Your prefix, and the account number **and exact amount** match exactly one waiting (`pending` or `needs_review`) charge | It pays that charge: `charge.succeeded`. |
| Your prefix, but no match (wrong amount, a closed charge, an account number you shared across waiting charges with the same amount, or a number you never used) | Recorded and sent to you as `payment.received`, with the account number, amount, phone and receipt. You decide what it pays. |
| A prefix that belongs to no app | Held as **unmatched**. Talksasa staff find out whose it is and assign it; that app then gets `payment.received`. |

There are no partial payments. A paybill payment for less or more than the
charge pays nothing on that charge; you get it as `payment.received`. See
[paybill payments](paybill.md).

### 8. Suspended apps still receive their money

If Talksasa suspends your app, its API keys stop working at once
(`401`). Money your customers send is still recorded, still succeeds your
charges and is still delivered to your webhook and feed. Talksasa staff
are warned about it.

### 9. Reversals

Sometimes a payment is reversed after it succeeded (for example Safaricom
reverses it). A Talksasa admin then records a **reversal**: a new
transaction (`kind: reversal`, `direction: out`) for the payment's full
amount, with a reason and Safaricom's reversal receipt. Nothing is edited
or deleted; the original payment stays in the history. Then:

- you get `transaction.reversed`, whose `data` is the reversal
  transaction: `reverses_transaction_id` names the payment and `charge_id`
  the charge;
- if the payment had paid a charge, the charge becomes `reversed`.

A reversed charge is never paid again and never reopens. Your app decides
what a reversal means for the invoice: put it back to unpaid, suspend the
service, or create a new charge.

## Money that pays no charge

`payment.received` is money that reached your app without paying a charge.
`data` is the transaction plus `duplicate_of_charge`:

- `duplicate_of_charge` set: the customer paid a charge that had already
  succeeded (rule 6). Refund them, or keep it as a balance in your own system.
- `duplicate_of_charge` null: a paybill payment with your prefix that
  matched no waiting charge (rule 7), or an unmatched payment staff
  assigned to you. Use `account_number`, `amount` and `phone` to decide
  which invoice it pays.

Talksasa Pay doesn't keep balances or apply this money anywhere. What it
pays is your decision, in your system.

## Statuses

| Status | Meaning | Can change to |
|---|---|---|
| `pending` | Waiting for money. Can be prompted, paid at the paybill and cancelled. | `succeeded`, `needs_review`, `expired`, `cancelled` |
| `needs_review` | Safaricom said paid without a receipt; staff are checking. **Not paid.** Never expires. | `succeeded`, or `pending` if staff find it was not paid |
| `succeeded` | Paid. `transaction_id` names the payment. | `reversed` |
| `expired` | Passed `expires_at` while pending. No longer prompted or shown as payable. | `succeeded` (late money) |
| `cancelled` | You cancelled it. | `succeeded` (late money) |
| `reversed` | Staff reversed the payment. | Never changes again |

`paybill` (the manual M-Pesa instructions) is set only while the charge
can be paid (`pending` or `needs_review`) and is null otherwise.

## Cancelling

`POST /charges/{id}/cancel` cancels a `pending` charge and sends
`charge.cancelled`. Cancelling a charge that is already cancelled returns it
unchanged (safe to retry). Anything else is `409 not_cancellable`.

A prompt already on the customer's phone is not withdrawn: if they approve
it, the charge still succeeds (rule 6).

## Expiry

When `expires_at` passes, a `pending` charge becomes `expired` and you get
`charge.expired`. The checkout page stops offering it, and it can no longer
be prompted. A prompt that was already on the phone and is approved later
still succeeds it (rule 6).

## Amounts

- All amounts are **integers in Kenya shillings (KES)**. There are no cents
  and no decimals anywhere in the API. `1500` means KES 1,500.
- Do your own sums with integers too. Never convert to floating point.
