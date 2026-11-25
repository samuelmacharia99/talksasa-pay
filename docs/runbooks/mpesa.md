# Runbook: M-Pesa (Safaricom Daraja)

Talksasa Pay talks to Safaricom's Daraja API for three things:

- **STK push** (the "M-Pesa prompt"): from the app API
  (`POST /v1/charges` with a phone, or `POST /v1/charges/{id}/stk-push`)
  and from the checkout page's **Send payment prompt** button.
- **STK query**: if a prompt's result doesn't arrive within about a minute,
  the worker asks Safaricom.
- **C2B confirmations**: every payment made to the paybill from the M-Pesa
  menu.

Safaricom calls us back at `/cb/mpesa/<callback token>/stk` and
`/cb/mpesa/<callback token>/c2b/confirmation` on the API domain (Caddy
already routes `/cb/*`). Every callback is stored first and then applied by
the worker, so nothing Safaricom sends is lost even if applying it fails.

**Money rule:** a charge is paid only when Safaricom confirms with an M-Pesa
receipt for exactly its amount. A prompt Safaricom calls "successful"
without a receipt goes to **needs review** and waits for a person (see
below). The full rules are in [docs/api/charges.md](../api/charges.md).

**Secrets rule:** consumer key, consumer secret, passkey and the callback
token are secrets. They live only in the SOPS bundle. Never paste them into
chat, tickets, email, shell history or logs. If one leaks, rotate it
(bottom of this page).

## Settings

<!-- settings: see settings.md -->

| Variable | Secret? | Meaning |
|---|---|---|
| `TSP_MPESA_ENV` | no | Empty: M-Pesa off (pay page shows only the paybill; `/v1` stk-push answers `503 mpesa_unavailable`; callbacks not mounted). `sandbox` or `production` selects the Daraja environment. Production deployments must use `production`. |
| `TSP_MPESA_PAYBILL` | no | The paybill shown to customers and used for C2B. |
| `TSP_MPESA_SHORTCODE` | no | Business short code for STK push. Defaults to the paybill; in the sandbox it is Safaricom's test short code `174379`. |
| `TSP_MPESA_CALLBACK_BASE_URL` | no | Public base Safaricom calls, ending in `/`, e.g. `https://<api-domain>/`. HTTPS outside development. |
| `TSP_MPESA_CALLBACK_CIDRS` | no | Safaricom's callback source networks, comma separated. Required with `production`; may be empty in the sandbox. |
| `TSP_MPESA_CONSUMER_KEY_FILE` | yes | Daraja app consumer key. |
| `TSP_MPESA_CONSUMER_SECRET_FILE` | yes | Daraja app consumer secret. |
| `TSP_MPESA_PASSKEY_FILE` | yes | Lipa na M-Pesa Online passkey for the short code. |
| `TSP_MPESA_CALLBACK_TOKEN_FILE` | yes | Random token in the callback path (at least 32 of `A-Z a-z 0-9 _ -`). Daraja does not sign callbacks; this token plus the IP allow-list is what tells a real callback from a forged one. |
| `TSP_MPESA_REQUEST_TIMEOUT`, `TSP_MPESA_QUERY_AFTER`, `TSP_MPESA_GIVE_UP_AFTER` | no | Daraja call timeout (15 s), first status check (60 s), hand to staff after (30 min). Defaults are fine. |

The service refuses to start with an incomplete M-Pesa configuration and
says which variable is wrong. Staging and production read secrets only from
files (`*_FILE`).

## Development: the fake Daraja

Locally you don't need Safaricom. `make up` also starts a fake Daraja (the
`fake-daraja` service on `127.0.0.1:8090`, code in
`internal/mpesa/mpesatest`) and points the api and worker at it, so prompts,
status checks, callbacks and paybill payments all work offline. It answers
by the phone's last digit:

| Phone ends in | Result |
|---|---|
| `0` (and 7-9) | Paid shortly after the prompt, with a fake receipt |
| `1` | Cancelled by the customer (`1032`) |
| `2` | No callback; the status check says success: ends in **needs review** |
| `3` | No callback and the status check never answers: needs review after the give-up time |
| `4` | Safaricom refuses the prompt (`502 mpesa_error`) |
| `5` | Daraja errors but the prompt went out; the success callback still arrives |
| `6` | Paid, and Safaricom sends the same callback twice |

Simulate a paybill (C2B) payment with
`curl -X POST 127.0.0.1:8090/fake/c2b -d '{"bill_ref":"HOST-4K7QZ9M","amount":100}'`
(`bill_ref` is the account number the customer typed). A charge's account
number and exact amount pay it; your app's prefix with anything else is
forwarded as `payment.received`; a prefix no app has lands in **Unmatched
payments**.

`TSP_MPESA_BASE_URL` (pointing at the fake) is accepted only in
development.

## Sandbox setup, step by step

Do this once per environment that should talk to the Daraja sandbox
(normally staging).

### 1. Create the Daraja app

1. Sign in at the Safaricom developer portal (`developer.safaricom.co.ke`)
   with the Talksasa developer account (ask the account owner; don't create
   a personal one for company use).
2. **My Apps → Add a new app.** Name it e.g. `talksasa-pay-staging`. Tick
   **M-Pesa Express (Lipa na M-Pesa Online)** and **C2B**.
3. Open the app. The **Consumer Key** and **Consumer Secret** are on the
   app's page (click to reveal). Leave the page open; you will type them
   straight into the SOPS editor in step 3.

### 2. Find the sandbox passkey and short code

- STK push in the sandbox uses Safaricom's test short code **`174379`**.
- The matching **passkey** is shown in the portal's M-Pesa Express
  simulator / test credentials page. Copy it from there into the SOPS
  editor; don't put it anywhere else, even though it is a test value.
- The test credentials page also lists the sandbox **C2B short code** and a
  test phone number. Note them for the smoke test.

### 3. Put the secrets into the SOPS bundle

<!-- settings: see settings.md -->

The mechanism is in [secrets.md](secrets.md): each environment has
`secrets/<env>.enc.yaml`, encrypted with the environment's age key, and the
deploy writes each key to `/run/talksasa-pay/secrets/<key>` (tmpfs), which
compose mounts as a Docker secret.

1. Generate the callback token on your machine without echoing it to a
   file:

   ```sh
   # prints a 48-character URL-safe token; paste it into the editor below, then clear the screen
   head -c 64 /dev/urandom | base64 | tr -d '\n=+/' | head -c 48; echo
   ```

2. Open the bundle in the SOPS editor (it decrypts in memory and re-encrypts
   on save):

   ```sh
   sops secrets/staging.enc.yaml
   ```

3. Add four keys and type or paste the values:

   ```yaml
   mpesa_consumer_key: <from the Daraja app page>
   mpesa_consumer_secret: <from the Daraja app page>
   mpesa_passkey: <sandbox passkey from the test credentials page>
   mpesa_callback_token: <the token from step 1>
   ```

4. Save, check the file is still encrypted (`grep -c ENC[ secrets/staging.enc.yaml`
   should be well above zero), commit it.

5. Nothing else to wire: `deploy/compose.prod.yml` already mounts the four
   keys into the api and worker as `/run/secrets/mpesa_*` and sets the
   `TSP_MPESA_*_FILE` variables. Once `TSP_MPESA_ENV` is set, the deploy
   (`deploy/host/apply.sh`) refuses to continue if any of the four is
   missing from the bundle or the token is malformed.

### 4. Set the non-secret settings

<!-- settings: see settings.md -->

In the environment's env file on the host (`/etc/talksasa-pay/<env>.env`;
`deploy/env.example` documents every variable):

```dotenv
TSP_MPESA_ENV=sandbox
TSP_MPESA_SHORTCODE=174379
# TSP_MPESA_CALLBACK_BASE_URL defaults to https://$TSP_API_DOMAIN/
TSP_MPESA_CALLBACK_CIDRS=            # empty is allowed in the sandbox; see "IP allow-list"
```

Deploy (`scripts/deploy.sh staging`). The api log says M-Pesa is on and in
which environment; it never prints the credentials.

### 5. Make the callbacks reachable

Safaricom must reach `TSP_MPESA_CALLBACK_BASE_URL` over HTTPS from the
internet.

- **Staging (preferred):** nothing to do. Caddy already serves `/cb/*` on
  the API domain with a real certificate.
- **A developer machine, temporarily:** run an HTTPS tunnel to the local
  API (port 8088), for example a throwaway Cloudflare quick tunnel or
  ngrok, and set `TSP_MPESA_CALLBACK_BASE_URL` to the tunnel's URL. Rules:
  - the full callback URL contains the callback token, so treat it as a
    secret: don't paste it in chat or tickets;
  - stop the tunnel as soon as the test is done, and generate a new
    callback token afterwards if the URL was shown anywhere;
  - never point a tunnel at production data.

### 6. Register the C2B URL

C2B confirmations only arrive after the URL is registered with Safaricom:

```sh
talksasa-pay admin mpesa register-urls --actor "Your Name"
# locally: make admin ARGS='mpesa register-urls --actor "Your Name"'
```

It registers `<callback base>/cb/mpesa/<token>/c2b/confirmation` for the
paybill, with no validation URL (we accept every payment; unknown account
numbers go to Unmatched payments) and is written to the audit log. Run it
again whenever the callback base URL or token changes. In the sandbox you
can re-register freely; in production see the go-live checklist.

### 7. Sandbox smoke test

On staging, with a `tsp_test_` key of a test app:

1. Create a KES 1 charge (`POST /v1/charges`). Open its `checkout_url`.
2. Enter the sandbox test phone number (or your own Safaricom line, if the
   sandbox prompts it) and press **Send payment prompt**. Approve it.
   The page should turn to **Paid** within seconds; the charge is
   `succeeded` and its transaction shows the receipt in the admin panel.
3. On a new charge, cancel the prompt. The page shows "The payment didn't
   go through" with **Try again**; the charge is still `pending`.
4. On a new charge, ignore the prompt. After about a minute the status
   check runs; the prompt ends failed (or needs review, see below).
5. Use the Daraja C2B simulator to pay the sandbox C2B short code with a
   pending charge's account number (e.g. `HOST-4K7QZ9M`) and exact amount.
   The charge succeeds. Pay again with a different amount: the app gets
   `payment.received`. Pay with a prefix no app has: it lands in
   **Unmatched payments**.
6. In the admin panel, **M-Pesa callbacks** lists every callback, all
   processed.

## Safaricom callback IP allow-list

`TSP_MPESA_CALLBACK_CIDRS` restricts which addresses may call `/cb/*`.
Anything else gets `404` and an alarm in the log.

- Get the **current** list of Safaricom's callback IP addresses from
  Safaricom (Daraja documentation or your Safaricom account manager) when
  you go live, and again whenever Safaricom announces a change. Don't copy
  a list from a blog post or an old config, and this runbook deliberately
  doesn't hard-code one: it changes.
- Write each as a CIDR (`/32` for a single address), comma separated.
- Production refuses to start without it. In the sandbox leave it empty
  unless you know the sandbox's addresses.
- Caddy passes the real client address; don't put another proxy in front
  without updating `TSP_TRUSTED_PROXIES`.

## Going live on the production paybill

The production paybill already exists. Before switching it on:

- [ ] A **production** Daraja app approved by Safaricom for the paybill
      (Go Live in the portal), with M-Pesa Express and C2B.
- [ ] Production consumer key/secret and the **production passkey** (sent by
      Safaricom for the short code) in `secrets/production.enc.yaml`, typed
      in with `sops`, never sent by email or chat. A fresh callback token.
- [ ] `TSP_MPESA_ENV=production`, `TSP_MPESA_PAYBILL` = the paybill,
      `TSP_MPESA_SHORTCODE` unset (defaults to the paybill) unless Safaricom
      gave a different STK short code, `TSP_MPESA_CALLBACK_BASE_URL` =
      `https://<production api domain>/`.
- [ ] `TSP_MPESA_CALLBACK_CIDRS` = Safaricom's current list (see above).
- [ ] **C2B registration.** Safaricom allows registering the confirmation URL
      for a production short code only once; changing it later goes
      through Safaricom support. Check the URL (base, token) twice, then run
      `talksasa-pay admin mpesa register-urls --actor "..."` once. If the
      paybill was registered to another system before, ask Safaricom to
      clear it first. Remember: one confirmation URL per paybill is exactly
      why Talksasa Pay exists, so no app may register its own.
- [ ] A real KES 10 prompt to a staff phone pays a test charge; a real KES
      10 paybill payment to another test charge's account number pays it;
      both show in the admin panel with receipts. Refund them through the
      usual finance process and record the reversals (below).
- [ ] Cancel and timeout tried once each on production: both end failed.
- [ ] Daily checks below assigned to a named person.

## Daily checks

Open the admin **Overview**. Under **Needs attention**:

| Item | What to do |
|---|---|
| Charges need review / payment prompts have no answer | Follow "Safaricom said paid without a receipt" below. Aim for same day: the customer is waiting and the app shows "confirming". |
| Unmatched payments | Paybill payments whose account-number prefix belongs to no app. Follow "Unmatched payments" below. |
| M-Pesa callbacks not processed | Open **M-Pesa callbacks** → "Not processed yet". A callback that is **Waiting** for minutes means the worker is down (`make worker-logs` / check the worker container). **Failing** shows the error; fix the cause and the worker retries it. Never edit or delete a callback. |
| Payments for inactive apps | The money was recorded and forwarded as usual (money rule 8). Tell the app's team: a suspended app's customers are still paying. |

Also glance at **Transactions → status Pending, channel M-Pesa prompt**:
anything pending for more than 30 minutes means the status checker is not
running.

## Safaricom said paid without a receipt (needs review)

When a prompt's callback never arrives and the status check says the payment
**succeeded**, Safaricom gives no receipt number. We never mark a charge
paid without a receipt, so the transaction and its charge go to **needs
review**, and the app gets `stk_push.needs_review` and
`charge.needs_review` (it shows the customer "We're confirming your
payment"). A charge in needs review never expires.

Staff with the Finance or Admin role resolve it:

1. Open the transaction (Overview → needs review, or the link in the
   app's support request). Note the amount, the masked phone and the
   time.
2. Find the payment on the **M-Pesa portal** (org portal → transaction
   search by time and amount; the M-Pesa request id on the transaction page
   helps).
3. **Found it:** in **Resolve this payment**, choose **Confirm with
   receipt**, type the receipt exactly as on the portal, give a reason
   ("Found on the M-Pesa portal, 25 Nov 10:42"), confirm. The transaction
   succeeds, the charge becomes `succeeded` and the app gets
   `charge.succeeded`.
4. **Not there** (check again after 30 minutes; M-Pesa can be slow):
   **Mark as not paid** with a reason. The prompt fails, the charge goes
   back to `pending`, and the customer can pay again.
5. If the receipt is **already recorded** elsewhere (`receipt taken`), don't
   force it: the money is already counted (often the customer also paid by
   paybill). Mark this one not paid and note the other receipt in the
   reason.

Every resolution is in the audit log with your name and reason. If a late
callback arrives after you resolved it, the system matches it by receipt
and does not count the money twice.

## Unmatched payments

A paybill payment whose account number starts with a prefix that belongs to
no app (a customer's typo, or an old number) is recorded with no app and
listed under **Unmatched payments**. It is not lost: the receipt is
recorded and the money is in the paybill.

Staff with the Finance or Admin role assign it:

1. Open **Unmatched payments** and the payment. Note the account number as
   typed, the amount, the masked phone and the time.
2. Work out whose it is: a near-miss of an app's prefix (`HOTS-1042` for
   `HOST-1042`), a customer who called that app's support with the
   receipt, or the app team recognising the amount and phone.
3. **Assign to app**, pick the app, give a reason ("Customer called HOST
   support with receipt SKP4ABCD12; typed HOTS"). The app gets
   `payment.received` and decides what the money pays.

From the CLI (same rules, same audit entry):

```sh
talksasa-pay admin txn assign --txn <transaction id> --app HOST --reason "..." --actor "Your Name"
```

Assignment is set once: an assigned payment can't be moved to another app.
Assigning to a suspended app is allowed (money rule 8). If nobody claims a
payment, refund the customer through the usual finance process.

## Reversing a payment

When Safaricom reverses a payment (or finance refunds it outside the
system), record it so the books and the app agree. **Admin role only.**

1. Open the transaction and choose **Reverse**. Give a reason and, for an
   M-Pesa payment, Safaricom's reversal receipt.
2. Talksasa Pay records a reversal transaction (`out`, the payment's full
   amount) pointing at the payment. If the payment paid a charge, the
   charge becomes `reversed`. The app gets `transaction.reversed`.

From the CLI:

```sh
talksasa-pay admin txn reverse --txn <transaction id> --receipt <reversal receipt> --reason "..." --actor "Your Name"
```

A payment can be reversed once. Nothing is edited or deleted, and a
reversed charge never reopens: the app decides what to do (for example
create a new charge).

## How unusual payments are handled

These rules were decided with the business owner (2026-11-24, revised
2026-11-25 for the gateway model). Nothing is ever counted twice; each
M-Pesa receipt is recorded once.

| Situation | What Talksasa Pay does |
|---|---|
| Safaricom confirms a prompt for a **different amount** than we asked | Records nothing, raises an alarm; staff cannot confirm it with a receipt (only mark it not paid). |
| Paybill notice for **another business number** | Not recorded; alarm. Check the paybill configuration. |
| Payment arrives **after we gave up** on a prompt | Recorded as a new transaction; it pays the prompt's charge (even if the charge expired or was cancelled meanwhile). The prompt shows as succeeded and points at the new transaction. |
| Paybill notice with an app's prefix that **matches one waiting charge** (account number and exact amount) | Pays that charge. If a prompt for it was waiting, the prompt is marked succeeded too; Safaricom's own prompt confirmation with the same receipt is then a duplicate. |
| Paybill notice with an app's prefix that **matches no waiting charge** (or more than one) | Recorded and forwarded to the app as `payment.received`. |
| The customer **really paid twice** (paybill + approved prompt, different receipts) | Both payments are recorded. The first pays the charge; the second is forwarded as `payment.received` with `duplicate_of_charge`. The app or finance refunds it. |
| Paybill payment with an **unknown prefix** | Recorded unmatched, for staff to assign (above). |
| Payment for an **inactive app** | Recorded and forwarded as normal; shown on the overview for staff. |

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `/v1` stk-push answers `503 mpesa_unavailable`; the checkout page shows no prompt form | `TSP_MPESA_ENV` empty (M-Pesa off). |
| `502 mpesa_error` on every prompt | Wrong consumer key/secret or passkey, wrong short code for the environment, or Daraja down. The api log has Daraja's error (never the credentials). |
| Prompts reach phones but results never arrive; everything ends "needs review" or failed after a minute | Callbacks can't reach us: wrong `TSP_MPESA_CALLBACK_BASE_URL`, tunnel closed, IP allow-list blocking Safaricom (look for the callback alarm log line), or the sandbox simply not calling back. |
| Callback log alarm "unknown token" | Someone is probing, or the token was rotated without re-registering C2B. |
| Paybill payments don't appear | C2B URL not registered (or registered to an old URL/token). Re-run `register-urls` (sandbox) or contact Safaricom (production). |
| Many paybill payments arrive as `payment.received` instead of paying charges | Customers pay a different amount than the charge, or the app reuses its own account numbers across several waiting charges with the same amount. Tell the app team. |

## Rotating credentials

- **Consumer secret / key:** regenerate in the Daraja portal, update the
  bundle with `sops`, deploy. Brief prompt failures during the deploy are
  possible; do it at a quiet time.
- **Passkey:** only Safaricom issues it; same process.
- **Callback token:** generate a new one, update the bundle, deploy, then
  re-register C2B (in production through Safaricom). STK callbacks use the
  URL sent with each prompt, so in-flight prompts that were sent with the
  old token can't call back: their status check resolves them.
