<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 409 not_cancellable: money has been paid on the request. Talk to Talksasa finance. */
final class NotCancellableException extends ConflictException
{
}
