# Talksasa Pay

An M-Pesa payment gateway on **one Talksasa paybill**, for Talksasa's apps
(ISP Billing `BILL-`, Cloud Hosting `HOST-`, future ones). It works like
Paystack or Flutterwave: an app creates a **charge** ("collect KES 1,500 for
my reference INV-42"), Talksasa Pay sends the M-Pesa prompt or shows a
checkout page with the paybill steps, confirms the payment with Safaricom,
keeps an append-only ledger and tells the app the result, with the M-Pesa
receipt, by signed webhook and in a gap-free events feed. The gateway knows
nothing about the apps' customers or invoices
([ADR 0002](docs/adr/0002-generic-gateway.md), which supersedes ADR 0001).

## Status
| Phase | Scope | State |
|---|---|---|
| 0 | Foundations: config, logging, DB, HTTP, metrics, CI | done |
| 1 | Core model, rebuilt as a gateway (2026-11-25): apps, keys, charges, ledger, audit | in progress |
| 2 | Daraja STK + C2B collections: prompts from `/v1` and the checkout page, callbacks inbox, status checks, needs-review resolution, paybill routing by account prefix, unmatched payments, [runbook](docs/runbooks/mpesa.md) | in progress (sandbox run pending) |
| 3 | App API `/v1` (charges, events feed), signed webhooks via River worker, Go + PHP/Laravel SDKs, docs in [docs/api](docs/api/README.md) | in progress (charges rewrite) |
| 4 | B2C refunds (maker-checker) | |
| UI | Admin panel + hosted checkout page (Next.js), staff sign-in with 2FA | in progress (charges rewrite) |
| 5 | Reconciliation (statement import), reports | |
| 6 | Card gateway | |
| Deploy | `scripts/deploy.sh`: tested, health-checked rollout with backup and auto-rollback; Caddy TLS edge | done |
| 7–8 | Security review, go-live, manual test plan | |

## Quick start (development)
Requires Docker and Node 22 or newer (for pnpm via corepack; the pnpm workspace root is the repository root, the web code is under `web/`). Go runs in a pinned
container via `scripts/go.sh`.
```sh
make up                          # postgres, migrations, api :8088, admin :3001, checkout pages :3000
make admin ARGS='seed demo --actor "Your Name"'      # demo apps, charges and payments
make admin ARGS='user invite --email you@talksasa.co.ke --name "Your Name" --role admin --actor "Your Name"'
#   open the printed setup link, set a password, scan the QR code, save the recovery codes,
#   then sign in at http://127.0.0.1:3001
#   checkout pages: http://127.0.0.1:3000/<token printed by seed demo>
#   M-Pesa runs against a fake Daraja (127.0.0.1:8090): on a checkout page, a phone ending in 0
#   pays, 1 cancels, 2 ends in needs review (docs/runbooks/mpesa.md lists them all).
#   Paybill payment: curl -X POST 127.0.0.1:8090/fake/c2b -d '{"bill_ref":"HOST-4K7QZ9M","amount":100}'

make verify                      # everything below; run before every merge
make test                        # Go unit + database integration tests (-race)
make lint                        # golangci-lint (incl. gosec) + sqlc drift
make web-check                   # TS types match OpenAPI, typecheck, eslint, vitest
make smoke                       # security, auth and integrity checks against the running Docker stack
make e2e                         # Playwright browser tests (sign-in with real TOTP, every screen)
make e2e-mpesa                   # charge -> prompt/paybill -> callback -> signed webhook verified by the PHP SDK
make worker-logs                 # follow the background worker (webhooks, charge expiry)
```
Deploying: `scripts/deploy.sh staging|production --host user@host` (see
[docs/runbooks/deploy.md](docs/runbooks/deploy.md)).

App API keys: `make admin ARGS='key issue --app HOST --label staging --actor "Your Name"'`,
then `curl -H "Authorization: Bearer tsp_test_..." localhost:8088/v1/whoami`.
M-Pesa (Daraja sandbox setup, going live, daily checks, needs review,
unmatched payments, reversals): [docs/runbooks/mpesa.md](docs/runbooks/mpesa.md);
for app developers: [docs/api](docs/api/README.md).
Platform hosts (Procfile, env vars only): [docs/runbooks/paas.md](docs/runbooks/paas.md).
Load test: `go run ./cmd/loadtest -h` (local stack only; it creates charges
and fake M-Pesa payments, then checks the money invariants).
`make help` lists every target. Metrics are on `127.0.0.1:9464/metrics`
(never on the public port). The admin API (`:8089`) is never published; see
[docs/runbooks/admin-access.md](docs/runbooks/admin-access.md).

## Layout
```
cmd/talksasa-pay      single binary: api | worker | migrate | admin | keygen | healthcheck
cmd/fakedaraja        in-memory Safaricom Daraja for development
cmd/loadtest          /v1 load generator + money invariant checks
db/migrations         SQL schema (embedded into the binary), explained in docs/schema.md
db/queries            SQL for sqlc  ->  internal/store (generated, do not edit)
internal/app          apps, API keys, authentication
internal/accountref   account numbers: prefix routing, generation with a check character
internal/charge       charges: create, prompt, cancel, expire
internal/ledger       record payments, succeed charges, reversals
internal/stk          M-Pesa prompts, callbacks, status checks, paybill routing
internal/events       transactional outbox + gap-free feed
internal/webhook      signed webhook delivery (SSRF-safe client)
internal/appapi       /v1 handlers
internal/httpapi      public listener: /v1, checkout page API, M-Pesa callbacks
internal/audit        hash-chained audit log
internal/platform     config-independent plumbing: db, crypto, log, httpx, metrics
internal/admin        staff accounts: argon2id, TOTP, sessions, roles
internal/adminapi     /admin/v1 (internal listener only)
internal/reporting    read-only views for the panel and the checkout page
internal/mpesa        Daraja client, callback parsing; mpesatest = fake Daraja
internal/integrationtest  tests against real Postgres with production privileges
api/*.openapi.yaml    API contracts; TypeScript types are generated from them
clients/php, pkg/talksasapay  PHP/Laravel and Go SDKs
web/apps/admin        staff panel (Next.js; talks to Go server-side only)
web/apps/pay          hosted checkout page (Next.js)
web/packages          ui (design tokens, components), money, api-client
web/e2e               Playwright tests
docs/                 schema explained, ADRs, API guides, runbooks
```

## Rules of the codebase
- Money is `money.KES` (int64 whole shillings). No floats, ever.
- App-scoped code takes the app id from the authenticated key only.
- Never log, return or store secrets in plain text. Wrap them in `config.Secret`.
- Ledger rows are never updated (except forward status moves and set-once
  fields) or deleted: corrections are new rows. The database enforces this too.
- A charge is paid only by a succeeded, receipted transaction for exactly its
  amount; the database refuses anything else.
- Every write runs in `db.WithTx` with a named actor. Sensitive actions call
  `audit.Record` in the same transaction.
- The frontends hold no secrets and do no money arithmetic: amounts and
  statuses always come from the Go service. Staff session tokens stay in
  httpOnly cookies and are sent to Go only from the Next.js server.
