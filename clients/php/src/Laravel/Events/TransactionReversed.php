<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\Transaction;

/**
 * transaction.reversed: Talksasa staff reversed a payment (e.g. Safaricom
 * reversed it). $transaction is the reversal: $transaction->reversesTransactionId
 * names the payment, $transaction->chargeId the charge that is now reversed
 * (null for a payment.received payment). Re-open the invoice it paid.
 */
final class TransactionReversed extends TalksasaPayEvent
{
    public readonly Transaction $transaction;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->transaction = $event->transaction();
    }
}
