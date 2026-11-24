# Talksasa Pay

Internal payment collection layer for Talksasa Home of Creatives Ltd. Every
Talksasa product (ISP Billing `BILL-`, Cloud Hosting `HOST-`, future ones)
collects through **one Talksasa M-Pesa paybill**. Talksasa Pay works out which
product and customer each payment belongs to, keeps an append-only ledger and
notifies the product. It only ever collects Talksasa's own revenue
([ADR 0001](docs/adr/0001-revenue-only.md)).

## Status
| Phase | Scope | State |
|---|---|---|
| 0 | Foundations: config, logging, DB, HTTP, metrics, CI | done |
| 1 | Core model: products, keys, payers, requests, ledger, allocation | done |
| 2 | Daraja STK + C2B collections: M-Pesa prompts from `/v1` and the pay page, callbacks inbox, status checks, needs-review resolution, SDKs, [runbook](docs/runbooks/mpesa.md) | in progress (sandbox run pending) |
| 3 | Product API `/v1` (+ events feed), signed webhooks via River worker, Go + PHP/Laravel SDKs, docs in [docs/api](docs/api/README.md) | done (product integration pending codebase access) |
| 4 | B2C refunds (maker-checker) | |
| UI | Admin panel + hosted pay page (Next.js), staff sign-in with 2FA, read-only views | done (pulled forward) |
| 5 | Reconciliation (STK sweeper, suspense assignment, statement import), reports | |
| 6 | Card gateway | |
| Deploy | `scripts/deploy.sh`: tested, health-checked rollout with backup and auto-rollback; Caddy TLS edge | done |
| 7–8 | Security review, go-live, manual test plan | |

## Quick start (development)
Requires Docker and Node 24 (for pnpm via corepack). Go runs in a pinned
container via `scripts/go.sh`.
```sh
make up                          # postgres, migrations, api :8088, admin :3001, pay page :3000
make admin ARGS='seed demo --actor "Your Name"'      # demo products, payers, bills, payments
make admin ARGS='user invite --email you@talksasa.co.ke --name "Your Name" --role admin --actor "Your Name"'
#   open the printed setup link, set a password, scan the QR code, save the recovery codes,
#   then sign in at http://127.0.0.1:3001
#   pay pages: http://127.0.0.1:3000/<token printed by seed demo>
#   M-Pesa runs against a fake Daraja (127.0.0.1:8090): on a pay page, a phone ending in 0
#   pays, 1 cancels, 2 ends in needs review (docs/runbooks/mpesa.md lists them all).
#   Paybill payment: curl -X POST 127.0.0.1:8090/fake/c2b -d '{"bill_ref":"HOST-1042","amount":100}'

make verify                      # everything below; run before every merge
make test                        # Go unit + database integration tests (-race)
make lint                        # golangci-lint (incl. gosec) + sqlc drift
make web-check                   # TS types match OpenAPI, typecheck, eslint, vitest
make smoke                       # 49 checks against the running Docker stack
make e2e                         # Playwright browser tests (sign-in with real TOTP, every screen)
make worker-logs                 # follow the background worker (webhooks, expiry)
```
Deploying: `scripts/deploy.sh staging|production --host user@host` (see
[docs/runbooks/deploy.md](docs/runbooks/deploy.md)).

Product API keys: `make admin ARGS='key issue --product HOST --label staging --actor "Your Name"'`,
then `curl -H "Authorization: Bearer tsp_test_..." localhost:8088/v1/whoami`.
M-Pesa (Daraja sandbox setup, going live, daily checks, resolving payments
Safaricom confirmed without a receipt): [docs/runbooks/mpesa.md](docs/runbooks/mpesa.md);
for product developers: [docs/api/stk-push.md](docs/api/stk-push.md).
`make help` lists every target. Metrics are on `127.0.0.1:9464/metrics`
(never on the public port). The admin API (`:8089`) is never published; see
[docs/runbooks/admin-access.md](docs/runbooks/admin-access.md).

## Layout
```
cmd/talksasa-pay      single binary: api | migrate | admin | keygen | healthcheck
db/migrations         SQL schema (embedded into the binary)
db/queries            SQL for sqlc  ->  internal/store (generated, do not edit)
internal/allocation   pure allocation engine + ledger writer
internal/ledger       record payments, reversals
internal/payer        payer accounts, account numbers (Damm check digit), routing
internal/paymentrequest
internal/product      products, API keys, authentication
internal/audit        hash-chained audit log
internal/platform     config-independent plumbing: db, crypto, log, httpx, metrics
internal/admin        staff accounts: argon2id, TOTP, sessions, roles
internal/adminapi     /admin/v1 (internal listener only)
internal/reporting    read-only views for the panel and the pay page
internal/mpesa       Daraja client, callback parsing; mpesatest = fake Daraja
internal/integrationtest  tests against real Postgres with production privileges
api/*.openapi.yaml    API contracts; TypeScript types are generated from them
web/apps/admin        staff panel (Next.js; talks to Go server-side only)
web/apps/pay          hosted pay page (Next.js)
web/packages          ui (design tokens, components), money, api-client
web/e2e               Playwright tests
docs/                 schema explained, ADRs, runbooks
```

## Rules of the codebase
- Money is `money.KES` (int64 whole shillings). No floats, ever.
- Product-scoped code takes the product id from the authenticated key only.
- Never log, return or store secrets in plain text. Wrap them in `config.Secret`.
- Ledger rows are never updated (except forward status moves) or deleted:
  corrections are new rows. The database enforces this too.
- Every write runs in `db.WithTx` with a named actor. Sensitive actions call
  `audit.Record` in the same transaction.
- The frontends hold no secrets and do no money arithmetic: amounts and
  statuses always come from the Go service. Staff session tokens stay in
  httpOnly cookies and are sent to Go only from the Next.js server.
