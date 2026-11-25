# Errors

Every error is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem
response with `Content-Type: application/problem+json`:

```json
{
  "type": "https://pay.talksasa.com/problems/validation_failed",
  "title": "Unprocessable Entity",
  "status": 422,
  "code": "validation_failed",
  "detail": "The request is not valid; see errors.",
  "request_id": "01JD...",
  "errors": [
    { "field": "amount", "message": "must be a whole number of KES from 1 to 250000" }
  ]
}
```

- Branch on `code`. It is stable; `title` and `detail` are for people and may
  be reworded.
- `errors` appears on validation problems and names each bad field.
- `request_id` (also in the `X-Request-Id` header) identifies the request in
  Talksasa's logs. Log it, and quote it when you ask for help.
- `429` and `503` responses carry `Retry-After` in seconds, except `503 mpesa_unavailable`, which is not worth retrying.

## Codes

| `code` | Status | Cause | What to do |
|---|---|---|---|
| `unauthorized` | 401 | No key, a malformed key, or a key that is unknown, revoked, expired, for the other environment (`tsp_test_` vs `tsp_live_`), or for an app Talksasa has suspended. | Check the `Authorization: Bearer ...` header and which key the environment loads. Ask Talksasa staff if the key was revoked. Don't retry. |
| `ip_not_allowed` | 403 | The key has an IP allow-list and this request came from another address. | Find your server's real outbound address and ask staff to add it. See [authentication](authentication.md). |
| `app_suspended` | 403 | Talksasa suspended your app while this request was in flight. Once an app is suspended, its keys get `401 unauthorized` instead (see [authentication](authentication.md#401-or-403)). | Contact Talksasa. Don't retry. |
| `rate_limited` | 429 | Too many requests with this key in a short time. | Wait `Retry-After` seconds, then retry. Spread batch jobs out. |
| `overloaded` | 503 | Talksasa Pay is at capacity and turned the request away. Nothing was changed. | Wait `Retry-After` seconds and retry the same call (same `Idempotency-Key`). |
| `busy` | 503 | The records this request needs were locked by another request for too long. Nothing was changed. | Wait `Retry-After` seconds and retry the same call (same `Idempotency-Key`). |
| `timeout` | 503 | The request took too long; it may or may not have been done. | Retry with the **same** `Idempotency-Key` (or re-read). Never with a new key. |
| `not_found` | 404 | No such resource **for your app**: a wrong id, an unknown path, or something that belongs to another app. | Check the id. |
| `method_not_allowed` | 405 | The path exists but not with this HTTP method. | Check the method against the [contract](../../api/app.openapi.yaml). |
| `validation_failed` | 422 for the JSON body; 400 for query parameters and the `Idempotency-Key` header | One or more fields are invalid. `errors` lists them, for example an amount out of range, a bad phone number, an account number without your prefix, metadata over 4 KB, an `expires_in` out of range, or an unknown field. | Fix the fields named in `errors`. Don't retry unchanged. |
| `invalid_json` | 400 | The body is not a single valid JSON object. | Send one JSON object with the documented fields. |
| `unsupported_media_type` | 415 | A request with a body did not say `Content-Type: application/json`. | Set the header. |
| `payload_too_large` | 413 | The body is over 64 KB. | Send less: no request in this API needs that much. |
| `idempotency_key_required` | 400 | `POST /charges` or `POST /charges/{id}/stk-push` without an `Idempotency-Key` header. | Add one; see [idempotency](idempotency-and-retries.md). |
| `idempotency_conflict` | 409 | This `Idempotency-Key` was used before with a different body. | A bug: one key was used for two different charges or prompts. Fix how you build keys. |
| `duplicate_reference` | 409 | You already have a charge with this `reference`, in any status. References are unique for ever. | Fetch it with `GET /charges?reference=...` and use it. To charge again, use a new reference. |
| `not_cancellable` | 409 | The charge is not `pending` (it succeeded, is in review, or expired). | Don't retry. Re-read the charge and show its status. |
| `not_payable` | 409 | `stk-push` on a charge that is not `pending` (succeeded, in review, cancelled or expired). | Re-read the charge and show its status. |
| `amount_too_large` | 409 | The amount is above what an M-Pesa prompt allows (KES 250,000). Charges can't exceed it, so you should not see this from `/v1`. | Show the paybill instructions instead. |
| `stk_in_progress` | 409 | `stk-push` while a prompt for this charge is still on the customer's phone. `detail` has the live prompt's id. | Tell the customer to check their phone. Don't send another. |
| `mpesa_error` | 502 | Safaricom refused or failed to send the prompt. Nothing was charged. `detail` has the failed prompt's id. | Don't retry automatically. Offer the paybill, or let the customer try again later with a new `Idempotency-Key`. |
| `mpesa_unavailable` | 503 | M-Pesa prompts are not enabled in this environment. | Show the paybill instructions. Not worth retrying. |
| `not_implemented` | 501 | The endpoint exists in the contract but is not available in this environment yet. | Don't retry. Ask Talksasa staff. |
| `internal_error` | 500 | Something went wrong on Talksasa's side. Nothing secret is in the response. | Retry with backoff (writes are idempotent). If it persists, send the `request_id` to Talksasa. |

New codes may be added. Treat an unknown `4xx` code as "don't retry, log
and alert", and an unknown `5xx` as retryable, except `mpesa_error` and
`mpesa_unavailable`, which are final answers. The prompt's own failures
(cancelled, timed out...) arrive as `stk_push.failed` events, not errors;
see [M-Pesa payment prompts](stk-push.md).
