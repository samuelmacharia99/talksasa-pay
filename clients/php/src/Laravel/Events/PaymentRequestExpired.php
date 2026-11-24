<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** payment_request.expired: passed expires_at with nothing paid. */
final class PaymentRequestExpired extends PaymentRequestEvent
{
}
