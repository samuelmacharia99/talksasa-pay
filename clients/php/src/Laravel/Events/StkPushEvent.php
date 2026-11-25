<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\StkPush;

/**
 * Base of the stk_push.* events. $stkPush is the prompt's state at the time
 * of the event. These report the prompt, not the charge: mark an invoice
 * paid only on ChargeSucceeded.
 */
abstract class StkPushEvent extends TalksasaPayEvent
{
    public readonly StkPush $stkPush;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->stkPush = $event->stkPush();
    }
}
