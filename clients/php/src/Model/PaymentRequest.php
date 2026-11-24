<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;

/** A bill you asked Talksasa Pay to collect. Amounts are whole KES. */
final readonly class PaymentRequest
{
    public const STATUS_OPEN = 'open';
    public const STATUS_PARTIALLY_PAID = 'partially_paid';
    public const STATUS_PAID = 'paid';
    public const STATUS_CANCELLED = 'cancelled';
    public const STATUS_EXPIRED = 'expired';

    public function __construct(
        public string $id,
        /** Your invoice number. */
        public string $reference,
        public string $description,
        public string $currency,
        public int $amount,
        public int $amountPaid,
        public int $outstanding,
        public string $status,
        public PayerRef $payer,
        /** Hosted pay page: redirect the customer here. */
        public string $payUrl,
        /** Null once the request is closed. */
        public ?Paybill $paybill,
        public ?DateTimeImmutable $expiresAt,
        public DateTimeImmutable $createdAt,
        public DateTimeImmutable $updatedAt,
        public ?DateTimeImmutable $paidAt,
        public ?DateTimeImmutable $closedAt,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        $paybill = Data::nullableObj($a, 'paybill');

        return new self(
            Data::str($a, 'id'),
            Data::str($a, 'reference'),
            Data::str($a, 'description'),
            Data::str($a, 'currency'),
            Data::int($a, 'amount'),
            Data::int($a, 'amount_paid'),
            Data::int($a, 'outstanding'),
            Data::str($a, 'status'),
            PayerRef::fromArray(Data::obj($a, 'payer')),
            Data::str($a, 'pay_url'),
            $paybill === null ? null : Paybill::fromArray($paybill),
            Data::nullableDate($a, 'expires_at'),
            Data::date($a, 'created_at'),
            Data::date($a, 'updated_at'),
            Data::nullableDate($a, 'paid_at'),
            Data::nullableDate($a, 'closed_at'),
        );
    }

    public function isPaid(): bool
    {
        return $this->status === self::STATUS_PAID;
    }

    /** Open or partially paid: the customer can still pay it. */
    public function isPayable(): bool
    {
        return $this->status === self::STATUS_OPEN || $this->status === self::STATUS_PARTIALLY_PAID;
    }
}
