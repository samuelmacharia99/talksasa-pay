<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** payment_request.reopened: a payment on it was reversed; money is owed again. */
final class PaymentRequestReopened extends PaymentRequestEvent
{
}
