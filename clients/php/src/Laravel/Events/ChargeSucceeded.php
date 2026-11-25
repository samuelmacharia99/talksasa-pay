<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/**
 * charge.succeeded: paid with an M-Pesa receipt for exactly the charge
 * amount ($charge->transactionId). Mark your invoice paid. May arrive for a
 * charge you cancelled or that expired: late money is never dropped.
 */
final class ChargeSucceeded extends ChargeEvent
{
}
