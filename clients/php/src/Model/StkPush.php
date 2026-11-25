<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;

/**
 * An M-Pesa payment prompt (STK push) sent to a customer's phone for a
 * charge's exact amount.
 *
 * A prompt's status is NOT the charge's status. Mark your invoice paid only
 * on charge.succeeded (or a re-read Charge with status succeeded), never
 * because a prompt was sent or is in needs_review. A failed prompt does not
 * fail the charge: it stays pending and can be prompted again.
 */
final readonly class StkPush
{
    /** Being sent to Safaricom. */
    public const STATUS_INITIATING = 'initiating';
    /** On the customer's phone, waiting for their PIN. */
    public const STATUS_SENT = 'sent';
    /** Paid: Safaricom confirmed it with an M-Pesa receipt. */
    public const STATUS_SUCCEEDED = 'succeeded';
    /** Not paid: see $resultCode / $resultDesc. The charge stays pending. */
    public const STATUS_FAILED = 'failed';
    /** Safaricom reported success without a receipt; Talksasa staff are confirming it. Not paid yet. */
    public const STATUS_NEEDS_REVIEW = 'needs_review';

    public function __construct(
        public string $id,
        public string $chargeId,
        /** The M-Pesa transaction that holds (or will hold) this payment. */
        public string $transactionId,
        /** initiating | sent | succeeded | failed | needs_review */
        public string $status,
        /** Whole KES: always the charge's amount. */
        public int $amount,
        /** Masked, e.g. 2547****5678. */
        public string $phone,
        /** Safaricom's CheckoutRequestID; quote it to support. */
        public ?string $checkoutRequestId,
        /** Safaricom result code, e.g. "1032" (cancelled by the customer). */
        public ?string $resultCode,
        /** Safaricom's description of the result. */
        public ?string $resultDesc,
        public DateTimeImmutable $createdAt,
        public DateTimeImmutable $updatedAt,
        public ?DateTimeImmutable $completedAt,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(
            Data::str($a, 'id'),
            Data::str($a, 'charge_id'),
            Data::str($a, 'transaction_id'),
            Data::str($a, 'status'),
            Data::int($a, 'amount'),
            Data::str($a, 'phone'),
            Data::nullableStr($a, 'checkout_request_id'),
            Data::nullableStr($a, 'result_code'),
            Data::nullableStr($a, 'result_desc'),
            Data::date($a, 'created_at'),
            Data::date($a, 'updated_at'),
            Data::nullableDate($a, 'completed_at'),
        );
    }

    /** Still waiting for the customer or Safaricom (initiating or sent). */
    public function isPending(): bool
    {
        return $this->status === self::STATUS_INITIATING || $this->status === self::STATUS_SENT;
    }

    public function isFailed(): bool
    {
        return $this->status === self::STATUS_FAILED;
    }

    public function isSucceeded(): bool
    {
        return $this->status === self::STATUS_SUCCEEDED;
    }

    /** The customer cancelled the prompt (Safaricom result code 1032). */
    public function wasCancelledByCustomer(): bool
    {
        return $this->status === self::STATUS_FAILED && $this->resultCode === '1032';
    }
}
