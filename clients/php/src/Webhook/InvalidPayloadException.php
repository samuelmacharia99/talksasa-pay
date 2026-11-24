<?php

declare(strict_types=1);

namespace Talksasa\Pay\Webhook;

/** The signature was valid but the body is not a well-formed event. */
final class InvalidPayloadException extends WebhookException
{
    public const INVALID_PAYLOAD = 'invalid_payload';
}
