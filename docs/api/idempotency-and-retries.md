# Idempotency and retries

Networks fail. A request can time out after Talksasa Pay has already done the
work, and you can't tell from your side. Every call in this API is designed
so that you can simply send it again.

## Which calls are safe to retry

| Call | Safe to retry? | Why |
|---|---|---|
| Any `GET` | Yes | Reads change nothing. |
| `PUT /payers/{external_ref}` | Yes | An upsert: the payer ends up with the details you sent, however many times you send them. |
| `POST /payment-requests` | Yes, **with the same `Idempotency-Key` and the same body** | The key identifies the request. A repeat returns the original instead of creating a second bill. |
| `POST /payment-requests/{id}/cancel` | Yes | Cancelling a request that is already cancelled returns it with `200`. |
| `POST /payment-requests/{id}/stk-push` | Yes, **with the same `Idempotency-Key` and the same phone** | A repeat returns the same prompt instead of sending a second one to the customer's phone. Use a new key only to try again after a prompt failed. Never retry `502 mpesa_error` or `503 mpesa_unavailable` automatically. See [M-Pesa payment prompts](stk-push.md). |

## The `Idempotency-Key` header

`POST /payment-requests` and `POST /payment-requests/{id}/stk-push` require
an `Idempotency-Key` header (1-255 printable characters). Without it you get `400 idempotency_key_required`.

Rules:

- **One key per bill you mean to create.** Derive it from something stable on
  your side, such as `invoice-INV-2026-0042` or a UUID you store with the
  invoice before you call. Don't generate a fresh random key on each retry:
  that defeats the purpose.
- **Same key, same body: you get the original back.** The response is `200`
  (not `201`) with the header `Idempotent-Replayed: true` and the request as
  it is now, which may already be paid.
- **Same key, different body: `409 idempotency_conflict`.** Talksasa Pay
  refuses to guess which one you meant. This is a bug in your code: you
  reused a key for a different bill.
- **Keys are kept forever.** You can't reuse a key later for a new bill, even
  years later. To bill the same invoice again after cancelling, use a new key
  such as `invoice-INV-2026-0042-v2`.

## What `409` means

| `code` | Meaning | What to do |
|---|---|---|
| `idempotency_conflict` | This `Idempotency-Key` was used before with a different body. | Fix the code that builds keys. Don't retry with the same key. |
| `duplicate_reference` | You already have an open or partially paid request with this `reference`. | Look it up with `GET /payment-requests?reference=...` and use it. If you really mean to replace it, cancel it first (only possible if nothing has been paid). |
| `not_cancellable` | You tried to cancel a request that has money on it, or that is paid or expired. | Don't retry. Talk to Talksasa finance if the bill is wrong. |
| `payer_closed` | The payer's account has been closed by Talksasa staff. | Don't retry. Contact Talksasa. |
| `stk_in_progress` | A payment prompt for this request is already on the customer's phone. | Don't send another; wait for its `stk_push.*` event. |
| `not_payable` / `amount_too_large` | The request can't be prompted (paid, closed, or above KES 250,000). | Don't retry. Show the request's status or the paybill steps. |

`duplicate_reference` also protects you if your first `POST` succeeded, the
response was lost, and your retry used a different key: you get a `409`
instead of a second bill. Handle it by fetching the existing request.

## Timeouts

- Set a client timeout on every call. 30 seconds total and 5 seconds to
  connect works well. The server stops working on a request after about 15
  seconds and answers `503 overloaded`.
- After a timeout, retry the same call (same `Idempotency-Key` for `POST
  /payment-requests`). Don't assume it failed.

## When to retry and how fast

| Response | Retry? |
|---|---|
| Timeout or connection error | Yes, with backoff. |
| `429 rate_limited` | Yes, after `Retry-After` seconds. |
| `503 overloaded` | Yes, after `Retry-After` seconds. |
| `500 internal_error`, `502`, `504` | Yes, with backoff. If it keeps failing, contact Talksasa with the `request_id`. |
| Any other `4xx` | No. The same request will fail the same way. Fix the cause first. |

Backoff: wait `Retry-After` when it is present. Otherwise wait about 1 s,
then 2 s, 4 s, 8 s, and so on, up to 1 minute, with random jitter (any value
between zero and the current step) so that many workers don't retry in
step. Give up after about 5 attempts in a web request, and hand the job to a
background queue that keeps trying for longer.

```php
// Sketch: retry with Retry-After and full jitter.
for ($attempt = 1; ; $attempt++) {
    $res = $http->send($request);           // same Idempotency-Key every time
    $status = $res->getStatusCode();
    $retryable = in_array($status, [429, 500, 502, 503, 504], true);
    if (!$retryable || $attempt === 5) {
        return $res;
    }
    $wait = $res->hasHeader('Retry-After')
        ? (int) $res->getHeaderLine('Retry-After')
        : random_int(0, min(60, 2 ** ($attempt - 1)) * 1000) / 1000;
    usleep((int) ($wait * 1_000_000));
}
```

The PHP SDK does this for you; see [Laravel](laravel.md).

## Rate limits

Each key has a generous per-key request budget that refills continuously.
Normal integrations never reach it. If you do, you get `429 rate_limited`
with `Retry-After`. Batch jobs (for example monthly renewals) should send
requests one after another, not all at once.
