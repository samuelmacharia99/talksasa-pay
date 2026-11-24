# Runbook: M-Pesa (Safaricom Daraja)

Talksasa Pay talks to Safaricom's Daraja API for three things:

- **STK push** (the "M-Pesa prompt"): from the product API
  (`POST /v1/payment-requests/{id}/stk-push`) and from the pay page's
  **Send payment prompt** button.
- **STK query**: if a prompt's result doesn't arrive within about a minute,
  the worker asks Safaricom.
- **C2B confirmations**: every payment made to the paybill from the M-Pesa
  menu.

Safaricom calls us back at `/cb/mpesa/<callback token>/stk` and
`/cb/mpesa/<callback token>/c2b/confirmation` on the API domain (Caddy
already routes `/cb/*`). Every callback is stored first and then applied by
the worker, so nothing Safaricom sends is lost even if applying it fails.

**Money rule:** a bill is paid only when Safaricom confirms with an M-Pesa
receipt. A prompt Safaricom calls "successful" without a receipt goes to
**needs review** and waits for a person (see below).

**Secrets rule:** consumer key, consumer secret, passkey and the callback
token are secrets. They live only in the SOPS bundle. Never paste them into
chat, tickets, email, shell history or logs. If one leaks, rotate it
(bottom of this page).

## Settings

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
`curl -X POST 127.0.0.1:8090/fake/c2b -d '{"bill_ref":"HOST-1042","amount":100}'`.

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

On staging, with a `tsp_test_` key:

1. Create a payer and a KES 1 payment request. Open its pay page.
2. Enter the sandbox test phone number (or your own Safaricom line, if the
   sandbox prompts it) and press **Send payment prompt**. Approve it.
   The page should turn to **Paid** within seconds; the admin transaction
   shows the receipt.
3. On a new request, cancel the prompt. The page shows "The payment didn't
   go through" with **Try again**.
4. On a new request, ignore the prompt. After about a minute the status
   check runs; the prompt ends failed (or needs review, see below).
5. Use the Daraja C2B simulator to pay the sandbox C2B short code with the
   payer's account number (e.g. `HOST-1042`). It appears in Transactions
   and pays the bill; an unknown account number lands in Unmatched payments.
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
      why Talksasa Pay exists, so no product may register its own.
- [ ] A real KES 10 prompt to a staff phone pays a test bill; a real KES 10
      paybill payment to a test payer's account number is applied; both
      show in the admin panel with receipts. Reverse them through the usual
      finance process.
- [ ] Cancel and timeout tried once each on production: both end failed.
- [ ] Daily checks below assigned to a named person.

## Daily checks

Open the admin **Overview**. Under **Needs attention**:

| Item | What to do |
|---|---|
| Transactions need review / payment prompts have no answer | Follow "Safaricom said paid without a receipt" below. Aim for same day: the customer is waiting and the product shows "confirming". |
| M-Pesa callbacks not processed | Open **M-Pesa callbacks** → "Not processed yet". A callback that is **Waiting** for minutes means the worker is down (`make worker-logs` / check the worker container). **Failing** shows the error; fix the cause and the worker retries it. Never edit or delete a callback. |
| Payments for inactive products or payers | The money was applied as usual. Tell the product team: a suspended product's customer, or a closed payer, is still paying. |
| Unmatched payments | Paybill payments with an unknown account number. Assign them (Phase 5) or contact the payer. |

Also glance at **Transactions → status Pending, channel M-Pesa prompt**:
anything pending for more than 30 minutes means the status checker is not
running.

## Safaricom said paid without a receipt (needs review)

When a prompt's callback never arrives and the status check says the payment
**succeeded**, Safaricom gives no receipt number. We never mark a bill paid
without a receipt, so the transaction goes to **needs review** and the
product gets `stk_push.needs_review` (it shows the customer "We're
confirming your payment").

Staff with the Finance or Admin role resolve it:

1. Open the transaction (Overview → needs review, or the link in the
   product's support request). Note the amount, the masked phone and the
   time.
2. Find the payment on the **M-Pesa portal** (org portal → transaction
   search by time and amount; the M-Pesa request id on the transaction page
   helps).
3. **Found it:** in **Resolve this payment**, choose **Confirm with
   receipt**, type the receipt exactly as on the portal, give a reason
   ("Found on the M-Pesa portal, 24 Nov 10:42"), confirm. The transaction
   succeeds, the money is applied to the bill and the product gets
   `payment_request.paid`.
4. **Not there** (check again after 30 minutes; M-Pesa can be slow):
   **Mark as not paid** with a reason. The prompt fails and the customer can
   pay again.
5. If the receipt is **already recorded** elsewhere (`receipt taken`), don't
   force it: the money is already counted (often the customer also paid by
   paybill). Mark this one not paid and note the other receipt in the
   reason.

Every resolution is in the audit log with your name and reason. If a late
callback arrives after you resolved it, the system matches it by receipt
and does not count the money twice.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `/v1` stk-push answers `503 mpesa_unavailable`; pay page shows no prompt form | `TSP_MPESA_ENV` empty, or the bill is above KES 250,000 (pay page only). |
| `502 mpesa_error` on every prompt | Wrong consumer key/secret or passkey, wrong short code for the environment, or Daraja down. The api log has Daraja's error (never the credentials). |
| Prompts reach phones but results never arrive; everything ends "needs review" or failed after a minute | Callbacks can't reach us: wrong `TSP_MPESA_CALLBACK_BASE_URL`, tunnel closed, IP allow-list blocking Safaricom (look for the callback alarm log line), or the sandbox simply not calling back. |
| Callback log alarm "unknown token" | Someone is probing, or the token was rotated without re-registering C2B. |
| Paybill payments don't appear | C2B URL not registered (or registered to an old URL/token). Re-run `register-urls` (sandbox) or contact Safaricom (production). |

## Rotating credentials

- **Consumer secret / key:** regenerate in the Daraja portal, update the
  bundle with `sops`, deploy. Brief prompt failures during the deploy are
  possible; do it at a quiet time.
- **Passkey:** only Safaricom issues it; same process.
- **Callback token:** generate a new one, update the bundle, deploy, then
  re-register C2B (in production through Safaricom). STK callbacks use the
  URL sent with each prompt, so in-flight prompts that were sent with the
  old token can't call back: their status check resolves them.
