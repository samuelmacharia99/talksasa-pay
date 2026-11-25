# Runbook: staff access to the admin panel

The admin panel (`web/apps/admin`) is a Next.js server that talks to the Go
admin API on its own listener (`TSP_ADMIN_ADDR`, default `:8089`). That port
is **never** published to the internet: only the admin Next.js server can
reach it. Browsers talk to Next.js only; the staff session token lives in an
httpOnly, SameSite=Strict, `__Host-` cookie that page scripts cannot read.

Every staff member signs in with **email + password + authenticator app**
(TOTP). There is no way to turn 2FA off.

## Roles

| Role | Can |
|---|---|
| Viewer | See every screen. Change nothing. |
| Finance | Viewer access, plus resolving payments in needs review and assigning unmatched payments to an app. Prepares refunds once refunds launch. |
| Approver | Viewer access today. Approves refunds once refunds launch. |
| Admin | Everything above, plus apps, API keys, reversals and staff accounts. |

Role changes apply on the person's next click; they don't need to sign in
again. Nobody can change their own role, disable themselves, or remove the
last active admin.

## First admin (bootstrap)

There is no default account. On the server:

```sh
talksasa-pay admin user invite --email you@talksasa.co.ke --name "Your Name" --role admin --actor "Your Name"
# locally: make admin ARGS='user invite --email ... --name "..." --role admin --actor "..."'
```

It prints a one-time setup link, valid for 24 hours. Open it, choose a
password, scan the QR code with an authenticator app, enter the first code,
and **store the 10 recovery codes** in your password manager. No password
ever passes through the command line or the person who invites you.

## Adding staff

Admin panel, then Staff, then "Invite a staff member". Send the setup link
privately (direct message or in person, not a group chat or shared inbox).

## Lost phone / lost authenticator

1. The person signs in with one of their recovery codes (each works once).
2. If they have none left: an admin opens Staff and clicks "Reset sign-in" (or
   `talksasa-pay admin user reset --email ... --actor ...`). This wipes their
   password, authenticator and recovery codes, signs them out everywhere, and
   issues a new setup link.

If **every** admin is locked out, use the CLI reset on the server: it needs
shell access and the database credentials, and is audit-logged.

## Offboarding

Staff, then Disable (or `talksasa-pay admin user disable --email ... --actor ...`).
All their sessions end immediately and pending setup links stop working.
Accounts are never deleted, so the audit trail keeps its names.

## Sessions and lockout

- Sessions end after **15 minutes idle** or **8 hours** in total
  (`TSP_ADMIN_IDLE_TIMEOUT`, `TSP_ADMIN_ABSOLUTE_TIMEOUT`).
- Changing a password signs the person out everywhere.
- After 5 wrong passwords or codes the account locks for 1 minute, doubling
  per further failure up to 1 hour. The sign-in page shows the same message
  for every failure, so it cannot be used to find out which emails exist.
- Sign-in endpoints are also rate limited per client IP.

## Network settings

| Variable | Meaning |
|---|---|
| `TSP_ADMIN_ADDR` | Admin API listener. Bind it to the internal network only. |
| `TSP_TRUSTED_PROXIES` | CIDRs of the Next.js servers (and reverse proxy) allowed to pass the end user's IP. Anything else is ignored. |
| `TSP_ADMIN_SETUP_BASE_URL` | Public URL of the setup page, ending in `/setup/`. Must be https outside development. |
| `TSP_ADMIN_API_URL` (Next.js) | Internal URL of the admin API, e.g. `http://api:8089/admin/v1`. |
| `TSP_ADMIN_INSECURE_COOKIES` (Next.js) | `true` only for plain-http local development. |
| `TSP_TRUST_FORWARDED_FOR` (Next.js) | `true` when our own reverse proxy sets X-Forwarded-For. |

## What gets audit-logged

Invitations, activations, sign-ins (with IP and whether a recovery code was
used), sign-outs, password changes, resets, role changes, disables, and every
app and API key change, needs-review resolution, unmatched-payment assignment and reversal. See Audit log in the panel; the hash chain is
verified every time that page opens.
