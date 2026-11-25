# Laravel integration

The PHP SDK `talksasa/pay-php` (source in [`clients/php`](../../clients/php))
wraps the app API for any PHP 8.2+ app and ships a Laravel bridge: a config
file, a facade, a webhook endpoint that verifies signatures, Laravel events
for each Talksasa Pay event, and an Artisan command that polls the events
feed.

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

The method names below are the SDK's; the exact signatures, models and
options are in the [`clients/php` README](../../clients/php/README.md).

```php
use TalksasaPay;   // facade

// When an invoice is issued: one charge per invoice.
$charge = TalksasaPay::createCharge(
    reference: $invoice->number,                    // unique for ever within your app
    amount: $invoice->total_kes,                    // integer KES, 1 to 250,000
    description: "Cloud Hosting, {$invoice->period}",
    metadata: ['invoice_id' => $invoice->id],
    idempotencyKey: "invoice-{$invoice->id}",       // stable: safe to retry
);
$invoice->update([
    'talksasa_charge_id' => $charge->id,
    'checkout_url' => $charge->checkoutUrl,
    'mpesa_account' => $charge->accountNumber,
]);

return redirect()->away($charge->checkoutUrl);
```

Other calls: `getCharge($id)`, `listCharges(...)` (filter by `reference` or
`status`), `stkPush($chargeId, $phone)`, `cancelCharge($id)`, transactions
and the events feed.

Always pass `idempotencyKey` derived from your own record. Without it the
SDK makes up a random key, which protects only the retries inside that one
call, not a retry of your whole job after a crash.

The SDK throws a specific exception for each error `code` (all extend
`Talksasa\Pay\Exception\TalksasaPayException`), for example
`DuplicateReferenceException`, `IdempotencyConflictException`,
`NotCancellableException`, `NotPayableException`, `StkInProgressException`,
`ValidationException` (with the field errors) and `RateLimitedException`.
See [errors](errors.md) for what each code means.

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
| `charge.succeeded` | `ChargeSucceeded` |
| `charge.needs_review` | `ChargeNeedsReview` |
| `charge.expired` | `ChargeExpired` |
| `charge.cancelled` | `ChargeCancelled` |
| `stk_push.sent` | `StkPushSent` |
| `stk_push.succeeded` | `StkPushSucceeded` |
| `stk_push.failed` | `StkPushFailed` |
| `stk_push.needs_review` | `StkPushNeedsReview` |
| `payment.received` | `PaymentReceived` |
| `transaction.reversed` | `TransactionReversed` |
| every event, including types this SDK version doesn't know yet | `WebhookReceived` (dispatched after the specific one) |

Every event carries the envelope and where it came from (webhook or the
events poller), plus the resource: the charge for `Charge*` events, the
prompt for `StkPush*` events and the transaction for `PaymentReceived` and
`TransactionReversed`. The property names are in the `clients/php` README.

Use a queued listener so the webhook is answered within 10 seconds, and
re-read the charge before you change an invoice:

```php
namespace App\Listeners;

use App\Models\Invoice;
use Illuminate\Contracts\Queue\ShouldQueue;
use Talksasa\Pay\Laravel\Events\ChargeSucceeded;
use TalksasaPay;

class MarkInvoicePaid implements ShouldQueue
{
    public function handle(ChargeSucceeded $e): void
    {
        $invoice = Invoice::where('talksasa_charge_id', $e->charge->id)->first();
        if ($invoice === null) {
            return; // not ours, or created outside this app
        }
        // Events can arrive late or out of order: act on the current state.
        $current = TalksasaPay::getCharge($e->charge->id);
        if ($current->status === 'succeeded' && $invoice->status !== 'paid') {
            $invoice->update(['status' => 'paid', 'paid_at' => $current->succeededAt]);
        }
    }
}
```

Register it the usual way (Laravel discovers listeners by type-hint, or add
it in `EventServiceProvider`). Keep listeners idempotent: setting a status
is safe to repeat, adding to a balance is not.

Also listen for:

- `PaymentReceived`: money that paid no charge (a paybill payment with the
  wrong amount, or a customer who paid twice). Decide what it pays, keyed
  on the transaction id so a repeat never applies it twice. See
  [paybill payments](paybill.md).
- `TransactionReversed`: staff reversed a payment, and its charge is now
  `reversed`. Put the invoice back to unpaid or suspend the service, as
  your business requires.
- `ChargeSucceeded` for a charge you **cancelled**: late money is never
  dropped, so decide whether to honour or refund it.

## Pay now button

Send the M-Pesa prompt straight to the customer's phone from your own
"Pay now" button. The full flow, statuses and error codes are in
[M-Pesa payment prompts](stk-push.md).

**The rule: the invoice becomes paid only in the `ChargeSucceeded`
listener.** `StkPushSent`, `StkPushNeedsReview` and `ChargeNeedsReview` are
never payment.

```php
namespace App\Http\Controllers;

use App\Models\Invoice;
use Illuminate\Http\Request;
use Talksasa\Pay\Exception\{MpesaErrorException, MpesaUnavailableException,
    NotPayableException, StkInProgressException, ValidationException};
use TalksasaPay;

class PayInvoiceController
{
    public function __invoke(Request $request, Invoice $invoice)
    {
        $phone = $request->validate(['phone' => ['required', 'string', 'max:20']])['phone'];

        try {
            if ($invoice->talksasa_charge_id === null) {
                // First click: create the charge and prompt in one call. The
                // idempotency key makes a second click or a retry safe.
                $charge = TalksasaPay::createCharge(
                    reference: $invoice->number,
                    amount: $invoice->total_kes,
                    description: "Invoice {$invoice->number}",
                    phone: $phone,
                    idempotencyKey: "invoice-{$invoice->id}",
                );
                $invoice->update(['talksasa_charge_id' => $charge->id, 'stk_message' => null]);
            } else {
                // Try again after a failed prompt. The SDK makes a fresh key
                // per call and reuses it on its own retries, so one click
                // never sends two prompts.
                $invoice->update(['stk_message' => null]);
                TalksasaPay::stkPush($invoice->talksasa_charge_id, $phone);
            }
        } catch (StkInProgressException) {
            // Already on their phone: same screen.
        } catch (ValidationException) {
            return back()->withErrors(['phone' => 'Enter a Safaricom number, e.g. 0712 345 678.']);
        } catch (NotPayableException | MpesaUnavailableException | MpesaErrorException) {
            $charge = TalksasaPay::getCharge($invoice->talksasa_charge_id);
            return view('invoices.paybill', ['charge' => $charge]);   // pay by paybill instead
        }

        return view('invoices.check-phone', ['invoice' => $invoice]);
        // "Check your phone and enter your M-Pesa PIN." Poll your own
        // endpoint for $invoice->status and $invoice->stk_message.
    }
}
```

With a phone, `createCharge` still returns the charge when the first
prompt could not be sent (its latest prompt is then `failed`, or absent if
M-Pesa is off), so the customer can try again or use the paybill.

```php
use Talksasa\Pay\Laravel\Events\{ChargeSucceeded, StkPushFailed};

// The ONLY place an invoice becomes paid.
Event::listen(function (ChargeSucceeded $e) {
    Invoice::where('talksasa_charge_id', $e->charge->id)
        ->where('status', '!=', 'paid')
        ->update(['status' => 'paid', 'paid_at' => $e->charge->succeededAt]);
});

// Cancelled, timed out, insufficient funds, wrong PIN...: show why, offer a
// retry. The charge is still pending and can be prompted again.
Event::listen(function (StkPushFailed $e) {
    Invoice::where('talksasa_charge_id', $e->stkPush->chargeId)
        ->where('status', '!=', 'paid')
        ->update(['stk_message' => $e->stkPush->resultDesc ?? 'The payment prompt failed.']);
});
```

On the "check your phone" screen, when `stk_message` is set, show it with a
**Try again** button (posting to the same controller) and the paybill
steps: the business number and account number from the charge's `paybill`.
Optionally show "We're confirming your payment with Safaricom" on
`ChargeNeedsReview`, without changing the invoice.

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
- [ ] Webhook route registered with `Route::talksasaPayWebhook(...)`;
      returns `200` for a test event.
- [ ] Listeners are queued and re-read the charge.
- [ ] A queue worker is running.
- [ ] `talksasa-pay:poll-events` is scheduled, with the cursor in the
      database.
- [ ] Shared cache store for de-duplication.
- [ ] Your production servers' outbound IPs sent to staff for the key's
      allow-list.
- [ ] The invoice is marked paid only on `charge.succeeded`.
- [ ] You handle `payment.received` (money that paid no charge),
      `transaction.reversed`, and a charge succeeding after you cancelled
      it.
- [ ] If you send M-Pesa prompts: `stk_push.failed` shows the reason with
      a retry and the paybill; tested end to end on staging (Daraja
      sandbox).
