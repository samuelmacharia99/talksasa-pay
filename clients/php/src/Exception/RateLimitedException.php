<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 429 rate_limited (after the client's own retries). $retryAfter is the server's hint in seconds. */
final class RateLimitedException extends ApiException
{
}
