<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** payer_closed: the payer account is closed and cannot be billed. */
final class PayerClosedException extends ConflictException
{
}
