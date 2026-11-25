<?php

declare(strict_types=1);

namespace Talksasa\Pay\Events;

use Talksasa\Pay\Client;
use Talksasa\Pay\Model\Event;

/**
 * Reads GET /v1/events from the stored cursor, hands each event to a
 * handler, and saves the cursor after each fully handled page. Run it from
 * cron or a scheduled queue job as the backstop to webhooks (or instead of
 * them): the feed is gap-free, so nothing is missed even if a webhook was.
 *
 * Delivery is at least once: if the handler throws, the cursor of the
 * current page is not saved and the exception propagates, so the page is
 * read again next run. Handlers must therefore be idempotent (de-duplicate
 * by $event->id, or apply state changes that are safe to repeat).
 *
 * Do not run two pollers for the same app at once (use a lock, e.g.
 * Laravel's withoutOverlapping()).
 */
final class EventPoller
{
    public function __construct(
        private readonly Client $client,
        private readonly CursorStore $cursor,
        private readonly int $pageSize = 100,
    ) {
        if ($pageSize < 1 || $pageSize > 200) {
            throw new \InvalidArgumentException('Page size must be between 1 and 200.');
        }
    }

    /**
     * Drains the feed (or stops after $maxPages pages).
     *
     * @param callable(Event): void $handler
     * @return int number of events handled
     */
    public function poll(callable $handler, ?int $maxPages = null): int
    {
        $handled = 0;
        $pages = 0;
        $after = $this->cursor->get();

        while ($maxPages === null || $pages < $maxPages) {
            $page = $this->client->listEvents($after, $this->pageSize);
            $pages++;
            foreach ($page->data as $event) {
                $handler($event);
                $handled++;
            }
            if ($page->nextCursor !== null && $page->nextCursor !== $after) {
                $this->cursor->set($page->nextCursor);
                $after = $page->nextCursor;
            }
            if (!$page->hasMore || $page->data === []) {
                break;
            }
        }

        return $handled;
    }
}
