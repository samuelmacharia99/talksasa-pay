<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;

/**
 * A charge: "collect exactly $amount KES for my $reference". Amounts are
 * whole KES. The amount, reference and account number never change; to
 * charge a different amount, cancel and create a new charge.
 *
 * Mark your invoice paid only when the charge is succeeded (the
 * charge.succeeded event, or a re-read charge), never because a prompt was
 * sent or the charge is in needs_review.
 */
final readonly class Charge
{
    /** Waiting for money; prompts may be sent (again). */
    public const STATUS_PENDING = 'pending';
    /** Safaricom said paid without a receipt; Talksasa staff are checking. NOT paid yet. */
    public const STATUS_NEEDS_REVIEW = 'needs_review';
    /** Paid: see $transactionId. */
    public const STATUS_SUCCEEDED = 'succeeded';
    /** Not paid in time. Late money still makes it succeeded. */
    public const STATUS_EXPIRED = 'expired';
    /** You cancelled it. Late money still makes it succeeded. */
    public const STATUS_CANCELLED = 'cancelled';
    /** Talksasa staff reversed the payment. */
    public const STATUS_REVERSED = 'reversed';

    /**
     * @param array<string,mixed>|null $metadata
     */
    public function __construct(
        public string $id,
        /** Your own reference (invoice number, order id); unique among your charges. */
        public string $reference,
        /** Whole KES. */
        public int $amount,
        public string $currency,
        public ?string $description,
        /** As you sent it. */
        public ?array $metadata,
        /** Paybill account number and STK AccountReference, e.g. HOST-1042. */
        public string $accountNumber,
        /** pending | needs_review | succeeded | expired | cancelled | reversed */
        public string $status,
        /** Hosted pay page for this charge: redirect the customer here. */
        public string $checkoutUrl,
        /** Manual M-Pesa instructions while the charge can be paid; null otherwise. */
        public ?Paybill $paybill,
        public DateTimeImmutable $expiresAt,
        public ?DateTimeImmutable $succeededAt,
        /** The payment that made the charge succeed. */
        public ?string $transactionId,
        /** The newest prompt, if any. */
        public ?StkPush $latestStkPush,
        public DateTimeImmutable $createdAt,
        public DateTimeImmutable $updatedAt,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        $paybill = Data::nullableObj($a, 'paybill');
        $push = Data::nullableObj($a, 'latest_stk_push');

        return new self(
            Data::str($a, 'id'),
            Data::str($a, 'reference'),
            Data::int($a, 'amount'),
            Data::str($a, 'currency'),
            Data::nullableStr($a, 'description'),
            Data::nullableObj($a, 'metadata'),
            Data::str($a, 'account_number'),
            Data::str($a, 'status'),
            Data::str($a, 'checkout_url'),
            $paybill === null ? null : Paybill::fromArray($paybill),
            Data::date($a, 'expires_at'),
            Data::nullableDate($a, 'succeeded_at'),
            Data::nullableStr($a, 'transaction_id'),
            $push === null ? null : StkPush::fromArray($push),
            Data::date($a, 'created_at'),
            Data::date($a, 'updated_at'),
        );
    }

    /** Paid (with an M-Pesa receipt for exactly $amount). */
    public function isSucceeded(): bool
    {
        return $this->status === self::STATUS_SUCCEEDED;
    }

    /** Waiting for money: it can be prompted and cancelled. */
    public function isPending(): bool
    {
        return $this->status === self::STATUS_PENDING;
    }

    /** Safaricom reported success without a receipt; staff are confirming. Not paid yet. */
    public function needsReview(): bool
    {
        return $this->status === self::STATUS_NEEDS_REVIEW;
    }
}
