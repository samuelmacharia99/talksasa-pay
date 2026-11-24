<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** payment_request.paid: fully paid. Mark your invoice paid / provision the service. */
final class PaymentRequestPaid extends PaymentRequestEvent
{
}
