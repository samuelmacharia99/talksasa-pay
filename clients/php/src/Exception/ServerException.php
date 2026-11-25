<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 5xx (after the client's own retries): internal_error, not_implemented,
 * 503 overloaded / busy (nothing was changed), 503 timeout (outcome unknown:
 * retry with the SAME idempotency key, or re-read), or a proxy error page.
 * M-Pesa failures are the subclasses MpesaUnavailableException and
 * MpesaErrorException.
 */
class ServerException extends ApiException
{
}
