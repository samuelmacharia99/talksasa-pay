<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 409 not_payable: the request is already paid, cancelled or expired, so no payment prompt can be sent. */
final class NotPayableException extends ConflictException
{
}
