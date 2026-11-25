<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 not_payable: the charge is not pending (it succeeded, is under review,
 * expired, was cancelled or reversed), so no payment prompt can be sent.
 * Re-read it with Client::getCharge().
 */
final class NotPayableException extends ConflictException
{
}
