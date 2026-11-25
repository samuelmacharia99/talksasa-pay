<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 amount_too_large: the amount is above the M-Pesa prompt limit
 * (KES 250,000). Show the paybill instructions instead.
 */
final class AmountTooLargeException extends ConflictException
{
}
