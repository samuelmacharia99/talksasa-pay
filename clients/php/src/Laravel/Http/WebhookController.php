<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Http;

use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Talksasa\Pay\Laravel\EventRelay;
use Talksasa\Pay\Laravel\Events\TalksasaPayEvent;
use Talksasa\Pay\Model\Event;

/**
 * Receives verified webhooks (behind VerifyWebhookSignature) and dispatches
 * the Laravel events once per event id. Answer fast: do slow work in queued
 * listeners. A listener exception becomes a 500 and Talksasa redelivers.
 */
final class WebhookController
{
    public function __invoke(Request $request, EventRelay $relay): JsonResponse
    {
        $event = $request->attributes->get(VerifyWebhookSignature::ATTRIBUTE);
        if (!$event instanceof Event) {
            // Route registered without the middleware: refuse rather than trust the body.
            return new JsonResponse(['error' => 'signature_not_verified'], 400);
        }

        $fresh = $relay->relay($event, TalksasaPayEvent::SOURCE_WEBHOOK);

        return new JsonResponse(['received' => true, 'duplicate' => !$fresh]);
    }
}
