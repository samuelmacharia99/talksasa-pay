# Laravel integration

The PHP SDK `talksasa/pay-php` (source in [`clients/php`](../../clients/php))
wraps the API for any PHP 8.2+ app and ships a Laravel bridge: a config file,
a facade, a webhook endpoint that verifies signatures, Laravel events for
each Talksasa Pay event, and an Artisan command that polls the events feed.

## Install

```sh
composer require talksasa/pay-php
php artisan vendor:publish --tag=talksasa-pay-config
```

The service provider and the `TalksasaPay` facade register themselves
through package discovery.

## Configure

Add to `.env` (never commit real values):

```dotenv
TALKSASA_PAY_BASE_URL=https://<api-host>          # ask Talksasa staff; the client adds /v1
TALKSASA_PAY_API_KEY=tsp_test_...                  # tsp_live_... in production
TALKSASA_PAY_WEBHOOK_SECRET=whsec_...
# Only during a secret rotation (24 hours): the old secret.
TALKSASA_PAY_WEBHOOK_SECRET_PREVIOUS=
```

Other settings in `config/talksasa-pay.php` have sensible defaults: timeouts
(15 s per attempt, 5 s to connect), automatic retries on network errors,
`429`, `502`, `503` and `504`, the cache store used to de-duplicate events,
and where the poller keeps its cursor.

Use a cache store that all your app servers share (Redis, database or
Memcached) for de-duplication. The `file` and `array` stores only work on a
single server.

## Make calls

```php
use TalksasaPay;   // facade

// When a customer signs up or changes their details.
$payer = TalksasaPay::upsertPayer(
    externalRef: (string) $customer->id,
    displayName: $customer->name,
    email: $customer->email,
    phones: [$customer->phone],
);
$customer->update(['mpesa_account' => $payer->accountNumber]);

// When an invoice is issued.
$request = TalksasaPay::createPaymentRequest(
    payer: (string) $customer->id,
    reference: $invoice->number,
    description: "Cloud Hosting, {$invoice->period}",
    amount: $invoice->total_kes,                    // integer KES
    idempotencyKey: "invoice-{$invoice->id}",       // stable: safe to retry
);
$invoice->update([
    'talksasa_request_id' => $request->id,
    'pay_url' => $request->payUrl,
]);

return redirect()->away($request->payUrl);
```

Always pass `idempotencyKey` derived from your own record. Without it the
SDK makes up a random key, which protects only the retries inside that one
call, not a retry of your whole job after a crash.

The SDK throws a specific exception for each error `code` (all extend
`Talksasa\Pay\Exception\TalksasaPayException`), for example
`DuplicateReferenceException`, `IdempotencyConflictException`,
`NotCancellableException`, `ValidationException` (with the field errors) and
`RateLimitedException`. See [errors](errors.md) for what each means.

## Receive webhooks

Register the webhook route with the package's route macro and give
Talksasa staff the full URL:

```php
// routes/web.php or routes/api.php
use Illuminate\Support\Facades\Route;

Route::talksasaPayWebhook('hooks/talksasa-pay');   // POST, named talksasa-pay.webhook
```

The macro puts the signature check (`VerifyWebhookSignature`) in front of
the package's `WebhookController` and removes CSRF protection from that one
route, because Talksasa signs every webhook instead. Don't point a route at
`WebhookController` yourself: without the signature check it refuses every
request with `400`.

The signature check verifies against your configured secrets and rejects
stale or forged requests with `400`. The controller then dispatches the
Laravel events and answers `200`; an event it has already handled is
answered `200` without dispatching again.

## Listen for Laravel events

Each Talksasa Pay event type has its own Laravel event class in
`Talksasa\Pay\Laravel\Events`:

| Talksasa Pay event | Laravel event |
|---|---|
| `payment_request.created` | `PaymentRequestCreated` |
| `payment_request.partially_paid` | `PaymentRequestPartiallyPaid` |
| `payment_request.paid` | `PaymentRequestPaid` |
| `payment_request.reopened` | `PaymentRequestReopened` |
| `payment_request.cancelled` | `PaymentRequestCancelled` |
| `payment_request.expired` | `PaymentRequestExpired` |
| `transaction.allocated` | `TransactionAllocated` |
| `transaction.reversed` | `TransactionReversed` |
| `payer.credit_changed` | `PayerCreditChanged` |
| `stk_push.sent` | `StkPushSent` |
| `stk_push.succeeded` | `StkPushSucceeded` |
| `stk_push.failed` | `StkPushFailed` |
| `stk_push.needs_review` | `StkPushNeedsReview` |
| every event, including types this SDK version doesn't know yet | `WebhookReceived` (dispatched after the specific one) |

Every event has `$event` (the envelope) and `$source` (`webhook` or `poll`).
Payment request events also have `$paymentRequest`; transaction events
`$transaction`; `PayerCreditChanged` has `$creditChange`; `stk_push.*` events `$stkPush` (a `Talksasa\Pay\Model\StkPush`
with `status`, `resultCode`, `resultDesc`, `checkoutRequestId`).

Use a queued listener so the webhook is answered within 10 seconds, and
re-read the request before you change an invoice:

```php
namespace App\Listeners;

use App\Models\Invoice;
use Illuminate\Contracts\Queue\ShouldQueue;
use Talksasa\Pay\Laravel\Events\PaymentRequestEvent;
use TalksasaPay;

class SyncInvoice implements ShouldQueue
{
    // Handles paid, partially_paid, reopened, cancelled and expired alike.
    public function handle(PaymentRequestEvent $e): void
    {
        $invoice = Invoice::where('talksasa_request_id', $e->paymentRequest->id)->first();
        if ($invoice === null) {
            return; // not ours, or created outside this app
        }
        // Events can arrive late or out of order: act on the current state.
        $current = TalksasaPay::getPaymentRequest($e->paymentRequest->id);
        $invoice->update([
            'status' => $current->status,
            'amount_paid' => $current->amountPaid,
        ]);
    }
}
```

Register it the usual way (Laravel discovers listeners by type-hint, or add
it in `EventServiceProvider`). Keep listeners idempotent: setting a status is
safe to repeat, adding to a balance is not.

## Pay invoice button

Send the M-Pesa prompt straight to the customer's phone from your own
"Pay invoice" button. The full flow, statuses and error codes are in
[M-Pesa payment prompts](stk-push.md).

**The rule: the invoice becomes paid only in the `PaymentRequestPaid`
listener.** `StkPushSent` and `StkPushNeedsReview` are never payment.

```php
namespace App\Http\Controllers;

use App\Models\Invoice;
use Illuminate\Http\Request;
use Talksasa\Pay\Exception\{AmountTooLargeException, MpesaErrorException, MpesaUnavailableException,
    NotPayableException, StkInProgressException, ValidationException};
use TalksasaPay;

class PayInvoiceController
{
    public function __invoke(Request $request, Invoice $invoice)
    {
        $phone = $request->validate(['phone' => ['required', 'string', 'max:20']])['phone'];

        // Create the payment request, or get the same one back on a second
        // click or after a crash: the idempotency key makes this safe.
        $pr = TalksasaPay::createPaymentRequest(
            payer: (string) $invoice->user_id,
            reference: $invoice->number,
            description: "Invoice {$invoice->number}",
            amount: $invoice->total_kes,
            idempotencyKey: "invoice-{$invoice->id}-v1",
        );
        $invoice->update(['talksasa_request_id' => $pr->id, 'stk_message' => null]);
        if ($pr->isPaid()) {
            return to_route('invoices.show', $invoice);      // credit already covered it
        }

        try {
            // A fresh key per click; the SDK reuses it on its own retries,
            // so one click never sends two prompts.
            TalksasaPay::stkPush($pr->id, $phone);
        } catch (StkInProgressException) {
            // Already on their phone: same screen.
        } catch (ValidationException) {
            return back()->withErrors(['phone' => 'Enter a Safaricom number, e.g. 0712 345 678.']);
        } catch (AmountTooLargeException | NotPayableException | MpesaUnavailableException | MpesaErrorException) {
            return view('invoices.paybill', ['paybill' => $pr->paybill]);   // pay by paybill instead
        }

        return view('invoices.check-phone', ['invoice' => $invoice]);
        // "Check your phone and enter your M-Pesa PIN." Poll your own
        // endpoint for $invoice->status and $invoice->stk_message.
    }
}
```

```php
use Talksasa\Pay\Laravel\Events\{PaymentRequestPaid, StkPushFailed};

// The ONLY place an invoice becomes paid.
Event::listen(function (PaymentRequestPaid $e) {
    Invoice::where('talksasa_request_id', $e->paymentRequest->id)
        ->where('status', '!=', 'paid')
        ->update(['status' => 'paid', 'paid_at' => $e->paymentRequest->paidAt]);
});

// Cancelled, timed out, insufficient funds, wrong PIN...: show why, offer a retry.
Event::listen(function (StkPushFailed $e) {
    Invoice::where('talksasa_request_id', $e->stkPush->paymentRequestId)
        ->where('status', '!=', 'paid')
        ->update(['stk_message' => $e->stkPush->resultDesc ?? 'The payment prompt failed.']);
});
```

On the "check your phone" screen, when `stk_message` is set, show it with a
**Try again** button (posting to the same controller) and the paybill steps
(`$pr->paybill->businessNumber`, account `$pr->paybill->accountNumber`).
Optionally show "We're confirming your payment with Safaricom" on
`StkPushNeedsReview`, without changing the invoice.

## Poll the events feed

The `talksasa-pay:poll-events` command reads [`GET /events`](events-feed.md)
from the stored cursor and dispatches the same Laravel events, skipping any
already handled by a webhook. Schedule it as the backstop:

```php
// routes/console.php (Laravel 11+)
use Illuminate\Support\Facades\Schedule;

Schedule::command('talksasa-pay:poll-events')
    ->everyMinute()
    ->withoutOverlapping()
    ->onOneServer();
```

Store the cursor in the database for production: set
`TALKSASA_PAY_CURSOR_STORE=database`, then publish and run the package's
migration, which creates a `talksasa_pay_cursors` table:

```sh
php artisan vendor:publish --tag=talksasa-pay-migrations
php artisan migrate
```

The default `cache` store is fine only with a persistent cache; if the
cursor is lost the command replays the whole feed, which de-duplication
absorbs but takes time.

## Checklist before going live

- [ ] `tsp_live_` key in the production `.env` only, and a separate
      `tsp_test_` key for staging.
- [ ] Webhook URL given to Talksasa staff; signing secret in `.env`.
- [ ] Webhook route registered with `Route::talksasaPayWebhook(...)`; returns `200` for a test event.
- [ ] Listeners are queued and re-read the payment request.
- [ ] A queue worker is running.
- [ ] `talksasa-pay:poll-events` is scheduled, with the cursor in the
      database.
- [ ] Shared cache store for de-duplication.
- [ ] Your production servers' outbound IPs sent to staff for the key's
      allow-list.
- [ ] You handle `payment_request.reopened` (reversals) as well as `paid`.
- [ ] If you send M-Pesa prompts: the invoice is marked paid only on
      `payment_request.paid`; `stk_push.failed` shows the reason with a
      retry and the paybill; tested end to end on staging (Daraja sandbox).
