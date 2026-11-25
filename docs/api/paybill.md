# Paybill payments

Many customers prefer to pay from the M-Pesa menu: **Lipa na M-Pesa →
Pay Bill**, the Talksasa business number, an account number and the amount.
Safaricom then sends Talksasa Pay a notice of the payment (a "C2B
confirmation"), and Talksasa Pay routes it to your app by the account
number. This page explains how.

## Your account prefix

Every app has an **account prefix** of 2-6 capital letters, for example
`HOST` for Cloud Hosting. `GET /whoami` returns it as `account_prefix`. An
account number is your prefix, a hyphen, a digit, then capital letters and
digits, at most 12 characters: `HOST-1042`, `HOST-4K7QZ9M`.

Any paybill payment whose account number starts with your prefix (followed
by a digit) belongs to your app. Talksasa staff choose the prefix when they
create your app, and it never changes.

## Account numbers on charges

Every charge has an `account_number`, shown to the customer with the
business number in the charge's `paybill` field and on its checkout page:

```json
"account_number": "HOST-4K7QZ9M",
"paybill": { "business_number": "123456", "account_number": "HOST-4K7QZ9M" }
```

- **Generated** (when you send no `account_number`): exactly 12
  characters, with a check character at the end. It belongs to one charge
  only, so a payment to it can only pay that charge. A mistyped character
  or two swapped neighbours fail the check and can't pay the wrong charge.
- **Your own** (when you send `account_number`, e.g. your customer number
  `HOST-1042`): you may reuse it on several charges. It has no check
  character, so pick numbers that are hard to mistype.

Customers may type the account number in any case, with or without the
hyphen or spaces.

## What happens to a paybill payment

| The payment | What Talksasa Pay does | You get |
|---|---|---|
| Your prefix; the account number and the **exact amount** match exactly **one** waiting charge (`pending` or `needs_review`) | It pays that charge. If a prompt for the charge was waiting, the prompt is marked `succeeded` too. | `charge.succeeded` |
| Your prefix; anything else: a different amount, a charge that already succeeded, expired or was cancelled, two or more waiting charges with that account number and amount, or a number you never used | Recorded and forwarded to you; no charge changes. | `payment.received` |
| A prefix that belongs to **no app** (a typo in the prefix, or an old number) | Recorded as **unmatched** and held for Talksasa staff. | Nothing yet. If staff find out it's yours and assign it, `payment.received`. |
| A notice for another business number | Not recorded; Talksasa staff are alerted. | Nothing |

Every payment is recorded once, by its M-Pesa receipt. Talksasa Pay never
splits a payment, never pays part of a charge and keeps no balances.

### `payment.received`

`data` is the transaction, with the M-Pesa details you need to decide what
the money pays:

| Field | Meaning |
|---|---|
| `id` | The transaction id. Use it as your idempotency key when you apply the money in your system. |
| `amount` | What the customer paid, in KES. |
| `account_number` | Exactly what the customer typed. |
| `phone` | The paying phone, masked (`2547****5678`). |
| `receipt` | The M-Pesa receipt. |
| `charge_id`, `duplicate_of_charge` | Set when Talksasa Pay knows the money was a second payment for a charge that had already succeeded (for example, a prompt approved after the customer had paid at the paybill); null otherwise. |
| `channel` | `mpesa_c2b` for paybill payments; `mpesa_stk` when a prompt was paid twice. |

What it pays is up to you. Typical choices: find the customer by account
number and apply it to their oldest unpaid invoice; keep it as a balance on
their account in your system; or refund it. Talksasa Pay won't do any of
this for you.

## Unmatched payments and assignment

A payment whose prefix matches no app is held as unmatched. Talksasa
finance staff see it in the admin panel, work out whose it is (from the
account number, phone and amount, or because a customer calls) and assign
it to an app. Assignment is audited with the staff member's name and
reason, and can be done once. The app then gets `payment.received` as
above.

If a customer tells you they paid with a wrong account number, ask them for
the M-Pesa receipt and send it to Talksasa support.

## Prompts and paybill together

A customer with a prompt on their phone may still pay at the paybill. If
they pay the charge's account number and exact amount, that pays the
charge, and the waiting prompt is marked `succeeded`. If they then also
approve the prompt (two receipts), the second payment reaches you as
`payment.received` with `duplicate_of_charge` set.

## Testing

Locally, the fake Daraja simulates a paybill payment:

```sh
curl -X POST 127.0.0.1:8090/fake/c2b -d '{"bill_ref":"HOST-4K7QZ9M","amount":1500}'
```

Use a charge's `account_number` and `amount` to pay it, another amount to
get `payment.received`, or a prefix no app has to create an unmatched
payment. On staging, the Daraja sandbox's C2B simulator does the same with
the sandbox short code.
