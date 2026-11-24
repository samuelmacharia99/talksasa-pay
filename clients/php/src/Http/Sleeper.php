<?php

declare(strict_types=1);

namespace Talksasa\Pay\Http;

/** Waits between retries. Inject a fake in tests. */
interface Sleeper
{
    public function sleep(float $seconds): void;
}
