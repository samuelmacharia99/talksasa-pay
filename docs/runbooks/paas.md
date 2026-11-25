# Running on a platform (Procfile hosts, e.g. Talksasa Cloud)

Some hosts build the Go program themselves and start it from a `Procfile`.
They set plain environment variables only and route traffic to `$PORT`.

## Start command

The repository root `Procfile`:

```
web: go build -buildvcs=false -trimpath -o /tmp/tsp ./cmd/talksasa-pay && /tmp/tsp migrate up && exec /tmp/tsp serve
```

- `migrate up` runs on every boot. With nothing to apply it logs
  `schema already up to date` and exits 0.
- `serve` runs the API **and** the worker in one process. The worker
  processes M-Pesa callbacks, status checks, webhooks and charge expiry;
  without it payments would be accepted but never processed.
- The API listens on all interfaces on `$PORT` (unless `TSP_HTTP_ADDR` is
  set). Health: `GET /healthz` (process up) and `GET /readyz` (database
  reachable).
- Go 1.26 or newer is needed to build.

## Environment variables

Required:

| Variable | What it is |
|---|---|
| `TSP_ENV` | `production` (or `staging`). |
| `TSP_SECRETS_FROM_ENV` | `true`. Staging and production otherwise accept secrets only as `*_FILE` paths; this host has no secret files. The service logs a warning at startup while it is on. |
| `DATABASE_URL` | Usually provided and rotated by the host (platform-managed). Used for both the API/worker and `migrate up` unless `TSP_DATABASE_URL` / `TSP_MIGRATE_DATABASE_URL` are set; those override it. The user must be able to create tables. |
| `TSP_DB_SSLMODE` | TLS to the database without editing the URL. Unset: production uses `require`. Set `disable` only when the database is on the same private network and does not offer TLS (e.g. a sidecar); the service then logs a warning at startup. A failed connection says when the server does not offer TLS. |
| `TSP_MASTER_KEYS` | Master keyring that encrypts stored secrets (app webhook secrets, M-Pesa credentials). One line `1:<base64 of 32 random bytes>`; generate with `talksasa-pay keygen --version 1`. **Back it up**: without it, stored secrets cannot be decrypted. Never change or drop a version that has been used. |
| `TSP_API_KEY_PEPPER` | Random string of at least 32 characters (e.g. `openssl rand -base64 48`) mixed into API key hashes. Changing it invalidates every issued API key. |
| `TSP_PAY_BASE_URL` | Public `https://` base of the checkout page, ending in `/` (each charge's `checkout_url` is this plus a token). |
| `TSP_ADMIN_SETUP_BASE_URL` | Public `https://` URL of the admin panel's account-setup page, ending in `/` (staff invitation links). |

M-Pesa (needed before taking payments; the credentials can instead be
saved on the admin Settings page):

| Variable | What it is |
|---|---|
| `TSP_MPESA_CALLBACK_TOKEN` | Random string, at least 32 characters of `A-Z a-z 0-9 _ -`. It is part of the secret callback URL Safaricom calls. |
| `TSP_MPESA_ENV` | `sandbox` or `production`. Empty = M-Pesa off unless saved on Settings. |
| `TSP_MPESA_PAYBILL` | The paybill number (5-7 digits). |
| `TSP_MPESA_CONSUMER_KEY`, `TSP_MPESA_CONSUMER_SECRET`, `TSP_MPESA_PASSKEY` | Daraja app credentials. |
| `TSP_MPESA_CALLBACK_BASE_URL` | Public `https://` base Safaricom calls back, ending in `/` (this service's public URL). |
| `TSP_MPESA_CALLBACK_CIDRS` | Safaricom's callback source networks, comma separated. Required for production M-Pesa. |

Optional: `TSP_DATABASE_URL` / `TSP_MIGRATE_DATABASE_URL` (override
`DATABASE_URL`), `TSP_LOG_LEVEL` (`info`), `TSP_DB_MAX_CONNS` (20; `serve`
opens two pools, so allow 2x in the database's connection limit).

## Not covered by this single service

- The admin panel (`web/apps/admin`) and checkout page (`web/apps/pay`) are
  separate Next.js services. The admin panel talks to the Go admin listener
  (`127.0.0.1:8089`), so it must run next to the API or the listener must
  be reachable privately; never expose it publicly.
- Until then, staff use the CLI from a shell in the service:
  `/tmp/tsp admin user invite ...`, `/tmp/tsp admin app create ...`,
  `/tmp/tsp admin key issue ...`.
- Client IP: the service trusts the end user's IP only from
  `X-Talksasa-Client-Ip` set by a proxy listed in `TSP_TRUSTED_PROXIES`.
  Behind a platform proxy that does not set it, per-IP rate limits apply to
  the proxy's address and production M-Pesa callbacks fail the
  `TSP_MPESA_CALLBACK_CIDRS` check. Sort this out before production M-Pesa.
