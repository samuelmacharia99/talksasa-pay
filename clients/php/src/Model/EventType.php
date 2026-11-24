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
    ];

    private function __construct()
    {
    }
}
