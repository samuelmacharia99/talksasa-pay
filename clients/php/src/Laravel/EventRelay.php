<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel;

use Illuminate\Contracts\Cache\Repository as Cache;
use Illuminate\Contracts\Events\Dispatcher;
use Talksasa\Pay\Laravel\Events;
use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\EventType;

/**
 * Turns Talksasa Pay events into Laravel events, once per event id. Shared
 * by the webhook controller and the poll command so a listener cannot tell
 * (and need not care) which delivered an event first.
 *
 * At least once: the de-duplication key is released if dispatching throws,
 * so the event is retried (webhook: Talksasa redelivers after the 500;
 * poller: the cursor is not advanced).
 */
class EventRelay
{
    /** Event type => the Laravel event dispatched for it (then WebhookReceived). */
    public const CLASSES = [
        EventType::CHARGE_SUCCEEDED => Events\ChargeSucceeded::class,
        EventType::CHARGE_NEEDS_REVIEW => Events\ChargeNeedsReview::class,
        EventType::CHARGE_EXPIRED => Events\ChargeExpired::class,
        EventType::CHARGE_CANCELLED => Events\ChargeCancelled::class,
        EventType::STK_PUSH_SENT => Events\StkPushSent::class,
        EventType::STK_PUSH_FAILED => Events\StkPushFailed::class,
        EventType::STK_PUSH_SUCCEEDED => Events\StkPushSucceeded::class,
        EventType::STK_PUSH_NEEDS_REVIEW => Events\StkPushNeedsReview::class,
        EventType::PAYMENT_RECEIVED => Events\PaymentReceived::class,
        EventType::TRANSACTION_REVERSED => Events\TransactionReversed::class,
    ];

    public function __construct(
        private readonly Cache $cache,
        private readonly Dispatcher $events,
        private readonly int $ttl,
        private readonly string $prefix = 'talksasa-pay:event:',
    ) {
    }

    /**
     * @param string $source TalksasaPayEvent::SOURCE_WEBHOOK or SOURCE_POLL
     * @return bool false when the event was already handled (duplicate)
     */
    public function relay(Event $event, string $source): bool
    {
        $key = $this->prefix . $event->id;
        if (!$this->cache->add($key, $source, $this->ttl)) {
            return false;
        }

        try {
            $class = self::CLASSES[$event->type] ?? null;
            if ($class !== null) {
                $this->events->dispatch(new $class($event, $source));
            }
            $this->events->dispatch(new Events\WebhookReceived($event, $source));
        } catch (\Throwable $e) {
            $this->cache->forget($key);
            throw $e;
        }

        return true;
    }
}
