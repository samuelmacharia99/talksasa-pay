<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** One entry of a problem's `errors` list (validation_failed). */
final class FieldError
{
    public function __construct(
        public readonly string $field,
        public readonly string $message,
    ) {
    }
}
