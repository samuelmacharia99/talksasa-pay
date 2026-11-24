<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

/** Event type names (part of the public contract: added to, never renamed). */
final class EventType
{
    public const PAYMENT_REQUEST_CREATED = 'payment_request.created';
    public const PAYMENT_REQUEST_PARTIALLY_PAID = 'payment_request.partially_paid';
    public const PAYMENT_REQUEST_PAID = 'payment_request.paid';
    /** A payment on the request was reversed. */
    public const PAYMENT_REQUEST_REOPENED = 'payment_request.reopened';
    public const PAYMENT_REQUEST_CANCELLED = 'payment_request.cancelled';
    public const PAYMENT_REQUEST_EXPIRED = 'payment_request.expired';
    public const TRANSACTION_ALLOCATED = 'transaction.allocated';
    public const TRANSACTION_REVERSED = 'transaction.reversed';
    public const PAYER_CREDIT_CHANGED = 'payer.credit_changed';
    /** The M-Pesa prompt is on the customer's phone. NOT paid. */
    public const STK_PUSH_SENT = 'stk_push.sent';
    /** Safaricom confirmed the prompt with a receipt; payment_request.paid/partially_paid follows. */
    public const STK_PUSH_SUCCEEDED = 'stk_push.succeeded';
    /** The prompt failed (cancelled, timed out, insufficient funds, wrong PIN...). See result_desc. */
    public const STK_PUSH_FAILED = 'stk_push.failed';
    /** Safaricom reported success without a receipt; staff are confirming. NOT paid. */
    public const STK_PUSH_NEEDS_REVIEW = 'stk_push.needs_review';

    public const ALL = [
        self::PAYMENT_REQUEST_CREATED,
        self::PAYMENT_REQUEST_PARTIALLY_PAID,
        self::PAYMENT_REQUEST_PAID,
        self::PAYMENT_REQUEST_REOPENED,
        self::PAYMENT_REQUEST_CANCELLED,
        self::PAYMENT_REQUEST_EXPIRED,
        self::TRANSACTION_ALLOCATED,
        self::TRANSACTION_REVERSED,
        self::PAYER_CREDIT_CHANGED,
        self::STK_PUSH_SENT,
        self::STK_PUSH_SUCCEEDED,
        self::STK_PUSH_FAILED,
        self::STK_PUSH_NEEDS_REVIEW,
    ];

    private function __construct()
    {
    }
}
