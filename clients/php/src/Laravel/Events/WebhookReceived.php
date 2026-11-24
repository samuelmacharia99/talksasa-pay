<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/**
 * Dispatched for EVERY Talksasa Pay event, whether it arrived by webhook or
 * from the event feed ($source), including types this package version does
 * not know yet (then $event->data is null; see $event->rawData).
 */
final class WebhookReceived extends TalksasaPayEvent
{
}
