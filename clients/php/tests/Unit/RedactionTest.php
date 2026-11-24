<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use Talksasa\Pay\Http\Secret;
use Talksasa\Pay\Tests\Support\Fixtures;

final class RedactionTest extends ClientTestCase
{
    public function testClientDumpsNeverShowTheKey(): void
    {
        $c = $this->client();
        self::assertStringNotContainsString(Fixtures::API_KEY, print_r($c, true));
        ob_start();
        var_dump($c);
        $dump = (string) ob_get_clean();
        self::assertStringNotContainsString(Fixtures::API_KEY, $dump);
        self::assertStringContainsString('[redacted]', $dump);
        self::assertSame('{}', json_encode($c));
    }

    public function testClientCannotBeSerialized(): void
    {
        $this->expectException(\LogicException::class);
        serialize($this->client());
    }

    public function testSecret(): void
    {
        $s = new Secret('whsec_abc');
        self::assertSame('whsec_abc', $s->reveal());
        self::assertSame('[redacted]', (string) $s);
        self::assertSame('"[redacted]"', json_encode($s));
        self::assertStringNotContainsString('whsec_abc', print_r($s, true));
        self::assertStringNotContainsString('whsec_abc', var_export(['k' => (string) $s], true));
    }

    public function testStackTracesHideKey(): void
    {
        try {
            new \Talksasa\Pay\Client(Fixtures::API_KEY, 'ftp://wrong');
            self::fail('expected exception');
        } catch (\InvalidArgumentException $e) {
            self::assertStringNotContainsString(Fixtures::API_KEY, $e->getTraceAsString());
            self::assertStringNotContainsString(Fixtures::API_KEY, print_r($e->getTrace(), true));
        }
    }
}
