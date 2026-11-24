# talksasa/pay-php

PHP client for the Talksasa Pay product API (`/v1`), webhook signature
verification, and a Laravel bridge. The core works in any framework or none;
the Laravel parts (`Talksasa\Pay\Laravel\*`) load only in Laravel.

- PHP 8.2+, `ext-json`.
- Any PSR-18 HTTP client. If none is passed, the client uses **Guzzle 7**
  when installed (every Laravel app already has it), so the timeout
  settings apply; otherwise `php-http/discovery` finds whatever PSR-18
  client is installed.
- Laravel 12 or 13 for the bridge (auto-discovered).

The contract is `api/product.openapi.yaml` in the Talksasa Pay repository.
Money is always **integer KES**. Products can request and read money; they
can never record, move or refund it.

## Install

The package lives in `clients/php` of the Talksasa Pay repository. Until it
is published to a private Composer repository, install it from a path or
VCS repository:

```jsonc
// composer.json of your app
"repositories": [
    { "type": "path", "url": "../talksasa-pay/clients/php" }
    // or: { "type": "vcs", "url": "git@github.com:talksasa/pay-php.git" } after a subtree split
],
"require": { "talksasa/pay-php": "^0.1" }
```

```sh
composer require talksasa/pay-php
```

## Laravel

### Configure

Add to `.env` (never commit real values):

```dotenv
TALKSASA_PAY_BASE_URL=https://<talksasa-pay-api-host>
TALKSASA_PAY_API_KEY=tsp_live_...        # tsp_test_... outside production
TALKSASA_PAY_WEBHOOK_SECRET=whsec_...
# During a secret rotation (24 h), also:
# TALKSASA_PAY_WEBHOOK_SECRET_PREVIOUS=whsec_...old
```

Optional: `TALKSASA_PAY_TIMEOUT` (15 s), `TALKSASA_PAY_CONNECT_TIMEOUT` (5),
`TALKSASA_PAY_RETRIES` (2), `TALKSASA_PAY_MAX_RETRY_AFTER` (30),
`TALKSASA_PAY_WEBHOOK_TOLERANCE` (300), `TALKSASA_PAY_DEDUPE_STORE`,
`TALKSASA_PAY_DEDUPE_TTL` (7 days), `TALKSASA_PAY_CURSOR_STORE`
(`cache`|`database`), `TALKSASA_PAY_ALLOW_INSECURE_HTTP` (development only).

To customise further, publish the config (and the cursor table migration if
you use the database cursor store, which is recommended in production):

```sh
php artisan vendor:publish --tag=talksasa-pay-config
php artisan vendor:publish --tag=talksasa-pay-migrations && php artisan migrate
```

Check the credentials: `php artisan tinker` then `TalksasaPay::whoami()`.

### Create a payer and a payment request, send the customer to pay

```php
use Talksasa\Pay\Client;
use Talksasa\Pay\Exception\DuplicateReferenceException;
use Talksasa\Pay\Laravel\Facades\TalksasaPay;

// 1. Upsert the customer (idempotent; full replacement: always send the
//    current email and the full phone list).
$payer = TalksasaPay::upsertPayer(
    externalRef: (string) $user->id,
    displayName: $user->name,
    email: $user->email,
    phones: [$user->phone],             // 0712..., +254712..., 254712...
);
// $payer->accountNumber, e.g. "HOST-1042": what the customer types as the
// M-Pesa paybill account number. Show it on their billing page.

// 2. Ask them to pay an invoice. Pass a stable idempotency key.
$request = TalksasaPay::createPaymentRequest(
    payer: (string) $user->id,
    reference: $invoice->number,         // only one open request per reference
    description: "Cloud hosting, {$invoice->period}",
    amount: $invoice->total_kes,         // int, whole shillings
    expiresAt: now()->addDays(14),       // optional
    idempotencyKey: "invoice-{$invoice->id}-v1",
);

$invoice->update(['talksasa_request_id' => $request->id]);

// Credit the payer already held is applied at once:
if ($request->isPaid()) {
    // nothing to pay; you'll also get a payment_request.paid event
}

// 3. Redirect to the hosted pay page (STK push and paybill instructions).
return redirect()->away($request->payUrl);
```

Or inject `Talksasa\Pay\Client` instead of using the facade.

### Handle webhooks

Register the route (in `routes/web.php` or `routes/api.php`):

```php
Route::talksasaPayWebhook('webhooks/talksasa-pay');   // POST, named talksasa-pay.webhook
```

and give Talksasa the full URL (`https://your-app/webhooks/talksasa-pay`).
The macro adds the signature-checking middleware and removes CSRF
protection from that route only (the signature is the authentication).

The middleware verifies `Talksasa-Signature` against the raw body (400 on
failure), then the controller de-duplicates by event id in the Laravel cache
and dispatches Laravel events:

| Talksasa event | Laravel event | Payload |
|---|---|---|
| `payment_request.created` | `PaymentRequestCreated` | `$e->paymentRequest` |
| `payment_request.partially_paid` | `PaymentRequestPartiallyPaid` | `$e->paymentRequest` |
| `payment_request.paid` | `PaymentRequestPaid` | `$e->paymentRequest` |
| `payment_request.reopened` (a payment was reversed) | `PaymentRequestReopened` | `$e->paymentRequest` |
| `payment_request.cancelled` | `PaymentRequestCancelled` | `$e->paymentRequest` |
| `payment_request.expired` | `PaymentRequestExpired` | `$e->paymentRequest` |
| `transaction.allocated` | `TransactionAllocated` | `$e->transaction` |
| `transaction.reversed` | `TransactionReversed` | `$e->transaction` |
| `payer.credit_changed` | `PayerCreditChanged` | `$e->creditChange` |
| every event, incl. future types | `WebhookReceived` | `$e->event` |

All live in `Talksasa\Pay\Laravel\Events`, carry the raw `$e->event`
(`Talksasa\Pay\Model\Event`) and `$e->source` (`webhook` or `poll`).

```php
use Illuminate\Contracts\Queue\ShouldQueue;
use Talksasa\Pay\Laravel\Events\PaymentRequestPaid;

final class MarkInvoicePaid implements ShouldQueue
{
    public function handle(PaymentRequestPaid $e): void
    {
        $pr = $e->paymentRequest;   // state at the time of the event
        Invoice::where('number', $pr->reference)
            ->where('status', '!=', 'paid')          // idempotent
            ->update(['status' => 'paid', 'paid_at' => $pr->paidAt]);
    }
}
```

Rules for listeners:

- **Be idempotent.** Delivery is at least once. The package's de-duplication
  (cache `add`, atomic) removes almost all repeats, but a cache flush or
  eviction re-opens the window. Make the state change itself safe to repeat
  (as above).
- **Be fast or queued.** Talksasa waits 10 s; anything else is retried for
  about 3 days. Use `ShouldQueue` listeners. If a synchronous listener
  throws, the de-duplication key is released and the webhook answers 500,
  so Talksasa redelivers it.
- **Events can arrive out of order.** Act on the data's status (e.g.
  `$pr->status`), and when in doubt re-read with
  `TalksasaPay::getPaymentRequest($id)`.
- Use a cache store shared by all app servers for de-duplication
  (`TALKSASA_PAY_DEDUPE_STORE=redis`, `database`, ...), not `array`/`file`
  on multiple hosts.

### Poll events as a backstop (reconcile by feed)

`GET /v1/events` is a gap-free feed of every event. The
`talksasa-pay:poll-events` command reads it from a stored cursor and
dispatches the same Laravel events (de-duplicated against webhooks), so an
event whose webhook never arrived (outage, misconfigured URL, firewall) is
still handled. Schedule it in `routes/console.php`:

```php
use Illuminate\Support\Facades\Schedule;

Schedule::command('talksasa-pay:poll-events')->everyFiveMinutes()->withoutOverlapping();
```

- The cursor is saved after each fully handled page. If a listener throws,
  the command exits non-zero and the page is read again next run.
- Store the cursor durably: `TALKSASA_PAY_CURSOR_STORE=database` (publish and
  run the migration) or a cache store that never evicts. With no cursor the
  feed is read from the beginning; that is safe (idempotent listeners plus
  de-duplication) but slow for a large history.
- The command also takes a cache lock so two runs never overlap.
- `--max-pages=N` bounds a run; `--page-size=N` (1-200).
- You can rely on polling alone (no webhooks); latency is then the schedule
  interval.

## Plain PHP (no Laravel)

```php
use Talksasa\Pay\Client;
use Talksasa\Pay\Events\{EventPoller, CallbackCursorStore};
use Talksasa\Pay\Webhook\{Verifier, WebhookException};

$client = new Client(
    apiKey: getenv('TALKSASA_PAY_API_KEY'),
    baseUrl: getenv('TALKSASA_PAY_BASE_URL'),
    // httpClient: any PSR-18 client; requestFactory/streamFactory: PSR-17
);

$request = $client->createPaymentRequest('cust-77', 'INV-1042', 'Hosting, Dec', 1500, idempotencyKey: 'invoice-1042-v1');
header('Location: ' . $request->payUrl, true, 303);

// Webhook endpoint
try {
    $event = Verifier::verify(
        file_get_contents('php://input'),                 // RAW body, not re-encoded JSON
        $_SERVER['HTTP_TALKSASA_SIGNATURE'] ?? '',
        [getenv('TALKSASA_PAY_WEBHOOK_SECRET')],           // add the previous secret during rotation
    );
} catch (WebhookException $e) {
    http_response_code(400);
    exit;
}
if (!already_processed($event->id)) {                       // your de-duplication
    handle($event);                                         // $event->type, $event->paymentRequest(), ...
}
http_response_code(200);

// Cron backstop
$poller = new EventPoller($client, new CallbackCursorStore(
    fn () => $db->fetchOne('SELECT cursor FROM tsp_cursor WHERE id = 1') ?: null,
    fn (string $c) => $db->executeStatement('UPDATE tsp_cursor SET cursor = ? WHERE id = 1', [$c]),
));
$poller->poll(fn ($event) => already_processed($event->id) || handle($event));
```

Lists: `listPaymentRequests()`, `listTransactions()` return one `Page`
(`->data`, `->nextCursor`); `paymentRequests()`, `transactions()` and
`events()` are generators that fetch further pages lazily:

```php
foreach ($client->paymentRequests(status: 'open', payer: 'cust-77') as $pr) { ... }
foreach ($client->transactions(since: new DateTimeImmutable('-1 day')) as $txn) { ... }
```

## Idempotency

- `upsertPayer` (PUT) and `cancelPaymentRequest` are naturally idempotent.
- `createPaymentRequest` always sends an `Idempotency-Key`. Pass your own,
  derived from the invoice (`"invoice-{$id}-v1"`): then a retry after a
  crash, timeout or deploy returns the original request (200) instead of
  creating a second one. Without it a random UUIDv4 is used, which only
  covers the client's automatic retries within that one call.
- Same key + different body is `IdempotencyConflictException`. To bill a
  changed amount: cancel the old request and create a new one with a new key
  (`-v2`). Keys are kept forever.
- Only one open/partially-paid request may exist per `reference`
  (`DuplicateReferenceException`): look it up with
  `listPaymentRequests(reference: ...)`.
- After a `TransportException` on a create, retry with the **same** key.

## Errors

Every exception implements `Talksasa\Pay\Exception\TalksasaPayException`.

| Exception | When |
|---|---|
| `ApiException` (base) | any error response: `$status`, `$errorCode` (stable), `$title`, `$detail`, `$requestId`, `$fieldErrors`, `$retryAfter` |
| `AuthenticationException` | 401 `unauthorized` |
| `PermissionDeniedException` | 403 `ip_not_allowed`, `product_suspended` |
| `NotFoundException` | 404 `not_found` |
| `ValidationException` | `validation_failed` (see `$fieldErrors`), `invalid_json`, `idempotency_key_required`, `unsupported_media_type`, `payload_too_large` |
| `ConflictException` | 409; subclasses `DuplicateReferenceException`, `IdempotencyConflictException`, `NotCancellableException`, `PayerClosedException` |
| `RateLimitedException` | 429 after retries (or a Retry-After too long to wait inline) |
| `ServerException` | 5xx after retries |
| `TransportException` | no HTTP response (DNS, connect, TLS, timeout) after retries |
| `UnexpectedResponseException` | 2xx with a body that breaks the contract (never silently zero) |

Quote `$e->requestId` when asking Talksasa for help.

```php
try {
    $pr = TalksasaPay::createPaymentRequest(...);
} catch (DuplicateReferenceException) {
    // The live (open or partially paid) request for this invoice already exists.
    foreach (TalksasaPay::paymentRequests(reference: $invoice->number) as $candidate) {
        if ($candidate->isPayable()) { $pr = $candidate; break; }
    }
} catch (ValidationException $e) {
    foreach ($e->fieldErrors as $fe) { $errors[$fe->field] = $fe->message; }
} catch (TalksasaPayException $e) {
    report($e);       // retry later with the same idempotency key
}
```

### Retries

Every `/v1` call is idempotent, so the client retries network errors, 429,
502, 503 and 504 (default 2 retries) with exponential backoff (0.5 s, 1 s,
... capped at 8 s, equal jitter). A `Retry-After` header is respected as a
minimum; one longer than `max_retry_after` is not waited out inline (the
exception carries `$retryAfter` so a job can `release()` itself). 500 and
4xx are never retried. Tune with `new RetryPolicy(...)` or
`RetryPolicy::none()`.

## Security notes

- The API key and webhook secrets belong in the environment / secret store
  only. The client keeps the key in a `Secret` wrapper: `var_dump`,
  `print_r`, `json_encode`, string casts and stack traces show
  `[redacted]`; serializing the client throws (don't put it in queued job
  payloads: resolve it inside the job).
- The base URL must be `https` (plain `http` only to loopback, or with
  `allow_insecure_http` for local docker setups). Redirects are not followed,
  so the `Authorization` header never leaves the configured host.
- Webhooks: verify the signature on the **raw** body before trusting
  anything; reject timestamps more than 300 s away (replay protection); the
  comparison is constant time. Don't trust the `Talksasa-Event-*` headers;
  use the signed body. Answer 400 on failure without detail beyond the
  reason code.
- Treat webhook data as a notification; for money decisions you can always
  re-read the resource over the authenticated API.
- A key may be restricted to an IP allow-list; ask Talksasa to add your
  servers' egress IPs.

## Webhook signature algorithm (canonical)

Header: `Talksasa-Signature: t=<unix seconds>,v1=<hex>[,v1=<hex>]`

1. Split on `,`, trim spaces/tabs around each item, split each at the first
   `=`. Exactly one `t` (1-12 ASCII digits) is required. `v1` values that are
   not 64 hex digits are ignored (compared lowercase); at least one
   well-formed `v1` is required. Other keys are ignored. An item without `=`
   (or with an empty key) makes the header malformed.
2. Reject if `|now - t| > 300` (either direction; exactly 300 is accepted).
3. `signed_payload = t + "." + raw_body` (exact bytes as received).
4. `expected = hex(HMAC-SHA256(key = the full secret string as bytes,
   including "whsec_", message = signed_payload))`.
5. Accept if any `v1` equals `expected` for any configured secret (two `v1`
   appear for 24 h after a rotation), compared in constant time.

Test vectors (generated independently with Python's `hmac`, verified by this
package's tests): `tests/fixtures/webhook_signature_vectors.json`. Each vector
has `secrets`, `payload`, `header`, `now`, `tolerance` and `expected`
(`valid` or a reason: `missing_header`, `malformed_header`,
`timestamp_out_of_tolerance`, `no_matching_signature`).

To test your own endpoint, sign a body like Talksasa does:
`Verifier::sign($body, $secret)`.

## Testing your app

- Mock `Talksasa\Pay\Client` (it is not final), or bind a PSR-18 fake as
  `talksasa-pay.http_client` in the container.
- Post signed webhooks in feature tests:

```php
$body = json_encode($eventArray);
$this->call('POST', '/webhooks/talksasa-pay', [], [], [], [
    'CONTENT_TYPE' => 'application/json',
    'HTTP_TALKSASA_SIGNATURE' => \Talksasa\Pay\Webhook\Verifier::sign($body, config('talksasa-pay.webhook_secrets')[0]),
], $body)->assertOk();
```

## Developing this package

Tooling runs in Docker:

```sh
cd clients/php
docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:/app" -w /app -e COMPOSER_HOME=/tmp/composer composer:2 install
docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:/app" -w /app php:8.4-cli vendor/bin/phpunit
```

(`composer:2` resolves for PHP 8.4, which pulls Laravel 13 / Symfony 8; to
test on PHP 8.2/8.3, run `composer config platform.php 8.2.0 && composer
update` in a scratch copy first.)

`composer.lock` is not committed: this is a library, and consumers resolve
its dependencies inside their own application lock file.
