<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Laravel;

use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Event as EventFacade;
use Talksasa\Pay\Laravel\Events\PaymentRequestPaid;
use Talksasa\Pay\Laravel\Events\TalksasaPayEvent;
use Talksasa\Pay\Laravel\Events\WebhookReceived;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Webhook\Verifier;

final class PollEventsCommandTest extends LaravelTestCase
{
    public function testPollsDispatchesAndStoresCursorInCache(): void
    {
        EventFacade::fake([PaymentRequestPaid::class, WebhookReceived::class]);
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1'), Fixtures::event(id: 'e2')], 'next_cursor' => 'c2', 'has_more' => true]);
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e3')], 'next_cursor' => 'c3', 'has_more' => false]);

        $this->artisan('talksasa-pay:poll-events')
            ->expectsOutputToContain('Read 3 event(s); dispatched 3 new, skipped 0')
            ->assertSuccessful();

        EventFacade::assertDispatchedTimes(PaymentRequestPaid::class, 3);
        EventFacade::assertDispatched(WebhookReceived::class, fn (WebhookReceived $e) => $e->source === TalksasaPayEvent::SOURCE_POLL);
        self::assertSame('c3', Cache::get('talksasa-pay:events-cursor'));
        self::assertSame('limit=100', $this->http->requests[0]->getUri()->getQuery());

        // Next run resumes from the cursor.
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null, 'has_more' => false]);
        $this->artisan('talksasa-pay:poll-events')->assertSuccessful();
        self::assertSame('after=c3&limit=100', $this->http->last()->getUri()->getQuery());
        self::assertSame('c3', Cache::get('talksasa-pay:events-cursor'));
    }

    public function testEventAlreadyDeliveredByWebhookIsNotDispatchedAgain(): void
    {
        EventFacade::fake([PaymentRequestPaid::class, WebhookReceived::class]);
        $body = json_encode(Fixtures::event(id: 'e1'), JSON_THROW_ON_ERROR);
        $this->call('POST', '/webhooks/talksasa-pay', [], [], [], [
            'CONTENT_TYPE' => 'application/json',
            'HTTP_TALKSASA_SIGNATURE' => Verifier::sign($body, self::SECRET),
        ], $body)->assertOk();

        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1'), Fixtures::event(id: 'e2')], 'next_cursor' => 'c2', 'has_more' => false]);
        $this->artisan('talksasa-pay:poll-events')
            ->expectsOutputToContain('dispatched 1 new, skipped 1 already handled')
            ->assertSuccessful();

        EventFacade::assertDispatchedTimes(PaymentRequestPaid::class, 2);
    }

    public function testFailureKeepsCursorAndExitsNonZero(): void
    {
        $this->http->pushJson(401, ['status' => 401, 'code' => 'unauthorized', 'title' => 'Unauthorized', 'type' => 'about:blank']);
        Cache::forever('talksasa-pay:events-cursor', 'c7');

        $this->artisan('talksasa-pay:poll-events')
            ->expectsOutputToContain('AuthenticationException')
            ->assertFailed();
        self::assertSame('c7', Cache::get('talksasa-pay:events-cursor'));
    }

    public function testMaxPagesAndPageSizeOptions(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1')], 'next_cursor' => 'c1', 'has_more' => true]);
        $this->artisan('talksasa-pay:poll-events', ['--max-pages' => 1, '--page-size' => 10])->assertSuccessful();
        self::assertCount(1, $this->http->requests);
        self::assertSame('limit=10', $this->http->last()->getUri()->getQuery());
    }

    public function testSkipsWhenAnotherRunHoldsTheLock(): void
    {
        $lock = Cache::lock('talksasa-pay:poll-events', 600);
        self::assertTrue($lock->get());
        $this->artisan('talksasa-pay:poll-events')->expectsOutputToContain('skipping')->assertSuccessful();
        self::assertCount(0, $this->http->requests);
        $lock->release();
    }

    public function testDatabaseCursorStore(): void
    {
        config()->set('database.default', 'testing');
        config()->set('talksasa-pay.poller.cursor_store', 'database');
        $this->app->forgetInstance(\Talksasa\Pay\Events\CursorStore::class);
        $migration = require __DIR__ . '/../../database/migrations/2026_11_24_000000_create_talksasa_pay_cursors_table.php';
        $migration->up();

        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1')], 'next_cursor' => 'c1', 'has_more' => false]);
        $this->artisan('talksasa-pay:poll-events')->assertSuccessful();
        self::assertSame('c1', DB::table('talksasa_pay_cursors')->where('name', 'events')->value('cursor'));

        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e2')], 'next_cursor' => 'c2', 'has_more' => false]);
        $this->artisan('talksasa-pay:poll-events')->assertSuccessful();
        self::assertSame('after=c1&limit=100', $this->http->last()->getUri()->getQuery());
        self::assertSame(1, DB::table('talksasa_pay_cursors')->count());
        self::assertSame('c2', DB::table('talksasa_pay_cursors')->value('cursor'));
    }
}
