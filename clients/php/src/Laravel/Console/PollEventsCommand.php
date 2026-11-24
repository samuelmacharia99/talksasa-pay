<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Console;

use Illuminate\Cache\Repository as CacheRepository;
use Illuminate\Console\Command;
use Illuminate\Contracts\Cache\Factory as CacheFactory;
use Illuminate\Contracts\Cache\LockProvider;
use Talksasa\Pay\Client;
use Talksasa\Pay\Events\CursorStore;
use Talksasa\Pay\Events\EventPoller;
use Talksasa\Pay\Laravel\EventRelay;
use Talksasa\Pay\Laravel\Events\TalksasaPayEvent;
use Talksasa\Pay\Model\Event;

/**
 * Reconcile-by-feed: reads GET /v1/events from the stored cursor and
 * dispatches the same Laravel events as the webhook (de-duplicated by event
 * id), so anything a webhook missed is still handled. Schedule it:
 *
 *     Schedule::command('talksasa-pay:poll-events')->everyFiveMinutes()->withoutOverlapping();
 */
final class PollEventsCommand extends Command
{
    protected $signature = 'talksasa-pay:poll-events
        {--max-pages= : Stop after this many pages (default: drain the feed)}
        {--page-size= : Events per page, 1-200 (default from config)}';

    protected $description = 'Read the Talksasa Pay event feed and dispatch any events not yet handled (backstop to webhooks)';

    public function handle(Client $client, CursorStore $cursor, EventRelay $relay, CacheFactory $cache): int
    {
        $pageSize = (int) ($this->option('page-size') ?: config('talksasa-pay.poller.page_size', 100));
        $maxPages = $this->option('max-pages') !== null ? max(1, (int) $this->option('max-pages')) : null;

        $store = $cache->store();
        $lock = $store instanceof CacheRepository && $store->getStore() instanceof LockProvider
            ? $store->getStore()->lock('talksasa-pay:poll-events', 600)
            : null;
        if ($lock !== null && !$lock->get()) {
            $this->warn('Another talksasa-pay:poll-events is running; skipping.');
            return self::SUCCESS;
        }

        $new = 0;
        try {
            $poller = new EventPoller($client, $cursor, $pageSize);
            $seen = $poller->poll(function (Event $event) use ($relay, &$new): void {
                if ($relay->relay($event, TalksasaPayEvent::SOURCE_POLL)) {
                    $new++;
                }
            }, $maxPages);
        } catch (\Throwable $e) {
            report($e);
            $this->error(sprintf('Polling failed: %s: %s', $e::class, $e->getMessage()));
            return self::FAILURE;
        } finally {
            $lock?->release();
        }

        $this->info(sprintf('Read %d event(s); dispatched %d new, skipped %d already handled.', $seen, $new, $seen - $new));
        return self::SUCCESS;
    }
}
