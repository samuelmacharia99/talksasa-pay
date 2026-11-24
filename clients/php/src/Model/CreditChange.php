<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use Talksasa\Pay\Internal\Data;

/** Data of a payer.credit_changed event. */
final readonly class CreditChange
{
    public function __construct(
        public PayerRef $payer,
        /** KES added (positive) or used (negative). */
        public int $delta,
        /** Balance after the change. */
        public int $creditBalance,
        public ?string $transactionId,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(
            PayerRef::fromArray(Data::obj($a, 'payer')),
            Data::int($a, 'delta'),
            Data::int($a, 'credit_balance'),
            Data::nullableStr($a, 'transaction_id'),
        );
    }
}
