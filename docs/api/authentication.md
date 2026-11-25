# Authentication

Every request carries an app API key as a bearer token:

```
Authorization: Bearer tsp_live_<key id>_<secret>
```

The key decides which app you are. There is no way to name another app in
a request, so a key can only reach its own app's charges, prompts,
transactions and events. Another app's ids simply answer `404`.

## Key format

| Part | Example | Notes |
|---|---|---|
| Prefix | `tsp_live_` or `tsp_test_` | `tsp_live_` works only in production; `tsp_test_` only outside it. A key used in the wrong environment is rejected with `401`. |
| Key id | 20 lowercase letters and digits 2-7 | Not secret. Shown to Talksasa staff and in `GET /whoami` as `key_id`, so you can tell keys apart. |
| Secret | 52 lowercase letters and digits 2-7 | Secret. Talksasa stores only a fingerprint and can never show it again. |

Treat the whole string as one opaque value. Don't parse it, trim it or
change its case.

## Getting a key

Talksasa staff issue keys from the admin panel and give each one a label
(for example `production, March 2027`) and, optionally, an expiry date. The
key is shown once, at the moment it is issued. If you lose it, ask for a new
one and have the old one revoked.

## Storing keys

- Keep the key in your secret store or environment (`.env` that is not
  committed, Laravel's encrypted environment, your host's secret manager).
- Never put it in source control, front-end code, mobile apps, logs or error
  reports. Calls to Talksasa Pay are made from your server only.
- Use a different key for each environment (staging, production) and, if you
  run several independent services, one per service. Then you can revoke one
  without touching the others.

## Rotating a key without downtime

An app can have several active keys at once, so rotation never needs an
outage:

1. Ask Talksasa staff to issue a second key.
2. Deploy your app with the new key.
3. Check that the new key is in use: `GET /whoami` returns the new `key_id`,
   and staff can see each key's "last used" time.
4. Ask staff to revoke the old key. Revocation takes effect on its next
   request.

Rotate at least once a year, whenever someone who had access leaves, and
immediately if a key may have leaked.

## IP allow-lists

Talksasa staff can restrict a key to the addresses your servers call from:
single addresses (`203.0.113.4`) or CIDR ranges (`203.0.113.0/24`,
`2001:db8::/32`), up to 50 entries. A key with no list works from any
address. Changes apply from the key's next request.

Use an allow-list for production keys whenever your servers have stable
outbound addresses. Before you move servers or change your NAT gateway, send
the new addresses to Talksasa staff so they can add them first, then remove
the old ones after the move.

## 401 or 403?

| Status | `code` | Meaning | What to do |
|---|---|---|---|
| `401` | `unauthorized` | The key is missing, malformed, unknown, revoked, expired, for the other environment, or belongs to an app Talksasa has suspended. The response doesn't say which, on purpose. | Check the `Authorization` header and the environment. If the key is right, ask staff whether it was revoked or expired, or the app suspended. Don't retry automatically. |
| `403` | `ip_not_allowed` | The key is valid but this request came from an address that is not on its allow-list. | Check the outbound address your server really uses (proxies and NAT gateways change it). Ask staff to add it. |
| `403` | `app_suspended` | Talksasa suspended your app while this request was in flight (rare). | Contact Talksasa. |

A suspended app's keys get `401 unauthorized`, the same answer as a
revoked key: the key check turns away keys of any app that is not active,
before the request reaches your data. You only see `403 app_suspended` if
the suspension lands between that check and the write, for example during a
`POST /charges`. Either way, contact Talksasa. Money your customers send
while the app is suspended is still recorded, still succeeds your charges
and is still delivered to your webhook and events feed (see
[charges](charges.md#8-suspended-apps-still-receive-their-money)).
