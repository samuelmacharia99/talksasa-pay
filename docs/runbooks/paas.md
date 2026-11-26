# Running on a platform (Procfile hosts, e.g. Talksasa Cloud)

Some hosts build the Go program themselves and start it from a `Procfile`.
They set plain environment variables only and route traffic to `$PORT`.

## Start command

The repository root `Procfile`:

```
release: go run -buildvcs=false ./cmd/talksasa-pay migrate up
web: sh scripts/paas-start.sh
```

- `release` applies migrations before each new version starts. With nothing
  to apply it logs `schema already up to date` and exits 0.
- `web` runs `scripts/paas-start.sh`, which builds the binary to `/tmp/tsp`
  and then `exec`s `/tmp/tsp serve`, so the platform's stop signal reaches
  the service directly. (The platform accepts one command per line.)
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
| `TSP_MPESA_CALLBACK_TOKEN` | Optional: the Settings page generates and stores one on the first save. When set (at least 32 characters of `A-Z a-z 0-9 _ -`) it wins. It is part of the secret callback URL Safaricom calls. |
| `TSP_MPESA_ENV` | `sandbox` or `production`. Empty = M-Pesa off unless saved on Settings. |
| `TSP_MPESA_PAYBILL` | The paybill number (5-7 digits). |
| `TSP_MPESA_CONSUMER_KEY`, `TSP_MPESA_CONSUMER_SECRET`, `TSP_MPESA_PASSKEY` | Daraja app credentials. |
| `TSP_MPESA_CALLBACK_BASE_URL` | Optional. Unset: the Settings page uses the address it is opened on plus `/api/`. Set (public `https://` base ending in `/`): saves always use it. |
| `TSP_MPESA_CALLBACK_CIDRS` | Safaricom's callback source networks, comma separated. Required for production M-Pesa. |

Optional: `TSP_DATABASE_URL` / `TSP_MIGRATE_DATABASE_URL` (override
`DATABASE_URL`), `TSP_LOG_LEVEL` (`info`), `TSP_DB_MAX_CONNS` (20; `serve`
opens two pools, so allow 2x in the database's connection limit).

## API + admin panel on one service (Talksasa Cloud split mode)

Talksasa Cloud can run this repository as a Go backend plus one Next.js
frontend behind one hostname. Redeploy dialog:

| Field | Value |
|---|---|
| Frontend | Next.js |
| API directory | `.` |
| Frontend directory | `web/apps/admin` |

How it fits together:

- Public traffic hits the platform's edge proxy. Paths under `/api/` (and
  `/health*`) go to the Go service, everything else to the admin panel.
  The Go service answers every route under `/api` as well as at the root,
  so apps call `https://<host>/api/v1/...`, Safaricom calls
  `https://<host>/api/cb/hooks/...`, and the platform's readiness probe
  `/api/health` is `/readyz`.
- The backend and frontend are separate containers. The admin panel
  reaches the Go admin listener at `http://backend:8089`, so the listener
  must bind all interfaces in its container (`TSP_ADMIN_ADDR`). The edge
  only forwards to port 8000, so 8089 is not public; it is reachable only
  from the service's own containers.
- The frontend is built with Node 22 from the pnpm workspace at the
  repository root (`pnpm install` there, then `pnpm --dir web/apps/admin
  run build`) and started with `next start`.
- The platform sets `PORT` itself (8000 for the backend, 3000 for the
  frontend); a `PORT` you set is overridden.
- Both containers get the same environment variables.
- The split mode may skip the Procfile `release:` line, so
  `scripts/paas-start.sh` runs `migrate up` before `serve` as well.

Environment variables, in addition to the required ones above:

| Variable | Value |
|---|---|
| `TSP_ADMIN_ADDR` | `0.0.0.0:8089` |
| `TSP_ADMIN_API_URL` | `http://backend:8089/admin/v1` |
| `TSP_ADMIN_SETUP_BASE_URL` | `https://<host>/setup/` |
| `TSP_MPESA_CALLBACK_BASE_URL` | Leave unset: the Settings page generates `https://<host>/api/` from the address you open it on. |
| `TSP_TRUSTED_PROXIES` | `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.1/32` (the service's private networks). Required for production M-Pesa: see "Client IP" below. |

Limits of this layout:

- The checkout page (`web/apps/pay`) is not deployed; each charge's
  `checkout_url` (`TSP_PAY_BASE_URL`) needs a second service for it.
- The Go service sees every staff request as coming from the frontend
  container, so the per-IP sign-in rate limit is shared by all staff, and
  audit entries record that address instead of the staff member's.

## Not covered by this single service

- Without the split mode above, the admin panel (`web/apps/admin`) and checkout page (`web/apps/pay`) are
  separate Next.js services. The admin panel talks to the Go admin listener
  (`127.0.0.1:8089`), so it must run next to the API or the listener must
  be reachable privately; never expose it publicly.
- Until then, staff use the CLI from a shell in the service:
  `/tmp/tsp admin user invite ...`, `/tmp/tsp admin app create ...`,
  `/tmp/tsp admin key issue ...`.
- Client IP: Talksasa Cloud's host nginx appends the caller's address to
  `X-Forwarded-For`, and the edge gateway appends its own Docker-network
  peer. From a peer in `TSP_TRUSTED_PROXIES` the service reads
  `X-Forwarded-For` right to left, skipping trusted (private) hops, so it
  gets the address nginx saw; anything a caller writes into the header is
  further left and never used, and a caller-set `X-Talksasa-Client-Ip` is
  ignored whenever `X-Forwarded-For` is present. Without
  `TSP_TRUSTED_PROXIES` every request looks like it comes from the edge:
  production M-Pesa callbacks fail the `TSP_MPESA_CALLBACK_CIDRS` check
  (log: `mpesa callback rejected`, `reason=ip_not_allowed`,
  `remote_ip=10.x`) and the charge goes to needs review.
