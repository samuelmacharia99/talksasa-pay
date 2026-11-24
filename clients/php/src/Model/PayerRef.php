<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use Talksasa\Pay\Internal\Data;

/** Identifies the payer on embedded resources. */
final readonly class PayerRef
{
    public function __construct(
        /** Your own customer id. */
        public string $externalRef,
        /** The M-Pesa account number, e.g. HOST-1042. */
        public string $accountNumber,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(Data::str($a, 'external_ref'), Data::str($a, 'account_number'));
    }
}
