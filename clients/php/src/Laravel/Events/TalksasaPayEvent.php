<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Event;

/**
 * Base of every Laravel event this package dispatches. Each Talksasa Pay
 * event is dispatched once (de-duplicated by id across webhooks and the
 * poller), first as its specific class, then as WebhookReceived.
 *
 * Listen with a queued listener (implements ShouldQueue) for anything slow:
 * the webhook must be answered within 10 s.
 */
abstract class TalksasaPayEvent
{
    public const SOURCE_WEBHOOK = 'webhook';
    public const SOURCE_POLL = 'poll';

    public function __construct(
        public readonly Event $event,
        /** "webhook" or "poll" (the event feed). */
        public readonly string $source,
    ) {
    }
}
