<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 duplicate_reference: an open or partially-paid payment request with
 * this reference already exists. Find it with
 * Client::listPaymentRequests(reference: ...) instead of creating another.
 */
final class DuplicateReferenceException extends ConflictException
{
}
