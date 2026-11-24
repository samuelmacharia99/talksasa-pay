<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use GuzzleHttp\Psr7\Response;
use PHPUnit\Framework\Attributes\DataProvider;
use Talksasa\Pay\Exception;
use Talksasa\Pay\Exception\ApiException;
use Talksasa\Pay\Http\RetryPolicy;

final class ErrorMappingTest extends ClientTestCase
{
    /** @return iterable<string,array{int,?string,class-string}> */
    public static function cases(): iterable
    {
        yield 'unauthorized' => [401, 'unauthorized', Exception\AuthenticationException::class];
        yield 'ip_not_allowed' => [403, 'ip_not_allowed', Exception\PermissionDeniedException::class];
        yield 'product_suspended' => [403, 'product_suspended', Exception\PermissionDeniedException::class];
        yield 'not_found' => [404, 'not_found', Exception\NotFoundException::class];
        yield 'validation_failed' => [422, 'validation_failed', Exception\ValidationException::class];
        yield 'invalid_json' => [400, 'invalid_json', Exception\ValidationException::class];
        yield 'idempotency_key_required' => [400, 'idempotency_key_required', Exception\ValidationException::class];
        yield 'payload_too_large' => [413, 'payload_too_large', Exception\ValidationException::class];
        yield 'unsupported_media_type' => [415, 'unsupported_media_type', Exception\ValidationException::class];
        yield 'idempotency_conflict' => [409, 'idempotency_conflict', Exception\IdempotencyConflictException::class];
        yield 'duplicate_reference' => [409, 'duplicate_reference', Exception\DuplicateReferenceException::class];
        yield 'not_cancellable' => [409, 'not_cancellable', Exception\NotCancellableException::class];
        yield 'payer_closed' => [409, 'payer_closed', Exception\PayerClosedException::class];
        yield 'payer_closed as 422' => [422, 'payer_closed', Exception\PayerClosedException::class];
        yield 'not_payable' => [409, 'not_payable', Exception\NotPayableException::class];
        yield 'amount_too_large' => [409, 'amount_too_large', Exception\AmountTooLargeException::class];
        yield 'stk_in_progress' => [409, 'stk_in_progress', Exception\StkInProgressException::class];
        yield 'mpesa_unavailable' => [503, 'mpesa_unavailable', Exception\MpesaUnavailableException::class];
        yield 'mpesa_error' => [502, 'mpesa_error', Exception\MpesaErrorException::class];
        yield 'rate_limited' => [429, 'rate_limited', Exception\RateLimitedException::class];
        yield 'overloaded' => [503, 'overloaded', Exception\ServerException::class];
        yield 'internal_error' => [500, 'internal_error', Exception\ServerException::class];
        yield 'unknown code, 409' => [409, 'brand_new_conflict', Exception\ConflictException::class];
        yield 'no code, 404' => [404, null, Exception\NotFoundException::class];
        yield 'no code, 418' => [418, null, ApiException::class];
    }

    #[DataProvider('cases')]
    public function testMapping(int $status, ?string $code, string $class): void
    {
        $body = ['type' => 'https://pay.talksasa.com/problems/x', 'title' => 'T', 'status' => $status, 'detail' => 'D', 'request_id' => 'req-1'];
        if ($code !== null) {
            $body['code'] = $code;
        }
        $this->http->pushJson($status, $body);

        try {
            $this->client(RetryPolicy::none())->getPaymentRequest('x');
            self::fail('expected exception');
        } catch (ApiException $e) {
            self::assertSame($class, $e::class);
            self::assertSame($status, $e->status);
            self::assertSame($status, $e->getCode());
            self::assertSame($code, $e->errorCode);
            self::assertSame('D', $e->detail);
            self::assertSame('req-1', $e->requestId);
            self::assertInstanceOf(Exception\TalksasaPayException::class, $e);
        }
    }

    public function testValidationFieldErrorsAndMessage(): void
    {
        $this->http->pushJson(422, [
            'type' => 'about:blank', 'title' => 'Validation failed', 'status' => 422, 'code' => 'validation_failed',
            'detail' => 'Some fields are invalid.', 'request_id' => 'req-2',
            'errors' => [['field' => 'amount', 'message' => 'must be at least 1'], ['field' => 'payer', 'message' => 'unknown payer'], ['bogus' => 1]],
        ]);
        try {
            $this->client()->createPaymentRequest('c', 'r', 'd', 0);
            self::fail('expected exception');
        } catch (Exception\ValidationException $e) {
            self::assertCount(2, $e->fieldErrors);
            self::assertSame('amount', $e->fieldErrors[0]->field);
            self::assertSame('must be at least 1', $e->fieldErrors[0]->message);
            self::assertSame('Talksasa Pay API error 422 (validation_failed): Some fields are invalid. [request_id req-2]', $e->getMessage());
        }
    }

    public function testRequestIdFallsBackToHeader(): void
    {
        $this->http->push(new Response(404, ['X-Request-Id' => 'hdr-id', 'Content-Type' => 'application/problem+json'], '{"status":404,"code":"not_found","title":"Not found"}'));
        try {
            $this->client()->getPayer('nope');
            self::fail('expected exception');
        } catch (Exception\NotFoundException $e) {
            self::assertSame('hdr-id', $e->requestId);
            self::assertNull($e->detail);
            self::assertSame('Not found', $e->title);
        }
    }
}
