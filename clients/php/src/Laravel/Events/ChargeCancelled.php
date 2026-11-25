<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** charge.cancelled: you cancelled the charge. If the customer still pays, ChargeSucceeded follows. */
final class ChargeCancelled extends ChargeEvent
{
}
