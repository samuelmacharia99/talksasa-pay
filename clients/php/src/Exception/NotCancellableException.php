<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 not_cancellable: only pending charges can be cancelled. The charge has
 * succeeded, is under review, expired or was reversed; re-read it.
 */
final class NotCancellableException extends ConflictException
{
}
