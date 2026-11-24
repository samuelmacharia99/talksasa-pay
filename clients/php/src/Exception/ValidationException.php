<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * The request was rejected as invalid: validation_failed (see $fieldErrors),
 * invalid_json, unsupported_media_type, idempotency_key_required,
 * payload_too_large. Fix the request; retrying it unchanged will not help.
 */
final class ValidationException extends ApiException
{
}
