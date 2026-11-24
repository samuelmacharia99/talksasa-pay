<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** transaction.allocated: money arrived; $transaction->allocations says where it went. */
final class TransactionAllocated extends TransactionEvent
{
}
