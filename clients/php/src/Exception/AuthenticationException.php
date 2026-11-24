<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 401 unauthorized: the API key is missing, malformed, revoked or expired. Not retryable. */
final class AuthenticationException extends ApiException
{
}
