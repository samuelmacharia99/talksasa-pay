<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Laravel;

use Illuminate\Support\Facades\Event as EventFacade;
use Illuminate\Testing\TestResponse;
use Talksasa\Pay\Laravel\EventRelay;
use Talksasa\Pay\Laravel\Events\ChargeCancelled;
use Talksasa\Pay\Laravel\Events\ChargeExpired;
use Talksasa\Pay\Laravel\Events\ChargeNeedsReview;
use Talksasa\Pay\Laravel\Events\ChargeSucceeded;
use Talksasa\Pay\Laravel\Events\PaymentReceived;
use Talksasa\Pay\Laravel\Events\StkPushEvent;
use Talksasa\Pay\Laravel\Events\StkPushFailed;
use Talksasa\Pay\Laravel\Events\StkPushNeedsReview;
use Talksasa\Pay\Laravel\Events\StkPushSent;
use Talksasa\Pay\Laravel\Events\StkPushSucceeded;
use Talksasa\Pay\Laravel\Events\TalksasaPayEvent;
use Talksasa\Pay\Laravel\Events\TransactionReversed;
use Talksasa\Pay\Laravel\Events\WebhookReceived;
use Talksasa\Pay\Model\EventType;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Webhook\Verifier;

final class WebhookTest extends LaravelTestCase
{
    /** Posts the exact raw body with the given signature header. */
    private function deliver(string $body, ?string $signature, ?string $eventId = null): TestResponse
    {
        $server = ['CONTENT_TYPE' => 'application/json', 'HTTP_ACCEPT' => 'application/json'];
        if ($signature !== null) {
            $server['HTTP_TALKSASA_SIGNATURE'] = $signature;
        }
        if ($eventId !== null) {
            $server['HTTP_TALKSASA_EVENT_ID'] = $eventId;
        }
        return $this->call('POST', '/webhooks/talksasa-pay', [], [], [], $server, $body);
    }

    private static function body(string $type = 'charge.succeeded', string $id = 'evt-1'): string
    {
        return json_encode(Fixtures::event($type, id: $id), JSON_THROW_ON_ERROR);
    }

    public function testValidWebhookDispatchesOnceEvenIfDeliveredTwice(): void
    {
        EventFacade::fake([ChargeSucceeded::class, WebhookReceived::class]);
        $body = self::body();
        $sig = Verifier::sign($body, self::SECRET);

        $this->deliver($body, $sig, 'evt-1')->assertOk()->assertExactJson(['received' => true, 'duplicate' => false]);
        $this->deliver($body, $sig, 'evt-1')->assertOk()->assertExactJson(['received' => true, 'duplicate' => true]);

        EventFacade::assertDispatchedTimes(ChargeSucceeded::class, 1);
        EventFacade::assertDispatchedTimes(WebhookReceived::class, 1);
        EventFacade::assertDispatched(ChargeSucceeded::class, function (ChargeSucceeded $e): bool {
            return $e->source === TalksasaPayEvent::SOURCE_WEBHOOK
                && $e->charge->reference === 'INV-1042'
                && $e->charge->isSucceeded()
                && $e->event->id === 'evt-1';
        });
    }

    public function testPreviousSecretAcceptedDuringRotation(): void
    {
        EventFacade::fake([WebhookReceived::class]);
        $body = self::body();
        $this->deliver($body, Verifier::sign($body, self::OLD_SECRET))->assertOk();
        EventFacade::assertDispatchedTimes(WebhookReceived::class, 1);
    }

    public function testEachTypeMapsToItsEvent(): void
    {
        $classes = array_values(EventRelay::CLASSES);
        EventFacade::fake([...$classes, WebhookReceived::class]);
        $n = 0;
        foreach (EventType::ALL as $type) {
            $body = self::body($type, 'evt-' . ++$n);
            $this->deliver($body, Verifier::sign($body, self::SECRET))->assertOk();
        }
        foreach ($classes as $class) {
            EventFacade::assertDispatchedTimes($class, 1);
        }
        EventFacade::assertDispatched(ChargeCancelled::class, fn (ChargeCancelled $e) => $e->charge->status === 'cancelled');
        EventFacade::assertDispatched(ChargeExpired::class, fn (ChargeExpired $e) => $e->charge->status === 'expired');
        EventFacade::assertDispatched(ChargeNeedsReview::class, fn (ChargeNeedsReview $e) => $e->charge->needsReview());
        EventFacade::assertDispatched(PaymentReceived::class, fn (PaymentReceived $e) => $e->payment->accountNumber === 'HOST-1042'
            && $e->payment->amount === 700 && !$e->payment->isDuplicate());
        EventFacade::assertDispatched(TransactionReversed::class, fn (TransactionReversed $e) => $e->transaction->isReversal()
            && $e->transaction->reversesTransactionId === Fixtures::TXN_ID && $e->transaction->chargeId === Fixtures::CHARGE_ID);
        EventFacade::assertDispatchedTimes(WebhookReceived::class, count(EventType::ALL));
    }

    public function testUnknownTypeDispatchesOnlyWebhookReceived(): void
    {
        EventFacade::fake([...array_values(EventRelay::CLASSES), WebhookReceived::class]);
        $body = json_encode(Fixtures::event('future.type', ['x' => 1], 'evt-future'), JSON_THROW_ON_ERROR);
        $this->deliver($body, Verifier::sign($body, self::SECRET))->assertOk();
        foreach (EventRelay::CLASSES as $class) {
            EventFacade::assertNotDispatched($class);
        }
        EventFacade::assertDispatched(WebhookReceived::class, fn (WebhookReceived $e) => $e->event->type === 'future.type'
            && $e->event->data === null && $e->event->rawData === ['x' => 1]);
    }

    public function testStkPushEventsMapToTheirEventsAndNeverToPaid(): void
    {
        EventFacade::fake([StkPushSent::class, StkPushSucceeded::class, StkPushFailed::class, StkPushNeedsReview::class, ChargeSucceeded::class, WebhookReceived::class]);
        $failed = Fixtures::stkPush(['status' => 'failed', 'result_code' => '1032', 'result_desc' => 'Request cancelled by user']);
        $bodies = [
            json_encode(Fixtures::event('stk_push.sent', id: 's1'), JSON_THROW_ON_ERROR),
            json_encode(Fixtures::event('stk_push.needs_review', Fixtures::stkPush(['status' => 'needs_review']), 's2'), JSON_THROW_ON_ERROR),
            json_encode(Fixtures::event('stk_push.failed', $failed, 's3'), JSON_THROW_ON_ERROR),
            json_encode(Fixtures::event('stk_push.succeeded', Fixtures::stkPush(['status' => 'succeeded']), 's4'), JSON_THROW_ON_ERROR),
        ];
        foreach ($bodies as $body) {
            $this->deliver($body, Verifier::sign($body, self::SECRET))->assertOk();
        }

        EventFacade::assertDispatchedTimes(StkPushSent::class, 1);
        EventFacade::assertDispatchedTimes(StkPushNeedsReview::class, 1);
        EventFacade::assertDispatchedTimes(StkPushSucceeded::class, 1);
        EventFacade::assertDispatched(StkPushFailed::class, fn (StkPushFailed $e): bool => $e instanceof StkPushEvent
            && $e->stkPush->resultDesc === 'Request cancelled by user'
            && $e->stkPush->wasCancelledByCustomer());
        // A prompt's fate is never the bill's: only charge.succeeded is.
        EventFacade::assertNotDispatched(ChargeSucceeded::class);
        EventFacade::assertDispatchedTimes(WebhookReceived::class, 4);
    }

    public function testInvalidSignatureIs400(): void
    {
        EventFacade::fake([WebhookReceived::class]);
        $body = self::body();
        $this->deliver($body, Verifier::sign($body, 'whsec_attacker'))
            ->assertStatus(400)->assertExactJson(['error' => 'no_matching_signature']);
        EventFacade::assertNotDispatched(WebhookReceived::class);
    }

    public function testTamperedBodyIs400(): void
    {
        EventFacade::fake([WebhookReceived::class]);
        $body = self::body();
        $sig = Verifier::sign($body, self::SECRET);
        $this->deliver(str_replace('"amount":1500', '"amount":1', $body), $sig)->assertStatus(400);
        EventFacade::assertNotDispatched(WebhookReceived::class);
    }

    public function testStaleTimestampIs400(): void
    {
        EventFacade::fake([WebhookReceived::class]);
        $body = self::body();
        $this->deliver($body, Verifier::sign($body, self::SECRET, time() - 301))
            ->assertStatus(400)->assertExactJson(['error' => 'timestamp_out_of_tolerance']);
        EventFacade::assertNotDispatched(WebhookReceived::class);
    }

    public function testMissingAndMalformedHeaderAre400(): void
    {
        $this->deliver(self::body(), null)->assertStatus(400)->assertExactJson(['error' => 'missing_header']);
        $this->deliver(self::body(), 't=abc')->assertStatus(400)->assertExactJson(['error' => 'malformed_header']);
    }

    public function testSignedGarbageIs400(): void
    {
        $body = '{"hello":"world"}';
        $this->deliver($body, Verifier::sign($body, self::SECRET))->assertStatus(400)->assertExactJson(['error' => 'invalid_payload']);
    }

    public function testListenerFailureReturns500AndRedeliveryIsProcessed(): void
    {
        $this->withoutExceptionHandling();
        $calls = 0;
        EventFacade::listen(ChargeSucceeded::class, function () use (&$calls): void {
            $calls++;
            if ($calls === 1) {
                throw new \RuntimeException('database down');
            }
        });
        $body = self::body();
        $sig = Verifier::sign($body, self::SECRET);

        try {
            $this->deliver($body, $sig);
            self::fail('expected the listener exception');
        } catch (\RuntimeException $e) {
            self::assertSame('database down', $e->getMessage());
        }
        $this->deliver($body, $sig)->assertOk()->assertJson(['duplicate' => false]);
        $this->deliver($body, $sig)->assertOk()->assertJson(['duplicate' => true]);
        self::assertSame(2, $calls);
    }

    public function testRouteIsNamedAndOnlyAcceptsPost(): void
    {
        self::assertSame(url('/webhooks/talksasa-pay'), route('talksasa-pay.webhook'));
        $this->get('/webhooks/talksasa-pay')->assertStatus(405);
    }

    public function testCsrfProtectionIsRemovedFromTheWebhookRoute(): void
    {
        $this->app->make(\Illuminate\Contracts\Http\Kernel::class); // syncs middleware groups to the router
        $router = $this->app['router'];
        $route = $router->getRoutes()->getByName('talksasa-pay.webhook');
        self::assertNotNull($route);
        $middleware = $router->gatherRouteMiddleware($route);

        self::assertContains(\Talksasa\Pay\Laravel\Http\VerifyWebhookSignature::class, $middleware);
        foreach ($middleware as $m) {
            $class = is_string($m) ? explode(':', $m, 2)[0] : '';
            self::assertFalse(
                str_contains($class, 'Csrf') || str_contains($class, 'RequestForgery'),
                "CSRF middleware {$class} still applies to the webhook route",
            );
        }
        // And the web group does contain CSRF protection, so the check above is meaningful.
        $probe = $router->getRoutes()->getByName('csrf-probe');
        self::assertNotNull($probe);
        self::assertNotEmpty(array_filter(
            $router->gatherRouteMiddleware($probe),
            static fn ($m) => is_string($m) && (str_contains($m, 'Csrf') || str_contains($m, 'RequestForgery')),
        ));
    }

    public function testMissingSecretConfigIs500(): void
    {
        config()->set('talksasa-pay.webhook_secrets', []);
        $body = self::body();
        $this->deliver($body, Verifier::sign($body, self::SECRET))->assertStatus(500);
    }
}
