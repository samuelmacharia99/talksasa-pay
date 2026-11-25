<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;

/**
 * Money received (or reversed) for your app. A reversal is its own
 * transaction (kind reversal) whose $reversesTransactionId names the payment.
 */
readonly class Transaction
{
    public const KIND_PAYMENT = 'payment';
    public const KIND_REVERSAL = 'reversal';

    public function __construct(
        public string $id,
        /** payment | refund | reversal | adjustment */
        public string $kind,
        /** in | out */
        public string $direction,
        /** mpesa_stk | mpesa_c2b | mpesa_b2c | manual */
        public string $channel,
        /** pending | succeeded | failed | cancelled | needs_review */
        public string $status,
        /** Whole KES. */
        public int $amount,
        public string $currency,
        /** M-Pesa receipt number. */
        public ?string $receipt,
        /** Masked. */
        public ?string $phone,
        /** The account number as typed at the paybill, or as sent with the prompt. */
        public ?string $accountNumber,
        /** The charge this money was for, when known. */
        public ?string $chargeId,
        public ?string $reversesTransactionId,
        public ?DateTimeImmutable $occurredAt,
        public DateTimeImmutable $createdAt,
        public ?DateTimeImmutable $completedAt,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(...self::fields($a));
    }

    /**
     * Constructor arguments by name, for subclasses.
     *
     * @param array<string,mixed> $a
     * @return array<string,mixed>
     */
    protected static function fields(array $a): array
    {
        return [
            'id' => Data::str($a, 'id'),
            'kind' => Data::str($a, 'kind'),
            'direction' => Data::str($a, 'direction'),
            'channel' => Data::str($a, 'channel'),
            'status' => Data::str($a, 'status'),
            'amount' => Data::int($a, 'amount'),
            'currency' => Data::str($a, 'currency'),
            'receipt' => Data::nullableStr($a, 'receipt'),
            'phone' => Data::nullableStr($a, 'phone'),
            'accountNumber' => Data::nullableStr($a, 'account_number'),
            'chargeId' => Data::nullableStr($a, 'charge_id'),
            'reversesTransactionId' => Data::nullableStr($a, 'reverses_transaction_id'),
            'occurredAt' => Data::nullableDate($a, 'occurred_at'),
            'createdAt' => Data::date($a, 'created_at'),
            'completedAt' => Data::nullableDate($a, 'completed_at'),
        ];
    }

    public function isReversal(): bool
    {
        return $this->kind === self::KIND_REVERSAL;
    }
}
