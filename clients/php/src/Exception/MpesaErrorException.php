<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 502 mpesa_error: Safaricom refused or failed to send the prompt. Nothing
 * was charged. Never retried automatically: ask the customer to pay by
 * paybill, or to try again later (a new attempt needs a new idempotency key,
 * because the same key returns this failed prompt).
 */
final class MpesaErrorException extends ServerException
{
}
