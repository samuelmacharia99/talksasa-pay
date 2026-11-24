<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use Talksasa\Pay\Events\CallbackCursorStore;
use Talksasa\Pay\Events\EventPoller;
use Talksasa\Pay\Events\InMemoryCursorStore;
use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Tests\Support\Fixtures;

final class EventPollerTest extends ClientTestCase
{
    public function testDrainsFeedAndSavesCursorPerPage(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1'), Fixtures::event(id: 'e2')], 'next_cursor' => 'c2', 'has_more' => true]);
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e3')], 'next_cursor' => 'c3', 'has_more' => false]);

        $saved = [];
        $store = new CallbackCursorStore(static fn () => 'c0', function (string $c) use (&$saved): void {
            $saved[] = $c;
        });
        $ids = [];
        $n = (new EventPoller($this->client(), $store, 2))->poll(function (Event $e) use (&$ids): void {
            $ids[] = $e->id;
        });

        self::assertSame(3, $n);
        self::assertSame(['e1', 'e2', 'e3'], $ids);
        self::assertSame(['c2', 'c3'], $saved);
        self::assertSame('after=c0&limit=2', $this->http->requests[0]->getUri()->getQuery());
        self::assertSame('after=c2&limit=2', $this->http->requests[1]->getUri()->getQuery());
    }

    public function testStartsFromBeginningWithoutCursor(): void
    {
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null, 'has_more' => false]);
        $store = new InMemoryCursorStore();
        $n = (new EventPoller($this->client(), $store))->poll(static fn () => null);

        self::assertSame(0, $n);
        self::assertSame('limit=100', $this->http->last()->getUri()->getQuery());
        self::assertNull($store->get(), 'empty page keeps the previous (null) cursor');
    }

    public function testEmptyPageKeepsCursor(): void
    {
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null, 'has_more' => false]);
        $store = new InMemoryCursorStore('c9');
        (new EventPoller($this->client(), $store))->poll(static fn () => null);
        self::assertSame('c9', $store->get());
    }

    public function testHandlerFailureDoesNotAdvanceCursor(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1')], 'next_cursor' => 'c1', 'has_more' => true]);
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e2'), Fixtures::event(id: 'e3')], 'next_cursor' => 'c3', 'has_more' => false]);
        $store = new InMemoryCursorStore();

        try {
            (new EventPoller($this->client(), $store))->poll(static function (Event $e): void {
                if ($e->id === 'e3') {
                    throw new \RuntimeException('listener failed');
                }
            });
            self::fail('expected exception');
        } catch (\RuntimeException $e) {
            self::assertSame('listener failed', $e->getMessage());
        }
        self::assertSame('c1', $store->get(), 'page 2 will be re-read next run');
    }

    public function testMaxPages(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1')], 'next_cursor' => 'c1', 'has_more' => true]);
        $store = new InMemoryCursorStore();
        $n = (new EventPoller($this->client(), $store))->poll(static fn () => null, maxPages: 1);
        self::assertSame(1, $n);
        self::assertSame('c1', $store->get());
        self::assertCount(1, $this->http->requests);
    }

    public function testRejectsBadPageSize(): void
    {
        $this->expectException(\InvalidArgumentException::class);
        new EventPoller($this->client(), new InMemoryCursorStore(), 201);
    }
}
