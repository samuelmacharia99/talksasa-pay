<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** payment_request.partially_paid: some money arrived; see $paymentRequest->outstanding. */
final class PaymentRequestPartiallyPaid extends PaymentRequestEvent
{
}
