<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Support;

use Talksasa\Pay\Http\Sleeper;

final class FakeSleeper implements Sleeper
{
    /** @var list<float> */
    public array $sleeps = [];

    public function sleep(float $seconds): void
    {
        $this->sleeps[] = $seconds;
    }
}
