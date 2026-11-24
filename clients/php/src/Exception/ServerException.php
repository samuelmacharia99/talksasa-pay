<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 5xx: internal_error, overloaded/timeout (503), or a proxy error page
 * (after the client's own retries). M-Pesa failures are the subclasses
 * MpesaUnavailableException and MpesaErrorException.
 */
class ServerException extends ApiException
{
}
