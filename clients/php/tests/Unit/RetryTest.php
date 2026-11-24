<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use GuzzleHttp\Psr7\Response;
use PHPUnit\Framework\Attributes\DataProvider;
use Psr\Http\Client\ClientExceptionInterface;
use Talksasa\Pay\Exception\RateLimitedException;
use Talksasa\Pay\Exception\ServerException;
use Talksasa\Pay\Exception\TransportException;
use Talksasa\Pay\Http\RetryPolicy;
use Talksasa\Pay\Tests\Support\Fixtures;

final class RetryTest extends ClientTestCase
{
    private const PROBLEM = ['type' => 'about:blank', 'title' => 'Overloaded', 'status' => 503, 'code' => 'overloaded'];

    /** @return iterable<string,array{int}> */
    public static function retryableStatuses(): iterable
    {
        yield '429' => [429];
        yield '502' => [502];
        yield '503' => [503];
        yield '504' => [504];
    }

    #[DataProvider('retryableStatuses')]
    public function testRetriesRetryableStatusThenSucceeds(int $status): void
    {
        $this->http->pushJson($status, ['status' => $status] + self::PROBLEM);
        $this->http->pushJson(200, Fixtures::payer());

        $this->client()->getPayer('cust-77');

        self::assertCount(2, $this->http->requests);
        self::assertSame([0.375], $this->sleeper->sleeps); // base 0.5, equal jitter with r=0.5
    }

    public function testExponentialBackoffAndGiveUp(): void
    {
        $this->http->pushJson(503, self::PROBLEM, ['Retry-After' => '0']);
        $this->http->pushJson(503, self::PROBLEM);
        $this->http->pushJson(503, self::PROBLEM);
        $this->http->pushJson(503, self::PROBLEM);

        $retry = new RetryPolicy(maxRetries: 3, baseDelay: 1.0, maxDelay: 3.0, random: static fn (): float => 1.0);
        try {
            $this->client($retry)->getPayer('x');
            self::fail('expected exception');
        } catch (ServerException $e) {
            self::assertSame(503, $e->status);
            self::assertSame('overloaded', $e->errorCode);
        }
        self::assertCount(4, $this->http->requests);
        self::assertSame([1.0, 2.0, 3.0], $this->sleeper->sleeps); // 1, 2, 4 capped at 3
    }

    public function testJitterRange(): void
    {
        $lo = new RetryPolicy(maxRetries: 5, baseDelay: 1.0, maxDelay: 100.0, random: static fn (): float => 0.0);
        $hi = new RetryPolicy(maxRetries: 5, baseDelay: 1.0, maxDelay: 100.0, random: static fn (): float => 0.999999);
        self::assertSame(4.0, $lo->delay(4));
        self::assertEqualsWithDelta(8.0, $hi->delay(4), 0.0001);
        self::assertNull($lo->delay(6));
        self::assertNull($lo->delay(0));
    }

    public function testHonoursRetryAfterAsFloor(): void
    {
        $this->http->pushJson(429, ['status' => 429, 'code' => 'rate_limited'] + self::PROBLEM, ['Retry-After' => '7']);
        $this->http->pushJson(200, Fixtures::payer());

        $this->client()->getPayer('x');
        self::assertSame([7.0], $this->sleeper->sleeps);
    }

    public function testRetryAfterHttpDate(): void
    {
        $this->http->pushJson(503, self::PROBLEM, ['Retry-After' => gmdate('D, d M Y H:i:s \G\M\T', time() + 5)]);
        $this->http->pushJson(200, Fixtures::payer());

        $this->client()->getPayer('x');
        self::assertCount(1, $this->sleeper->sleeps);
        self::assertGreaterThanOrEqual(3.0, $this->sleeper->sleeps[0]);
        self::assertLessThanOrEqual(6.0, $this->sleeper->sleeps[0]);
    }

    public function testRetryAfterAboveCapIsNotWaitedOut(): void
    {
        $this->http->pushJson(429, ['status' => 429, 'code' => 'rate_limited'] + self::PROBLEM, ['Retry-After' => '3600']);

        try {
            $this->client()->getPayer('x');
            self::fail('expected exception');
        } catch (RateLimitedException $e) {
            self::assertSame(3600, $e->retryAfter);
        }
        self::assertSame([], $this->sleeper->sleeps);
        self::assertCount(1, $this->http->requests);
    }

    public function testDoesNotRetry500Or4xx(): void
    {
        $this->http->pushJson(500, ['status' => 500, 'code' => 'internal_error'] + self::PROBLEM);
        try {
            $this->client()->getPayer('x');
            self::fail('expected exception');
        } catch (ServerException) {
        }
        $this->http->pushJson(409, ['status' => 409, 'code' => 'duplicate_reference'] + self::PROBLEM);
        try {
            $this->client()->createPaymentRequest('c', 'r', 'd', 1);
            self::fail('expected exception');
        } catch (\Talksasa\Pay\Exception\DuplicateReferenceException) {
        }
        self::assertCount(2, $this->http->requests);
        self::assertSame([], $this->sleeper->sleeps);
    }

    public function testNetworkErrorIsRetriedThenSurfacesAsTransportException(): void
    {
        $this->http->pushNetworkError('timeout 1')->pushNetworkError('timeout 2')->pushNetworkError('timeout 3');
        try {
            $this->client()->whoami();
            self::fail('expected exception');
        } catch (TransportException $e) {
            self::assertStringContainsString('timeout 3', $e->getMessage());
            self::assertStringNotContainsString(Fixtures::API_KEY, $e->getMessage());
        }
        self::assertCount(3, $this->http->requests);
        self::assertCount(2, $this->sleeper->sleeps);
    }

    public function testCreateRetriesWithSameIdempotencyKeyAndBody(): void
    {
        $this->http->pushNetworkError();
        $this->http->pushJson(502, ['status' => 502] + self::PROBLEM);
        $this->http->pushJson(201, Fixtures::paymentRequest());

        $this->client()->createPaymentRequest('cust-77', 'INV-1', 'Hosting', 1500);

        self::assertCount(3, $this->http->requests);
        $keys = array_map(static fn ($r) => $r->getHeaderLine('Idempotency-Key'), $this->http->requests);
        self::assertCount(1, array_unique($keys));
        self::assertNotSame('', $keys[0]);
        foreach ($this->http->requests as $r) {
            self::assertSame('{"payer":"cust-77","reference":"INV-1","description":"Hosting","amount":1500}', (string) $r->getBody());
        }
    }

    public function testPutAndCancelAreRetried(): void
    {
        $this->http->pushJson(503, self::PROBLEM)->pushJson(200, Fixtures::payer());
        $this->http->pushJson(504, ['status' => 504] + self::PROBLEM)->pushJson(200, Fixtures::paymentRequest());
        $c = $this->client();
        $c->upsertPayer('cust-77', 'W');
        $c->cancelPaymentRequest('id');
        self::assertCount(4, $this->http->requests);
    }

    public function testNoRetriesPolicy(): void
    {
        $this->http->pushJson(503, self::PROBLEM);
        $this->expectException(ServerException::class);
        $this->client(RetryPolicy::none())->whoami();
    }

    public function testNonNetworkClientExceptionIsNotRetried(): void
    {
        $this->http->push(new class ('bad request') extends \RuntimeException implements ClientExceptionInterface {
        });
        $this->expectException(TransportException::class);
        try {
            $this->client()->whoami();
        } finally {
            self::assertCount(1, $this->http->requests);
        }
    }

    public function testProxyHtml502StillMapsToServerException(): void
    {
        $this->http->push(new Response(502, ['X-Request-Id' => 'req-9'], '<html>bad gateway</html>'));
        $this->http->push(new Response(502, ['X-Request-Id' => 'req-9'], '<html>bad gateway</html>'));
        $this->http->push(new Response(502, ['X-Request-Id' => 'req-9'], '<html>bad gateway</html>'));
        try {
            $this->client()->whoami();
            self::fail('expected exception');
        } catch (ServerException $e) {
            self::assertSame(502, $e->status);
            self::assertNull($e->errorCode);
            self::assertSame('req-9', $e->requestId);
        }
    }
}
