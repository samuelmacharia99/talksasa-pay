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
    { "field": "amount", "message": "must be a whole number of KES from 1 to 1000000000" }
  ]
}
```

- Branch on `code`. It is stable; `title` and `detail` are for people and may
  be reworded.
- `errors` appears on validation problems and names each bad field.
- `request_id` (also in the `X-Request-Id` header) identifies the request in
  Talksasa's logs. Log it, and quote it when you ask for help.
- `429` and `503` responses carry `Retry-After` in seconds.

## Codes

| `code` | Status | Cause | What to do |
|---|---|---|---|
| `unauthorized` | 401 | No key, a malformed key, or a key that is unknown, revoked, expired or for the other environment (`tsp_test_` vs `tsp_live_`). | Check the `Authorization: Bearer ...` header and which key the environment loads. Ask Talksasa staff if the key was revoked. Don't retry. |
| `ip_not_allowed` | 403 | The key has an IP allow-list and this request came from another address. | Find your server's real outbound address and ask staff to add it. See [authentication](authentication.md). |
| `product_suspended` | 403 | Talksasa has suspended your product. | Contact Talksasa. Don't retry. |
| `rate_limited` | 429 | Too many requests with this key in a short time. | Wait `Retry-After` seconds, then retry. Spread batch jobs out. |
| `overloaded` | 503 | The service is busy or the request took too long. | Wait `Retry-After` seconds and retry the same call (same `Idempotency-Key`). |
| `not_found` | 404 | No such resource **for your product**: a wrong id, a payer you never created, an unknown path, or something that belongs to another product. | Check the id or `external_ref`. Create the payer with `PUT /payers/{external_ref}` first. |
| `method_not_allowed` | 405 | The path exists but not with this HTTP method. | Check the method against the [contract](../../api/product.openapi.yaml). |
| `validation_failed` | 422 (body) or 400 (query, headers) | One or more fields are invalid. `errors` lists them, for example an amount out of range, an unknown payer in `payer`, a bad phone number, a description over 500 characters, an `expires_at` in the past, or an unknown field. | Fix the fields named in `errors`. Don't retry unchanged. |
| `invalid_json` | 400 | The body is not a single valid JSON object. | Send one JSON object with the documented fields. |
| `unsupported_media_type` | 415 | A request with a body did not say `Content-Type: application/json`. | Set the header. |
| `payload_too_large` | 413 | The body is over 64 KB. | Send less: no request in this API needs that much. |
| `idempotency_key_required` | 400 | `POST /payment-requests` without an `Idempotency-Key` header. | Add one; see [idempotency](idempotency-and-retries.md). |
| `idempotency_conflict` | 409 | This `Idempotency-Key` was used before with a different body. | A bug: one key was used for two different bills. Fix how you build keys. |
| `duplicate_reference` | 409 | An `open` or `partially_paid` request with this `reference` already exists. | Fetch it with `GET /payment-requests?reference=...` and use it, or cancel it first if nothing is paid. |
| `payer_closed` | 409 | Talksasa staff have closed this payer's account, so it can't receive new bills. | Contact Talksasa. |
| `not_cancellable` | 409 | The request has money on it or is already paid or expired. Only open requests with nothing paid can be cancelled. | Don't retry. Talk to Talksasa finance if the bill is wrong. |
| `internal_error` | 500 | Something went wrong on Talksasa's side. Nothing secret is in the response. | Retry with backoff (writes are idempotent). If it persists, send the `request_id` to Talksasa. |

New codes may be added. Treat an unknown `4xx` code as "don't retry, log
and alert", and an unknown `5xx` as retryable.

> For the lead to verify: the status column against the final
> `internal/productapi` handlers (taken from the work in progress), in
> particular 400 vs 422 for `validation_failed` and whether a suspended
> product's key gets `401` or `403 product_suspended`.
