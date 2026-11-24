<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use Talksasa\Pay\Internal\Data;

/** One line of where a transaction's money went. */
final readonly class Allocation
{
    public const KIND_PAYMENT = 'payment';
    public const KIND_CREDIT_APPLICATION = 'credit_application';
    public const KIND_REVERSAL = 'reversal';

    public function __construct(
        /** Null means the payer's credit balance. */
        public ?string $paymentRequestId,
        /** KES; negative on reversals. */
        public int $amount,
        public string $kind,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(Data::nullableStr($a, 'payment_request_id'), Data::int($a, 'amount'), Data::str($a, 'kind'));
    }

    public function isToCredit(): bool
    {
        return $this->paymentRequestId === null;
    }
}
