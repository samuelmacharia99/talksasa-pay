<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** charge.expired: not paid in time. If money still arrives, ChargeSucceeded follows. */
final class ChargeExpired extends ChargeEvent
{
}
