<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Laravel;

use Illuminate\Support\Facades\Event as EventFacade;
use Illuminate\Testing\TestResponse;
use Talksasa\Pay\Laravel\Events\PayerCreditChanged;
use Talksasa\Pay\Laravel\Events\PaymentRequestPaid;
use Talksasa\Pay\Laravel\Events\PaymentRequestPartiallyPaid;
use Talksasa\Pay\Laravel\Events\TalksasaPayEvent;
use Talksasa\Pay\Laravel\Events\TransactionAllocated;
use Talksasa\Pay\Laravel\Events\WebhookReceived;
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

    private static function body(string $type = 'payment_request.paid', string $id = 'evt-1'): string
    {
        return json_encode(Fixtures::event($type, id: $id), JSON_THROW_ON_ERROR);
    }

    public function testValidWebhookDispatchesOnceEvenIfDeliveredTwice(): void
    {
        EventFacade::fake([PaymentRequestPaid::class, WebhookReceived::class]);
        $body = self::body();
        $sig = Verifier::sign($body, self::SECRET);

        $this->deliver($body, $sig, 'evt-1')->assertOk()->assertExactJson(['received' => true, 'duplicate' => false]);
        $this->deliver($body, $sig, 'evt-1')->assertOk()->assertExactJson(['received' => true, 'duplicate' => true]);

        EventFacade::assertDispatchedTimes(PaymentRequestPaid::class, 1);
        EventFacade::assertDispatchedTimes(WebhookReceived::class, 1);
        EventFacade::assertDispatched(PaymentRequestPaid::class, function (PaymentRequestPaid $e): bool {
            return $e->source === TalksasaPayEvent::SOURCE_WEBHOOK
                && $e->paymentRequest->reference === 'INV-1042'
                && $e->paymentRequest->amountPaid === 1500
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
        EventFacade::fake([PaymentRequestPartiallyPaid::class, TransactionAllocated::class, PayerCreditChanged::class, WebhookReceived::class]);
        foreach (['payment_request.partially_paid' => 'a', 'transaction.allocated' => 'b', 'payer.credit_changed' => 'c', 'future.type' => 'd'] as $type => $id) {
            $body = $type === 'future.type'
                ? json_encode(Fixtures::event($type, ['x' => 1], $id), JSON_THROW_ON_ERROR)
                : self::body($type, $id);
            $this->deliver($body, Verifier::sign($body, self::SECRET))->assertOk();
        }
        EventFacade::assertDispatchedTimes(PaymentRequestPartiallyPaid::class, 1);
        EventFacade::assertDispatched(TransactionAllocated::class, fn (TransactionAllocated $e) => $e->transaction->amount === 2000);
        EventFacade::assertDispatched(PayerCreditChanged::class, fn (PayerCreditChanged $e) => $e->creditChange->creditBalance === 900);
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
        EventFacade::listen(PaymentRequestPaid::class, function () use (&$calls): void {
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
