# Talksasa Pay data model, in plain language

This page explains what the database stores and the rules it enforces. The
SQL lives in `db/migrations/` (`000001_core` to `000004_mpesa`). The
decision behind this model is [ADR 0002](adr/0002-generic-gateway.md).

## The big picture

```
apps ──< charges ──< stk_pushes
  │         ▲  ▲          │
  │         │  └──────────┤ (each push owns one pending transaction)
  │         │ paid by     ▼
  ├──< transactions (the ledger) ──< transaction_events
  ├──< app_api_keys
  └──< events ──< webhook_deliveries ──< webhook_attempts
mpesa_callbacks: every Safaricom callback, stored before it is applied
audit_log: who did what (tamper-evident)
```

- An **app** is a system that collects money through the gateway (Cloud
  Hosting = `HOST`, ISP Billing = `BILL`, ...). Adding one means adding a
  row and issuing an API key. No code changes are needed.
- A **charge** is one request by an app to collect an exact amount, "KES
  1,500 for my reference INV-2041". The gateway knows nothing else about
  the app's customers or invoices.
- A **transaction** is real money that moved: an M-Pesa payment coming in,
  a reversal, or (later) a refund going out.
- An **STK push** is one M-Pesa prompt for a charge.
- An **event** is a change an app must hear about (webhook and feed).

There is no table or column for paying money out *to an app*. See
[ADR 0002](adr/0002-generic-gateway.md).

## Tables

### apps
Name, account prefix (2–6 capital letters, unique, can never change),
webhook URL, webhook signing secret and status (`active` / `suspended`).
The webhook secret is **encrypted** (AES-256-GCM, master key kept outside
the database); after a rotation the previous secret keeps signing for 24
hours. A suspended app's API keys stop working at once, but its money is
still recorded and forwarded (money rule 8).

### app_api_keys
Keys look like `tsp_live_<key id>_<secret>`. We store only the key id and a
keyed hash of the secret, so the full key exists only at the moment it is
issued. An app can have several active keys, which lets it rotate without
downtime. Keys can have an IP allow-list (up to 50 networks), can expire
and can be revoked. A revoked key cannot be un-revoked.

### charges
One request to collect an exact amount:

- **Set by the app, then fixed:** `reference` (unique within the app for
  ever), `amount` (whole KES, 1 to 250,000), optional `description`,
  `metadata` (a JSON object up to 4 KB), `phone` and expiry. The database
  refuses any change to the amount, reference, account number, metadata or
  expiry.
- **Account number.** What the customer types at the paybill, also sent as
  the STK AccountReference (Daraja allows 12 characters). Either the app's
  own (`HOST-1042`, may repeat across the app's charges) or generated:
  prefix, hyphen, a digit, random Crockford base32 characters and a check
  character, exactly 12 characters (`HOST-4K7QZ9M`). The database checks
  that the number carries the app's prefix. A generated number is unique
  among the app's live charges. `account_number_norm` (no hyphen) is the
  lookup key for paybill matching, so `host4k7qz9m` and `HOST 4K7QZ9M`
  match too.
- **Checkout link.** `hosted_token` is the random part of the charge's pay
  page URL.
- **Idempotency.** The app's `Idempotency-Key` and a hash of the request,
  so retrying the same API call never creates a second charge.

Statuses, and the only moves the database allows:

| Status | Meaning | Can move to |
|---|---|---|
| `pending` | Waiting for money | `succeeded`, `needs_review`, `expired`, `cancelled` |
| `needs_review` | Safaricom said paid without a receipt; staff are checking. Never expires. | `succeeded`, or back to `pending` (staff: not paid) |
| `succeeded` | Paid | `reversed` |
| `expired`, `cancelled` | Closed | `succeeded` (late money is never dropped) |
| `reversed` | Staff reversed the payment | nothing |

**A charge can only become `succeeded` by pointing at its payment.** The
database checks that `transaction_id` is a succeeded incoming payment of the
same app, naming this charge, for **exactly** the charge amount, and (for
M-Pesa) carrying a receipt. The paying transaction is set once. A charge
can only become `reversed` once its payment has a reversal transaction.

### transactions (the ledger)
One row per money movement: direction (`in` / `out`), kind (`payment`,
`reversal`, later `refund`), channel (`mpesa_stk`, `mpesa_c2b`,
`mpesa_b2c`, `manual`), M-Pesa receipt, amount, phone, the account number
as the customer typed it, status, the provider's raw message, the app
(`app_id`) and the charge it was for (`charge_id`), when known.

- **Unmatched money** is a transaction with no app (`app_id` NULL): a
  paybill payment whose prefix belongs to no app. Staff assign it to an app
  once; `app_id` can be set but never changed.
- **A second payment for a charge** (the customer paid twice) names the
  charge in `charge_id`, but is not the charge's `transaction_id`. It is
  reported to the app as `payment.received` with `duplicate_of_charge`.

The rules, enforced by the database itself:

- **A receipt is recorded once.** All M-Pesa channels share one receipt
  space, so the same receipt can't appear twice, even if it arrives once by
  STK callback and again by paybill notice. A repeat callback changes
  nothing.
- **Nothing is ever deleted.** Even the database owner gets an error.
- **Amounts never change.** Nor do direction, kind, channel, the reason or
  who created the row. Provider details such as the receipt, the app and
  the charge can be filled in once (a pending STK row gets its receipt when
  the callback arrives) but never altered.
- **A succeeded M-Pesa payment always has its receipt.**
- Status only moves forward: `pending` → `succeeded` / `failed` /
  `cancelled` / `needs_review`, and `needs_review` → a final state.
- **Corrections are new rows.** A reversal is a new `out` row pointing at
  the original succeeded payment, for its full amount, with the same app
  and charge. It carries a reason, the name of the person who made it, and
  Safaricom's reversal receipt. A payment can be reversed only once.
- Every status change is written to `transaction_events`, together with the
  worker or person who caused it.

### stk_pushes
One row per M-Pesa prompt for a charge: phone, amount (always the charge
amount), Safaricom's request ids, status (`initiating`, `sent`,
`succeeded`, `failed`, `unknown`; the API shows `unknown` as
`needs_review`), result code and description, and when to check with
Safaricom next. Each push owns the pending `mpesa_stk` transaction that
its callback (or staff, after a status check) completes. **At most one push
per charge is live** (`initiating` or `sent`) at a time, so a customer's
phone can't be spammed. `/v1` pushes are idempotent on the app's key.

### mpesa_callbacks
The inbox. Every callback Safaricom sends (STK results and paybill
notices) is stored verbatim, with its processing job, **before** we answer
Safaricom. Processing can be retried; nothing Safaricom sent is lost. Rows
are never deleted; only the processing outcome columns change.

### events, webhook_deliveries, webhook_attempts
`events` is the transactional outbox. Every change an app must hear about
is written here **in the same database transaction** as the change, so a
committed change can never miss its event. Rows are immutable. The
`/v1/events` feed returns only events whose writing transaction is older
than every still-running one, so a consumer's cursor can never skip an
event that committed late.

Each event has one `webhook_deliveries` row (its delivery state: pending,
delivered, failed or skipped) and one `webhook_attempts` row per HTTP try.
A delivered webhook can't change status again. Attempts are diagnostic
only and may be deleted by the retention job.

### audit_log
Every sensitive action is recorded: app created, key issued or revoked,
webhook URL changed, needs-review resolution, unmatched payment assigned,
reversal, staff changes and every other admin action. Each row includes a
fingerprint (SHA-256) of the row before it, forming a chain. Editing or
deleting a past row breaks the chain, and `talksasa-pay admin audit verify`
reports where.

### Staff accounts (admin panel)
`admin_users`, `admin_invitations`, `admin_sessions`, `admin_recovery_codes`
(migration `000002_admin`). Roles are `viewer`, `finance`, `approver` and
`admin`. Passwords are stored only as argon2id hashes, the authenticator
(TOTP) secret is encrypted with the master key, and session, setup-link and
recovery-code tokens are stored only as fingerprints, so a database copy
cannot be used to sign in. The database refuses to:

- delete any of these rows (staff are disabled, sessions revoked);
- mark an account active without a password and an authenticator;
- send an account back to "invited" without wiping its old credentials;
- change a session's expiry or stage, or revive a revoked session or used
  link.

See `docs/runbooks/admin-access.md` for how staff are onboarded and
offboarded.

## The money rules, as the database sees them

The full rules for app developers are in
[docs/api/charges.md](api/charges.md). In database terms:

1. A charge is created `pending`, unpaid, with an account number carrying
   its app's prefix.
2. It becomes `succeeded` only by pointing at a succeeded, receipted payment
   of the same app and charge for exactly its amount.
3. Each M-Pesa receipt is one transaction, once.
4. Money for an expired or cancelled charge may still succeed it; money for
   a succeeded charge is its own transaction naming the charge.
5. Money with an unknown prefix is a transaction with no app, until staff
   assign it.
6. A reversal is a new `out` transaction for the full amount of one
   succeeded payment; the charge then moves to `reversed`.

After the load and storm tests these invariants are checked by
`cmd/loadtest/invariants.sql` (also run by `make smoke`).

## App isolation

Every query takes the app id from the authenticated API key, never from the
request. On top of that, the foreign keys include the app id, so the
database cannot store a transaction or prompt for app A that points at a
charge of app B, even if application code tried.

## Money

All amounts are whole Kenya shillings stored as integers (`bigint`). There
are no floats anywhere. Inputs such as `"100.50"` are rejected rather than
rounded.
