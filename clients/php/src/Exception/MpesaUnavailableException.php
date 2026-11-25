<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 503 mpesa_unavailable: M-Pesa prompts are not enabled in this environment.
 * Never retried automatically. Send the customer to the charge's
 * checkout_url or show its paybill instructions instead.
 */
final class MpesaUnavailableException extends ServerException
{
}
