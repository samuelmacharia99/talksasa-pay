# Money rules

These rules decide where every shilling goes. They are the same for every
product, and Talksasa Pay applies them, not your code. Your job is to read
the result and update your own records.

## Amounts

- All amounts are **integers in Kenya shillings (KES)**. There are no cents
  and no decimals anywhere in the API. `1500` means KES 1,500.
- A request's `amount` is 1 to 1,000,000,000.
- Do your own sums with integers too. Never convert to floating point.

## What products can and can't do

Products can:

- create and update payers,
- create payment requests (bills) and cancel unpaid ones,
- read payers, requests, transactions and events.

Products can **never** record a payment, mark a bill paid, move money between
bills or payers, change a credit balance, or refund. Money only enters the
ledger from M-Pesa itself or from Talksasa finance staff, and every change is
audited. If a customer says they paid and Talksasa Pay doesn't show it, send
them to Talksasa support with their M-Pesa receipt number; don't mark the
invoice paid in your product.

## Where a payment goes

Each payer has one account number (for example `HOST-1042`). A payment is
applied in this order:

1. **The bill it was made for.** A payment started from a bill's `pay_url`
   (or an M-Pesa prompt for that bill) pays that bill first, if it is still
   open or partially paid.
2. **The payer's other unpaid bills, oldest first** (by creation time).
3. **Credit.** Whatever is left becomes credit on the payer's account
   (`credit_balance`).

A payment made straight to the paybill with the account number, without a
specific bill, skips step 1: it pays the oldest unpaid bills first, then the
rest becomes credit.

A bill never receives more than it asks for. Money sent for a bill that has
since been cancelled or has expired is never rejected: it goes to the
payer's other bills and then to credit.

The `allocations` on a transaction show exactly where its money went. A line
with `payment_request_id: null` is the credit balance.

### Example

A payer has two open bills: A for 1,000 (older) and B for 500. They pay 2,000
from bill B's pay page.

| Step | To | Amount |
|---|---|---|
| 1 | Bill B (the one they paid from) | 500, now `paid` |
| 2 | Bill A (oldest other unpaid bill) | 1,000, now `paid` |
| 3 | Credit | 500 |

You receive `transaction.allocated`, `payment_request.paid` twice, and
`payer.credit_changed` with `delta: 500`.

## Credit is used automatically

When you create a new payment request, any credit the payer holds is applied
to it at once, up to the bill's amount. So `POST /payment-requests` can
return a request that is already `partially_paid` or `paid`. Check `status`
and `outstanding` in the response; don't assume a new bill is unpaid. You
also receive `payer.credit_changed` with a negative `delta`.

You can't choose to keep credit unused, and you can't move credit yourself.

## Partial payments

Customers may pay a bill in several parts. While some but not all of it is
paid, its status is `partially_paid`, `amount_paid` is what has arrived, and
`outstanding` is what is left. Only treat a bill as settled when `status` is
`paid` (equivalently, `outstanding` is 0).

## Statuses

| Status | Meaning | Can change to |
|---|---|---|
| `open` | Nothing paid yet. | `partially_paid`, `paid`, `cancelled`, `expired` |
| `partially_paid` | Some paid, some outstanding. | `paid`, or `open` after a reversal |
| `paid` | Fully paid. | `partially_paid` or `open` after a reversal |
| `cancelled` | Cancelled before anything was paid. | Never changes again |
| `expired` | Passed `expires_at` with nothing paid. | Never changes again |

## Cancelling

You can cancel a request only while it is `open` **and nothing has been paid
on it**. Cancelling a request that is already cancelled returns it unchanged
(safe to retry). Anything else is `409 not_cancellable`: once money is on a
bill, only Talksasa finance can deal with it.

## Expiry

`expires_at` is optional (at most one year ahead). When it passes, a request
expires **only if nothing has been paid on it**. A partially paid request
never expires; it stays open for the rest. An expired request no longer
accepts payments for itself, but money sent for it still reaches the payer's
other bills or credit (see above).

## Amounts can't be changed

A request's `amount`, `reference` and payer are fixed once created. To bill a
different amount, cancel the request (if nothing has been paid) and create a
new one, with a new `Idempotency-Key`. If something has been paid, create a
second request for the difference, or talk to Talksasa finance.

Only one `open` or `partially_paid` request may exist per `reference`. A
second one gets `409 duplicate_reference`. After cancelling, you can reuse
the reference.

## Reversals

Sometimes a payment is reversed after it was applied (for example M-Pesa
reverses a transaction). Talksasa staff then record a new reversal transaction
that takes back exactly what the original payment applied, bill by bill and
from credit. Nothing is edited or deleted; the original payment stays in the
history.

Bills that lose money **reopen**: a `paid` bill becomes `partially_paid` or
`open` again, and you receive `transaction.reversed` and
`payment_request.reopened`. Your product must handle this: for example, put
the invoice back to unpaid and decide whether to suspend the service again.

A credit balance never goes negative. If part of the reversed payment went to
the payer's credit and that credit has since been spent on a newer bill, the
reversal is refused as a whole: nothing is recorded, no bill reopens, and no
events are sent. Talksasa finance then resolves it by hand. You only hear
about a reversal once it has actually been recorded.

## Duplicates

Each M-Pesa receipt is recorded once. If M-Pesa notifies Talksasa Pay twice
about the same payment, the second notice is ignored, so a customer is never
credited twice for one payment.
