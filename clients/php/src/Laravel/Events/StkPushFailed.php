<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/**
 * stk_push.failed: the prompt was not paid; $stkPush->resultDesc says why
 * (cancelled, timed out, insufficient funds...). The charge stays pending:
 * offer another prompt or the paybill.
 */
final class StkPushFailed extends StkPushEvent
{
}
