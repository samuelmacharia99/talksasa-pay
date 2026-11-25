# talksasa/pay-php

PHP client for the Talksasa Pay app API (`/v1`), webhook signature
verification, and a Laravel bridge. The core works in any framework or none;
the Laravel parts (`Talksasa\Pay\Laravel\*`) load only in Laravel.

- PHP 8.2+, `ext-json`.
- Any PSR-18 HTTP client. If none is passed, the client uses **Guzzle 7**
  when installed (every Laravel app already has it), so the timeout
  settings apply; otherwise `php-http/discovery` finds whatever PSR-18
  client is installed.
- Laravel 12 or 13 for the bridge (auto-discovered).

The contract is `api/app.openapi.yaml` in the Talksasa Pay repository (a
contract test keeps this package in step with it).

## How it works

Talksasa Pay is a payment gateway, like Paystack or Flutterwave, for the
Talksasa M-Pesa paybill. Your app creates a **charge** ("collect KES 1,500
for my reference INV-1042"); the gateway prompts the customer's phone (STK
push) and/or the customer pays at the paybill with the charge's account
number; the gateway confirms the payment with Safaricom and tells your app,
with the M-Pesa receipt, by webhook and in the events feed. The gateway
knows nothing about your customers or invoices: keep your own mapping from
`reference` (and `metadata`) to your records.

Money rules you must handle:

- Amounts are **integer KES**, 1 to 250,000 per charge. A charge's amount,
  reference and account number never change: to charge a different amount,
  cancel and create a new charge.
- `reference` is unique among your charges (`DuplicateReferenceException`).
- A charge is `succeeded` only when Safaricom confirms a payment with a
  receipt for exactly its amount. **Mark your invoice paid only on
  `charge.succeeded`** (`ChargeSucceeded`), never on a sent prompt or
  `needs_review`.
- A failed prompt does **not** fail the charge: it stays `pending` and you
  can prompt again.
- Late money is never dropped: a charge you cancelled, or that expired, can
  still become `succeeded`.
- Money that pays no waiting charge arrives as `payment.received`
  (`PaymentReceived`): a customer paying an already-succeeded charge again
  (`duplicateOfCharge` set: refund or credit it yourself), or a paybill
  payment with your account prefix that matched no waiting charge (you
  decide what it pays).
- Talksasa staff may reverse a payment: `transaction.reversed`
  (`TransactionReversed`), and the charge becomes `reversed`.
- Your app can request and read money; it can never record, move or refund
  it.

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
"require": { "talksasa/pay-php": "^0.2" }
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

Check the credentials: `php artisan tinker` then `TalksasaPay::whoami()`
(your app's name and account prefix).

### "Pay invoice": create a charge, then checkout or prompt

The whole flow, as tested in `tests/Laravel/PayInvoiceExampleTest.php`:

```php
use Illuminate\Http\Request;
use Talksasa\Pay\Exception\{AmountTooLargeException, MpesaErrorException, MpesaUnavailableException,
    NotPayableException, StkInProgressException, ValidationException};
use Talksasa\Pay\Laravel\Facades\TalksasaPay;

Route::post('/invoices/{invoice}/pay', function (Request $request, Invoice $invoice) {
    if ($invoice->status === 'paid') {
        return view('invoices.paid', compact('invoice'));
    }

    // One charge per invoice: create it once (the invoice number is the
    // reference), then reuse it.
    if ($invoice->talksasa_charge_id === null) {
        $charge = TalksasaPay::createCharge(
            amount: $invoice->total_kes,
            reference: $invoice->number,
            description: "Invoice {$invoice->number}",
            metadata: ['invoice_id' => $invoice->id],
            idempotencyKey: "invoice-{$invoice->id}-charge",   // a retry after a crash returns the same charge
        );
        $invoice->update(['talksasa_charge_id' => $charge->id]);
    } else {
        $charge = TalksasaPay::getCharge($invoice->talksasa_charge_id);
    }
    if ($charge->isSucceeded()) {
        return view('invoices.paid', compact('invoice'));
    }

    // Option 1: the hosted checkout page (prompt + paybill instructions + live status).
    if (! $request->filled('phone')) {
        return redirect()->away($charge->checkoutUrl);
    }

    // Option 2: prompt the phone from your own page.
    try {
        TalksasaPay::stkPush($charge->id, $request->input('phone'));
    } catch (StkInProgressException) {
        // A prompt is already on the phone: show the same screen.
    } catch (ValidationException) {
        return back()->withErrors(['phone' => 'Enter a Safaricom number, e.g. 0712 345 678.']);
    } catch (AmountTooLargeException | NotPayableException | MpesaUnavailableException | MpesaErrorException) {
        return view('invoices.paybill', ['paybill' => $charge->paybill]);   // business number + account number
    }
    return view('invoices.check-phone');   // "Check your phone and enter your M-Pesa PIN"
});
```

`createCharge(phone: ...)` sends the first prompt in the same call (see
`$charge->latestStkPush`); the separate `stkPush()` is for "try again".

Then listen (queued listeners; see the rules below):

```php
use Talksasa\Pay\Laravel\Events\{ChargeSucceeded, PaymentReceived, StkPushFailed, TransactionReversed};

// Paid. Also fires for a charge you cancelled or that expired (late money).
Event::listen(ChargeSucceeded::class, function (ChargeSucceeded $e) {
    Invoice::where('number', $e->charge->reference)
        ->where('status', '!=', 'paid')                                    // idempotent
        ->update(['status' => 'paid', 'paid_by_transaction' => $e->charge->transactionId]);
});

// Money that paid no waiting charge.
Event::listen(PaymentReceived::class, function (PaymentReceived $e) {
    $p = $e->payment;   // Talksasa\Pay\Model\PaymentReceived (a Transaction)
    if ($p->isDuplicate()) {
        // Paid twice for charge $p->duplicateOfCharge: refund or credit it.
        Refund::queue($p->receipt, $p->amount, $p->phone);
        return;
    }
    // A paybill payment with your prefix, e.g. to the customer's standing
    // account number HOST-1042: you decide what it pays.
    Payments::applyByAccountNumber($p->accountNumber, $p->amount, $p->id, $p->receipt);
});

// Staff reversed a payment: re-open what it paid.
Event::listen(TransactionReversed::class, function (TransactionReversed $e) {
    Invoice::where('paid_by_transaction', $e->transaction->reversesTransactionId)
        ->update(['status' => 'unpaid', 'paid_by_transaction' => null]);
});

// Prompt not paid (cancelled, timed out, insufficient funds...). The charge stays pending.
Event::listen(StkPushFailed::class, fn (StkPushFailed $e) => /* show $e->stkPush->resultDesc, offer "try again" */ null);
```

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
| `charge.succeeded` (paid; mark the invoice paid) | `ChargeSucceeded` | `$e->charge` |
| `charge.needs_review` (success without receipt, staff checking; NOT paid) | `ChargeNeedsReview` | `$e->charge` |
| `charge.expired` | `ChargeExpired` | `$e->charge` |
| `charge.cancelled` | `ChargeCancelled` | `$e->charge` |
| `stk_push.sent` (prompt on the phone; not paid) | `StkPushSent` | `$e->stkPush` |
| `stk_push.failed` (cancelled, timed out, ...: `resultDesc`; charge stays pending) | `StkPushFailed` | `$e->stkPush` |
| `stk_push.succeeded` (receipt recorded; `charge.succeeded` comes with it) | `StkPushSucceeded` | `$e->stkPush` |
| `stk_push.needs_review` (success without receipt; not paid) | `StkPushNeedsReview` | `$e->stkPush` |
| `payment.received` (a duplicate, or a paybill payment that matched no charge) | `PaymentReceived` | `$e->payment` |
| `transaction.reversed` (staff reversed a payment) | `TransactionReversed` | `$e->transaction` (the reversal) |
| every event, incl. types this version does not know | `WebhookReceived` | `$e->event` |

All live in `Talksasa\Pay\Laravel\Events`, carry the raw `$e->event`
(`Talksasa\Pay\Model\Event`) and `$e->source` (`webhook` or `poll`). Each
event is dispatched as its specific class (if known), then as
`WebhookReceived`.

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
  `$e->charge->status`), and when in doubt re-read with
  `TalksasaPay::getCharge($id)`.
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
use Talksasa\Pay\Model\EventType;
use Talksasa\Pay\Webhook\{Verifier, WebhookException};

$client = new Client(
    apiKey: getenv('TALKSASA_PAY_API_KEY'),
    baseUrl: getenv('TALKSASA_PAY_BASE_URL'),
    // httpClient: any PSR-18 client; requestFactory/streamFactory: PSR-17
);

$charge = $client->createCharge(1500, 'INV-1042', 'Hosting, Dec', idempotencyKey: 'invoice-1042-charge');
header('Location: ' . $charge->checkoutUrl, true, 303);

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
    match ($event->type) {
        EventType::CHARGE_SUCCEEDED => mark_paid($event->charge()->reference, $event->charge()->transactionId),
        EventType::PAYMENT_RECEIVED => route_payment($event->paymentReceived()),
        EventType::TRANSACTION_REVERSED => reopen($event->transaction()->reversesTransactionId),
        default => null,                                    // stk_push.*, charge.expired/cancelled, future types
    };
}
http_response_code(200);

// Cron backstop
$poller = new EventPoller($client, new CallbackCursorStore(
    fn () => $db->fetchOne('SELECT cursor FROM tsp_cursor WHERE id = 1') ?: null,
    fn (string $c) => $db->executeStatement('UPDATE tsp_cursor SET cursor = ? WHERE id = 1', [$c]),
));
$poller->poll(fn ($event) => already_processed($event->id) || handle($event));
```

## Client reference

| Method | API |
|---|---|
| `whoami(): WhoAmI` | `GET /whoami` (`appId`, `appName`, `accountPrefix`, `keyId`) |
| `createCharge(amount, reference, ?description, ?phone, ?accountNumber, ?metadata, ?expiresIn, ?idempotencyKey): Charge` | `POST /charges` |
| `getCharge(id): Charge` | `GET /charges/{id}` |
| `listCharges(?status, ?reference, ?cursor, ?limit): Page<Charge>` / `charges(?status, ?reference, ?pageSize): Generator<Charge>` | `GET /charges` |
| `cancelCharge(id): Charge` | `POST /charges/{id}/cancel` |
| `stkPush(chargeId, phone, ?idempotencyKey): StkPush` | `POST /charges/{id}/stk-push` |
| `getStkPush(id): StkPush` | `GET /stk-pushes/{id}` |
| `listTransactions(?chargeId, ?since, ?cursor, ?limit): Page<Transaction>` / `transactions(?chargeId, ?since, ?pageSize): Generator<Transaction>` | `GET /transactions` |
| `getTransaction(id): Transaction` | `GET /transactions/{id}` |
| `listEvents(?after, ?limit): EventPage` / `events(?after, ?pageSize): Generator<cursor, Event>` | `GET /events` |

Models (`Talksasa\Pay\Model`, read-only): `Charge` (`status`, `checkoutUrl`,
`paybill`, `accountNumber`, `metadata`, `transactionId`, `latestStkPush`,
`isSucceeded()`, `isPending()`, `needsReview()`), `StkPush` (`chargeId`,
`status`, `resultCode`, `resultDesc`, `isPending()`, `isFailed()`,
`wasCancelledByCustomer()`), `Transaction` (`kind`, `channel`, `amount`,
`receipt`, `accountNumber`, `chargeId`, `reversesTransactionId`,
`isReversal()`), `PaymentReceived` (a `Transaction` plus
`duplicateOfCharge`, `isDuplicate()`), and `Event` (`type`, `data`,
`rawData`, typed accessors `charge()`, `stkPush()`, `paymentReceived()`,
`transaction()`). `EventType::ALL` lists the event types.

Generators fetch further pages lazily:

```php
foreach ($client->charges(status: 'pending') as $charge) { ... }
foreach ($client->transactions(chargeId: $charge->id) as $txn) { ... }
```

## Idempotency

- `createCharge` and `stkPush` always send an `Idempotency-Key`, and the
  client's automatic retries re-send the **same** key, so one call never
  creates two charges or sends two prompts. Without `$idempotencyKey` a
  random UUIDv4 is generated per call.
- Pass your own key derived from your record (`"invoice-{$id}-charge"`) so a
  retry after a crash, timeout or deploy returns the original charge (200)
  instead of creating a second one. After a `TransportException` or a
  `ServerException` with `$errorCode === 'timeout'`, retry with the same key.
- Same key + different body is `IdempotencyConflictException`. Keys are
  kept forever.
- `reference` is unique among your charges (`DuplicateReferenceException`):
  look the charge up with `listCharges(reference: ...)`. To charge the same
  invoice again (e.g. a new amount after cancelling), use a new reference
  such as `INV-1042-2`.
- `cancelCharge` is idempotent (cancelling a cancelled charge returns it).
- Only one prompt per charge can be live (`StkInProgressException`). To try
  again after a **failed** prompt, call `stkPush` again with a new (or no)
  key: the old key would just return the failed prompt.

## Errors

Every exception implements `Talksasa\Pay\Exception\TalksasaPayException`.
Every problem code in the spec maps to a class
(`ApiException::CLASSES_BY_CODE`); unknown codes fall back by HTTP status.

| Exception | When |
|---|---|
| `ApiException` (base) | any error response: `$status`, `$errorCode` (stable), `$title`, `$detail`, `$requestId`, `$fieldErrors`, `$retryAfter` |
| `AuthenticationException` | 401 `unauthorized` |
| `PermissionDeniedException` | 403; subclasses `IpNotAllowedException` (`ip_not_allowed`) and `AppSuspendedException` (`app_suspended`) |
| `NotFoundException` | 404 `not_found` (also another app's ids) |
| `ValidationException` | `validation_failed` (see `$fieldErrors`), `invalid_json`, `idempotency_key_required`, `unsupported_media_type`, `payload_too_large`, `method_not_allowed` |
| `ConflictException` | 409; subclasses `DuplicateReferenceException`, `IdempotencyConflictException`, `NotCancellableException` (the charge is not pending), and for `stkPush`: `StkInProgressException` (a prompt is already live; `liveStkPushId()`), `NotPayableException` (the charge is not pending), `AmountTooLargeException` (above the M-Pesa prompt limit: use the paybill) |
| `RateLimitedException` | 429 `rate_limited` after retries (or a Retry-After too long to wait inline) |
| `ServerException` | 5xx after retries (`internal_error`, `not_implemented`, `overloaded`, `busy`, `timeout`); subclasses (never retried) `MpesaUnavailableException` (503 `mpesa_unavailable`: prompts are off) and `MpesaErrorException` (502 `mpesa_error`: Safaricom could not send the prompt; nothing charged; `stkPushId()`) |
| `TransportException` | no HTTP response (DNS, connect, TLS, timeout) after retries |
| `UnexpectedResponseException` | 2xx with a body that breaks the contract (never silently zero) |

Quote `$e->requestId` when asking Talksasa for help.

```php
try {
    $charge = TalksasaPay::createCharge($invoice->total_kes, $invoice->number);
} catch (DuplicateReferenceException) {
    $charge = TalksasaPay::listCharges(reference: $invoice->number)->data[0];
} catch (ValidationException $e) {
    foreach ($e->fieldErrors as $fe) { $errors[$fe->field] = $fe->message; }
} catch (TalksasaPayException $e) {
    report($e);       // retry later with the same idempotency key
}
```

### Retries

The client retries only safe requests: GETs, `createCharge` and `stkPush`
(with the same Idempotency-Key) and `cancelCharge`, on network errors, 429,
502, 503 and 504 (default 2 retries) with exponential backoff (0.5 s, 1 s,
... capped at 8 s, equal jitter). The M-Pesa answers `mpesa_unavailable`
and `mpesa_error` are final and **never** retried. A `Retry-After` header
is respected as a minimum; one longer than `max_retry_after` is not waited
out inline (the exception carries `$retryAfter` so a job can `release()`
itself). 500 and 4xx are never retried. Tune with `new RetryPolicy(...)` or
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
