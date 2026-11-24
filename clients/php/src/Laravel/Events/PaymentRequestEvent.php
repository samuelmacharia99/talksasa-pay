<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\PaymentRequest;

/** Base of the payment_request.* events. $paymentRequest is its state at the time of the event. */
abstract class PaymentRequestEvent extends TalksasaPayEvent
{
    public readonly PaymentRequest $paymentRequest;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->paymentRequest = $event->paymentRequest();
    }
}
