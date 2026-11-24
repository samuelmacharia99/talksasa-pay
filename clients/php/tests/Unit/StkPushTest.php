<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use Talksasa\Pay\Exception\AmountTooLargeException;
use Talksasa\Pay\Exception\ConflictException;
use Talksasa\Pay\Exception\MpesaErrorException;
use Talksasa\Pay\Exception\MpesaUnavailableException;
use Talksasa\Pay\Exception\NotPayableException;
use Talksasa\Pay\Exception\ServerException;
use Talksasa\Pay\Exception\StkInProgressException;
use Talksasa\Pay\Exception\ValidationException;
use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\EventType;
use Talksasa\Pay\Model\StkPush;
use Talksasa\Pay\Tests\Support\Fixtures;

final class StkPushTest extends ClientTestCase
{
    private const UUID4 = '/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/';

    /** @param array<string,mixed> $extra */
    private static function problem(int $status, string $code, array $extra = []): array
    {
        return $extra + ['type' => 'about:blank', 'title' => 'T', 'status' => $status, 'code' => $code, 'request_id' => 'req-1'];
    }

    public function testStkPushSendsPhoneWithGeneratedKey(): void
    {
        $this->http->pushJson(202, Fixtures::stkPush());
        $push = $this->client()->stkPush(Fixtures::REQUEST_ID, ' 0712345678 ');

        $r = $this->http->last();
        self::assertSame('POST', $r->getMethod());
        self::assertSame('/v1/payment-requests/' . Fixtures::REQUEST_ID . '/stk-push', $r->getUri()->getPath());
        self::assertMatchesRegularExpression(self::UUID4, $r->getHeaderLine('Idempotency-Key'));
        self::assertSame('application/json', $r->getHeaderLine('Content-Type'));
        self::assertSame(['phone' => '0712345678'], $this->http->lastJson());

        self::assertSame(Fixtures::STK_ID, $push->id);
        self::assertSame(Fixtures::REQUEST_ID, $push->paymentRequestId);
        self::assertSame(Fixtures::TXN_ID, $push->transactionId);
        self::assertSame(StkPush::STATUS_SENT, $push->status);
        self::assertSame(1300, $push->amount);
        self::assertSame('2547****5678', $push->phone);
        self::assertSame('ws_CO_241120261015001234', $push->checkoutRequestId);
        self::assertNull($push->resultCode);
        self::assertNull($push->completedAt);
        self::assertSame('2026-11-30T06:15:01.250000', $push->updatedAt->format('Y-m-d\TH:i:s.u'));
        self::assertTrue($push->isPending());
        self::assertFalse($push->isFailed());
    }

    public function testStkPushUsesGivenKeyAndEncodesId(): void
    {
        $this->http->pushJson(200, Fixtures::stkPush(), ['Idempotent-Replayed' => 'true']);
        $this->client()->stkPush('a/b', '+254712345678', 'invoice-42-stk-1');

        $r = $this->http->last();
        self::assertSame('invoice-42-stk-1', $r->getHeaderLine('Idempotency-Key'));
        self::assertSame('/v1/payment-requests/a%2Fb/stk-push', $r->getUri()->getPath());
    }

    public function testStkPushRejectsBadInputLocally(): void
    {
        foreach ([['', null], [str_repeat('7', 21), null], ['0712345678', "bad\nkey"], ['0712345678', '']] as [$phone, $key]) {
            try {
                $this->client()->stkPush(Fixtures::REQUEST_ID, $phone, $key);
                self::fail('expected InvalidArgumentException');
            } catch (\InvalidArgumentException) {
            }
        }
        self::assertSame([], $this->http->requests);
    }

    public function testRetriesKeepTheSameKeyAndBody(): void
    {
        $this->http->pushNetworkError('timed out');
        $this->http->pushJson(503, self::problem(503, 'timeout'));
        $this->http->pushJson(202, Fixtures::stkPush());

        $this->client()->stkPush(Fixtures::REQUEST_ID, '0712345678');

        self::assertCount(3, $this->http->requests);
        $keys = array_unique(array_map(static fn ($r) => $r->getHeaderLine('Idempotency-Key'), $this->http->requests));
        self::assertCount(1, $keys);
        foreach ($this->http->requests as $r) {
            self::assertSame('{"phone":"0712345678"}', (string) $r->getBody());
        }
    }

    public function testRateLimitIsRetriedWithSameKey(): void
    {
        $this->http->pushJson(429, self::problem(429, 'rate_limited'), ['Retry-After' => '1']);
        $this->http->pushJson(202, Fixtures::stkPush());

        $this->client()->stkPush(Fixtures::REQUEST_ID, '0712345678', 'k-1');

        self::assertCount(2, $this->http->requests);
        self::assertSame('k-1', $this->http->requests[1]->getHeaderLine('Idempotency-Key'));
        self::assertSame([1.0], $this->sleeper->sleeps);
    }

    public function testMpesaUnavailableIsNotRetried(): void
    {
        $this->http->pushJson(503, self::problem(503, 'mpesa_unavailable'));
        try {
            $this->client()->stkPush(Fixtures::REQUEST_ID, '0712345678');
            self::fail('expected exception');
        } catch (MpesaUnavailableException $e) {
            self::assertInstanceOf(ServerException::class, $e);
            self::assertSame(503, $e->status);
        }
        self::assertCount(1, $this->http->requests);
        self::assertSame([], $this->sleeper->sleeps);
    }

    public function testMpesaErrorIsNotRetried(): void
    {
        $this->http->pushJson(502, self::problem(502, 'mpesa_error', ['detail' => 'The prompt could not be sent']));
        try {
            $this->client()->stkPush(Fixtures::REQUEST_ID, '0712345678');
            self::fail('expected exception');
        } catch (MpesaErrorException $e) {
            self::assertSame('mpesa_error', $e->errorCode);
            self::assertSame('req-1', $e->requestId);
        }
        self::assertCount(1, $this->http->requests);
    }

    public function testConflictsMapToTypedExceptionsWithoutRetry(): void
    {
        $live = '0193A1B2-0000-7000-8000-0000000000DD';
        $cases = [
            [self::problem(409, 'stk_in_progress', ['detail' => "A prompt is already live: {$live}"]), StkInProgressException::class],
            [self::problem(409, 'not_payable'), NotPayableException::class],
            [self::problem(409, 'amount_too_large'), AmountTooLargeException::class],
        ];
        foreach ($cases as [$body, $class]) {
            $this->http->pushJson(409, $body);
            try {
                $this->client()->stkPush(Fixtures::REQUEST_ID, '0712345678');
                self::fail('expected ' . $class);
            } catch (ConflictException $e) {
                self::assertSame($class, $e::class);
                if ($e instanceof StkInProgressException) {
                    self::assertSame(strtolower($live), $e->liveStkPushId());
                }
            }
        }
        self::assertCount(3, $this->http->requests);
        self::assertSame([], $this->sleeper->sleeps);
    }

    public function testStkInProgressWithoutIdInDetail(): void
    {
        $this->http->pushJson(409, self::problem(409, 'stk_in_progress'));
        try {
            $this->client()->stkPush(Fixtures::REQUEST_ID, '0712345678');
            self::fail('expected exception');
        } catch (StkInProgressException $e) {
            self::assertNull($e->liveStkPushId());
        }
    }

    public function testInvalidPhoneIsValidationException(): void
    {
        $this->http->pushJson(422, self::problem(422, 'validation_failed', ['errors' => [['field' => 'phone', 'message' => 'not a Safaricom number']]]));
        try {
            $this->client()->stkPush(Fixtures::REQUEST_ID, '0812345678');
            self::fail('expected exception');
        } catch (ValidationException $e) {
            self::assertSame('phone', $e->fieldErrors[0]->field);
        }
    }

    public function testGetStkPushFailed(): void
    {
        $this->http->pushJson(200, Fixtures::stkPush([
            'status' => 'failed',
            'result_code' => '1032',
            'result_desc' => 'Request cancelled by user',
            'completed_at' => '2026-11-30T06:16:00Z',
        ]));
        $push = $this->client()->getStkPush(Fixtures::STK_ID);

        self::assertSame('GET', $this->http->last()->getMethod());
        self::assertSame('/v1/stk-pushes/' . Fixtures::STK_ID, $this->http->last()->getUri()->getPath());
        self::assertSame('', $this->http->last()->getHeaderLine('Idempotency-Key'));
        self::assertTrue($push->isFailed());
        self::assertTrue($push->wasCancelledByCustomer());
        self::assertFalse($push->isPending());
        self::assertSame('Request cancelled by user', $push->resultDesc);
        self::assertNotNull($push->completedAt);
    }

    public function testStkEventsDecodeToStkPush(): void
    {
        foreach ([EventType::STK_PUSH_SENT, EventType::STK_PUSH_SUCCEEDED, EventType::STK_PUSH_FAILED, EventType::STK_PUSH_NEEDS_REVIEW] as $type) {
            self::assertContains($type, EventType::ALL);
            $e = Event::fromArray(Fixtures::event($type));
            self::assertInstanceOf(StkPush::class, $e->data);
            self::assertSame(Fixtures::STK_ID, $e->stkPush()->id);
        }

        $paid = Event::fromArray(Fixtures::event('payment_request.paid'));
        $this->expectException(\LogicException::class);
        $paid->stkPush();
    }
}
