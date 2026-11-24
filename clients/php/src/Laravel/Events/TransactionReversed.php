<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Events;

/** transaction.reversed: an earlier payment was reversed (negative allocations). */
final class TransactionReversed extends TransactionEvent
{
}
