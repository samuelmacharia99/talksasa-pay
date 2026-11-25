<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Charge;
use Talksasa\Pay\Model\Event;

/** Base of the charge.* events. $charge is its state at the time of the event. */
abstract class ChargeEvent extends TalksasaPayEvent
{
    public readonly Charge $charge;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->charge = $event->charge();
    }
}
