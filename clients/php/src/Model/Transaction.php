<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;
use Talksasa\Pay\Exception\UnexpectedResponseException;

/** Money received (or reversed) for one of your payers. */
final readonly class Transaction
{
    /**
     * @param list<Allocation> $allocations
     */
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
        public int $amount,
        public string $currency,
        /** M-Pesa receipt number. */
        public ?string $receipt,
        /** Masked. */
        public ?string $phone,
        public ?PayerRef $payer,
        public ?string $paymentRequestId,
        public ?string $reversesTransactionId,
        public array $allocations,
        public ?DateTimeImmutable $occurredAt,
        public DateTimeImmutable $createdAt,
        public ?DateTimeImmutable $completedAt,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        $payer = Data::nullableObj($a, 'payer');
        $allocations = [];
        foreach (Data::list($a, 'allocations') as $row) {
            if (!is_array($row)) {
                throw new UnexpectedResponseException('Expected allocation to be an object.');
            }
            $allocations[] = Allocation::fromArray($row);
        }

        return new self(
            Data::str($a, 'id'),
            Data::str($a, 'kind'),
            Data::str($a, 'direction'),
            Data::str($a, 'channel'),
            Data::str($a, 'status'),
            Data::int($a, 'amount'),
            Data::str($a, 'currency'),
            Data::nullableStr($a, 'receipt'),
            Data::nullableStr($a, 'phone'),
            $payer === null ? null : PayerRef::fromArray($payer),
            Data::nullableStr($a, 'payment_request_id'),
            Data::nullableStr($a, 'reverses_transaction_id'),
            $allocations,
            Data::nullableDate($a, 'occurred_at'),
            Data::date($a, 'created_at'),
            Data::nullableDate($a, 'completed_at'),
        );
    }
}
