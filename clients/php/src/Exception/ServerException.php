<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 5xx: internal_error, overloaded (503), or a proxy error page (after the client's own retries). */
final class ServerException extends ApiException
{
}
