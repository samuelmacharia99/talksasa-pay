# ADR 0002: Talksasa Pay is a generic payment gateway (charges, not bills)

- Status: accepted
- Date: 2026-11-25
- Supersedes: [ADR 0001](0001-revenue-only.md)

## Context

Up to now Talksasa Pay was a billing ledger. For each product it stored the
customers (payers), their permanent account numbers and their bills
(payment requests). It applied every payment oldest-bill-first and kept the
change as a credit balance. Each product therefore had to mirror its
customers and invoices into Talksasa Pay, and two systems held the same
customer and invoice data.

The owner reviewed this on 2026-11-25 and asked for Talksasa Pay to work
like Paystack, Flutterwave or Pesapal instead. An app makes one API call:
"collect KES X from this phone for my reference Y". The gateway sends the
M-Pesa prompt and confirms the payment with Safaricom. It then tells the app
the result, with the M-Pesa receipt, by webhook and in the events feed. The
app keeps its own customers and invoices; the gateway knows none of them.

The decisions taken that day:

1. Store **charges** only. Payers, bills, allocations and credit balances
   go.
2. Keep neutral **apps** (formerly "products"): an app holds API keys, a
   webhook URL and its own payments, and nothing else.
3. Paybill (C2B) payments are routed to an app by the **account-number
   prefix** and forwarded to it.
4. A payment with an unknown prefix is **held unmatched** for staff.
5. The hosted pay page stays, as a **checkout link** for each charge.
6. Clean migration baseline: nothing was deployed, so there is no data
   migration.
7. Staff can record **reversals**, and the app is told.

Decisions added on the same day:

- A charge's `reference` is unique within the app for ever.
- Generated account numbers are 12 characters: the prefix, a hyphen, a
  digit, random Crockford base32 characters and a check character
  (`HOST-4K7QZ9M`).
- An app may supply its own account numbers (for example its customer
  number `HOST-1042`), and they may repeat across charges. A paybill payment
  completes a charge only when exactly one live charge matches.
- A charge in `needs_review` never expires.
- Reversing a payment is for the admin role only. Assigning an unmatched
  payment is for the finance and admin roles.
- The paybill (Daraja) credentials are managed from a settings page in the
  admin panel, encrypted in the database. The SOPS bundle stays as the
  fallback.

## Decision

Talksasa Pay is a generic M-Pesa payment gateway on the one Talksasa
paybill. Its records are apps, charges, transactions (the append-only
ledger), STK pushes, M-Pesa callbacks, events and the audit log.

The money rules (stated for app developers in
[docs/api/charges.md](../api/charges.md)):

1. **Charge.** The app sets a whole-KES `amount` (1 to 250,000), its own
   `reference` (unique within the app for ever; a duplicate gets 409) and
   optionally a description, a phone number, up to 4 KB of metadata and an
   expiry (15 minutes to 7 days, default 24 hours). The amount, reference
   and account number never change.
2. **Account number.** Every charge has one: the app's own (starting with
   its prefix) or a generated one. It is the STK AccountReference and what
   the customer types at the paybill.
3. **Paid means a receipt, once.** A charge succeeds only on a Safaricom
   success with an M-Pesa receipt for exactly the charge amount. Each
   receipt is recorded once. An STK success for a different amount records
   nothing and raises an alarm.
4. **Prompts.** A failed prompt does not fail the charge. The app gets
   `stk_push.failed`, the charge stays `pending` and can be prompted again.
   Only one prompt per charge is live at a time.
5. **Lost callback.** After about 60 seconds the gateway asks Safaricom
   (STK Query). A success without a receipt puts the charge in
   `needs_review` until finance or admin staff enter the receipt or mark it
   failed. This is audited.
6. **Late money is never dropped.** Money for a `pending`, `expired` or
   `cancelled` charge makes it `succeeded`. Money for an already
   `succeeded` charge is recorded as its own transaction and reported as
   `payment.received` with `duplicate_of_charge` set.
7. **Paybill.** A known prefix plus an account number and exact amount that
   match exactly one waiting charge pays that charge. A known prefix with no
   match is forwarded as `payment.received`. An unknown prefix is held
   unmatched until staff assign it to an app. A notice for another business
   number is not recorded and raises an alarm.
8. **Inactive app.** Its money is still recorded and forwarded, and staff
   are warned. Its API keys stop working.
9. **Reversal.** Admin only, with a reason, audited. It records a reversal
   transaction against a succeeded payment. The charge becomes `reversed`
   and the app gets `transaction.reversed`. There is no credit or reopening.

The structural protections of ADR 0001 stay in force:

- No table, column or API settles money to an app or to anyone else.
- Outgoing money (`direction = 'out'`) is only reversals and, in a later
  phase, refunds. Named staff create them, never an app API key.
- Apps are created only by Talksasa operators (admin CLI or panel), and
  every creation is audited.

## Consequences

- Removed: payer accounts and phones, payment requests, the per-product
  account counter, allocation batches and lines, credit balances, the
  balance-drift check and `admin ledger check`. Removed from `/v1`:
  `/payers` and `/payment-requests`. Removed from the admin panel: Payers
  and Requests.
- "Product" is renamed "app" everywhere: code, CLI (`admin app ...`), admin
  panel and docs.
- Events are now `charge.succeeded`, `charge.needs_review`,
  `charge.expired`, `charge.cancelled`, `stk_push.sent`, `stk_push.failed`,
  `stk_push.succeeded`, `stk_push.needs_review`, `payment.received` and
  `transaction.reversed`. The outbox, the gap-free feed and webhook signing
  are unchanged.
- Apps must keep their own mapping from `reference` (and `metadata`) to
  their invoices. They must also handle `payment.received`: money that
  arrived for them but paid no charge (paybill payments without a match,
  customers who paid twice).
- Partial payments are no longer applied anywhere. A paybill payment for
  the wrong amount pays no charge and is forwarded as `payment.received`;
  the app decides what it pays.
- Refunding a duplicate payment needs B2C refunds (a later phase). Until
  then the app or staff refund it outside the system.
- Talksasa Pay still runs on one Talksasa paybill and still settles nothing
  to apps. If an app is ever operated by, or collects for, a business other
  than Talksasa, the payment-service-provider question from ADR 0001 comes
  back: that needs legal review and a new ADR first.
