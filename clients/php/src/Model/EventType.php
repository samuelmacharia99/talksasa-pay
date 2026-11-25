<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

/** Event type names (part of the public contract: added to, never renamed). */
final class EventType
{
    /** Paid with an M-Pesa receipt for exactly the charge amount. Mark your invoice paid. */
    public const CHARGE_SUCCEEDED = 'charge.succeeded';
    /** Safaricom said paid without a receipt; staff are confirming. NOT paid yet. */
    public const CHARGE_NEEDS_REVIEW = 'charge.needs_review';
    /** Not paid in time. Late money still produces charge.succeeded. */
    public const CHARGE_EXPIRED = 'charge.expired';
    /** You cancelled it. Late money still produces charge.succeeded. */
    public const CHARGE_CANCELLED = 'charge.cancelled';
    /** The M-Pesa prompt is on the customer's phone. NOT paid. */
    public const STK_PUSH_SENT = 'stk_push.sent';
    /** The prompt failed (cancelled, timed out, insufficient funds, wrong PIN...). The charge stays pending. */
    public const STK_PUSH_FAILED = 'stk_push.failed';
    /** Safaricom confirmed the prompt with a receipt; charge.succeeded comes with it. */
    public const STK_PUSH_SUCCEEDED = 'stk_push.succeeded';
    /** Safaricom reported success without a receipt; staff are confirming. NOT paid. */
    public const STK_PUSH_NEEDS_REVIEW = 'stk_push.needs_review';
    /** Money for your app that paid no waiting charge (a duplicate, or an unmatched paybill payment). */
    public const PAYMENT_RECEIVED = 'payment.received';
    /** Staff reversed a payment; its charge (if any) is now reversed. */
    public const TRANSACTION_REVERSED = 'transaction.reversed';

    public const ALL = [
        self::CHARGE_SUCCEEDED,
        self::CHARGE_NEEDS_REVIEW,
        self::CHARGE_EXPIRED,
        self::CHARGE_CANCELLED,
        self::STK_PUSH_SENT,
        self::STK_PUSH_FAILED,
        self::STK_PUSH_SUCCEEDED,
        self::STK_PUSH_NEEDS_REVIEW,
        self::PAYMENT_RECEIVED,
        self::TRANSACTION_REVERSED,
    ];

    private function __construct()
    {
    }
}
