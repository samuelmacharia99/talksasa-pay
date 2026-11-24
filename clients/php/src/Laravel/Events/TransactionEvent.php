<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\Transaction;

/** Base of the transaction.* events. */
abstract class TransactionEvent extends TalksasaPayEvent
{
    public readonly Transaction $transaction;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->transaction = $event->transaction();
    }
}
