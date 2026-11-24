<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\CreditChange;
use Talksasa\Pay\Model\Event;

/** payer.credit_changed: an overpayment went to (or credit was used from) the payer's balance. */
final class PayerCreditChanged extends TalksasaPayEvent
{
    public readonly CreditChange $creditChange;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->creditChange = $event->creditChange();
    }
}
