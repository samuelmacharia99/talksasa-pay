<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use Talksasa\Pay\Internal\Data;

/** Manual M-Pesa instructions: Lipa na M-Pesa > Paybill > business number > account number. */
final readonly class Paybill
{
    public function __construct(
        public string $businessNumber,
        public string $accountNumber,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(Data::str($a, 'business_number'), Data::str($a, 'account_number'));
    }
}
