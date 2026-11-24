<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** stk_push.needs_review: Safaricom reported success without a receipt and Talksasa staff are confirming it. NOT paid: wait for PaymentRequestPaid. */
final class StkPushNeedsReview extends StkPushEvent
{
}
