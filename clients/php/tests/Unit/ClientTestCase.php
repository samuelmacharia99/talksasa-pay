<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use GuzzleHttp\Psr7\HttpFactory;
use PHPUnit\Framework\TestCase;
use Talksasa\Pay\Client;
use Talksasa\Pay\Http\RetryPolicy;
use Talksasa\Pay\Tests\Support\FakeSleeper;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Tests\Support\MockHttpClient;

abstract class ClientTestCase extends TestCase
{
    protected MockHttpClient $http;
    protected FakeSleeper $sleeper;

    protected function setUp(): void
    {
        $this->http = new MockHttpClient();
        $this->sleeper = new FakeSleeper();
    }

    protected function client(?RetryPolicy $retry = null, ?callable $keys = null): Client
    {
        $f = new HttpFactory();
        return new Client(
            apiKey: Fixtures::API_KEY,
            baseUrl: Fixtures::BASE_URL,
            httpClient: $this->http,
            requestFactory: $f,
            streamFactory: $f,
            retryPolicy: $retry ?? new RetryPolicy(maxRetries: 2, random: static fn (): float => 0.5),
            sleeper: $this->sleeper,
            idempotencyKeyGenerator: $keys,
        );
    }
}
