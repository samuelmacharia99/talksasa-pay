<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** stk_push.succeeded: Safaricom confirmed the prompt with a receipt. Act on ChargeSucceeded, which comes with it. */
final class StkPushSucceeded extends StkPushEvent
{
}
