<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;

/**
 * Data of payment.received: a payment routed to your app that did NOT pay
 * a waiting charge. Either:
 *   - a duplicate: the customer paid a charge that had already succeeded
 *     ($duplicateOfCharge is that charge's id; refund it or keep it as
 *     credit in your own system), or
 *   - a paybill payment whose account number starts with your prefix but
 *     matched no waiting charge (wrong amount, or a standing account number
 *     such as your customer number): you decide which invoice it pays,
 *     using $accountNumber and $amount.
 */
final readonly class PaymentReceived extends Transaction
{
    public function __construct(
        string $id,
        string $kind,
        string $direction,
        string $channel,
        string $status,
        int $amount,
        string $currency,
        ?string $receipt,
        ?string $phone,
        ?string $accountNumber,
        ?string $chargeId,
        ?string $reversesTransactionId,
        ?DateTimeImmutable $occurredAt,
        DateTimeImmutable $createdAt,
        ?DateTimeImmutable $completedAt,
        /** The already-succeeded charge this payment duplicated, if any. */
        public ?string $duplicateOfCharge,
    ) {
        parent::__construct(
            $id,
            $kind,
            $direction,
            $channel,
            $status,
            $amount,
            $currency,
            $receipt,
            $phone,
            $accountNumber,
            $chargeId,
            $reversesTransactionId,
            $occurredAt,
            $createdAt,
            $completedAt,
        );
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(...self::fields($a), duplicateOfCharge: Data::nullableStr($a, 'duplicate_of_charge'));
    }

    /** The customer paid a charge that had already succeeded. */
    public function isDuplicate(): bool
    {
        return $this->duplicateOfCharge !== null;
    }
}
