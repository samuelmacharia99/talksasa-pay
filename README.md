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
| 2 | Daraja STK + C2B collections, River worker | next |
| 3 | Product API `/v1`, webhooks, clients, launch-product integration | |
| 4 | B2C refunds (maker-checker) | |
| 5 | Reconciliation, admin panel (Next.js), reports | |
| 6 | Card gateway | |
| 7–8 | Security review, go-live, manual test plan | |

## Quick start (development)
Requires Docker only. Go runs in a pinned container via `scripts/go.sh`.
```sh
make dev-secrets                 # throwaway local secrets (gitignored)
docker compose up -d postgres
make test                        # unit + database integration tests (-race)
make smoke                       # end-to-end checks against the running Docker stack
make verify                      # test + lint + vuln + smoke: run before every merge
make up                          # postgres + migrate + api on 127.0.0.1:8088
make admin ARGS='product create --name "Cloud Hosting" --prefix HOST --actor "Your Name"'
make admin ARGS='key issue --product HOST --label staging --actor "Your Name"'
curl -H "Authorization: Bearer tsp_test_..." localhost:8088/v1/whoami
```
`make help` lists every target. Metrics are on `127.0.0.1:9464/metrics`
(never on the public port).

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
internal/integrationtest  tests against real Postgres with production privileges
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
