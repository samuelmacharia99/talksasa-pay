<?php

declare(strict_types=1);

namespace Talksasa\Pay\Webhook;

use Talksasa\Pay\Exception\TalksasaPayException;

/** A webhook delivery was rejected. Answer 400; never process it. */
abstract class WebhookException extends \RuntimeException implements TalksasaPayException
{
    public function __construct(public readonly string $reason, string $message)
    {
        parent::__construct($message);
    }
}
