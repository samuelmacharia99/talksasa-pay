<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** charge.needs_review: Safaricom said paid without a receipt; Talksasa staff are confirming. NOT paid: wait for ChargeSucceeded. */
final class ChargeNeedsReview extends ChargeEvent
{
}
