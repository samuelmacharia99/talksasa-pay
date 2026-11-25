<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 duplicate_reference: you already have a charge with this reference.
 * Find it with Client::listCharges(reference: ...) instead of creating
 * another; to charge the same invoice again, use a new reference (e.g.
 * "INV-1042-2").
 */
final class DuplicateReferenceException extends ConflictException
{
}
