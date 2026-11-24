<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 409 idempotency_conflict: this Idempotency-Key was already used with a different body. */
final class IdempotencyConflictException extends ConflictException
{
}
