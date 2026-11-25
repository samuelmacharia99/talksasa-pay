<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\PaymentReceived as Payment;

/**
 * payment.received: money for your app that paid no waiting charge.
 *   - $payment->isDuplicate(): the customer paid an already-succeeded
 *     charge ($payment->duplicateOfCharge) again. Refund it or credit the
 *     customer in your own system.
 *   - otherwise: a paybill payment with your prefix that matched no waiting
 *     charge. Decide what it pays from $payment->accountNumber and
 *     $payment->amount.
 */
final class PaymentReceived extends TalksasaPayEvent
{
    public readonly Payment $payment;

    public function __construct(Event $event, string $source)
    {
        parent::__construct($event, $source);
        $this->payment = $event->paymentReceived();
    }
}
