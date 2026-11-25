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
        yield 'ip_not_allowed' => [403, 'ip_not_allowed', Exception\IpNotAllowedException::class];
        yield 'app_suspended' => [403, 'app_suspended', Exception\AppSuspendedException::class];
        yield 'rate_limited' => [429, 'rate_limited', Exception\RateLimitedException::class];
        yield 'overloaded' => [503, 'overloaded', Exception\ServerException::class];
        yield 'busy' => [503, 'busy', Exception\ServerException::class];
        yield 'timeout' => [503, 'timeout', Exception\ServerException::class];
        yield 'not_found' => [404, 'not_found', Exception\NotFoundException::class];
        yield 'method_not_allowed' => [405, 'method_not_allowed', Exception\ValidationException::class];
        yield 'validation_failed' => [422, 'validation_failed', Exception\ValidationException::class];
        yield 'invalid_json' => [400, 'invalid_json', Exception\ValidationException::class];
        yield 'unsupported_media_type' => [415, 'unsupported_media_type', Exception\ValidationException::class];
        yield 'payload_too_large' => [413, 'payload_too_large', Exception\ValidationException::class];
        yield 'idempotency_key_required' => [400, 'idempotency_key_required', Exception\ValidationException::class];
        yield 'idempotency_conflict' => [409, 'idempotency_conflict', Exception\IdempotencyConflictException::class];
        yield 'duplicate_reference' => [409, 'duplicate_reference', Exception\DuplicateReferenceException::class];
        yield 'not_cancellable' => [409, 'not_cancellable', Exception\NotCancellableException::class];
        yield 'not_payable' => [409, 'not_payable', Exception\NotPayableException::class];
        yield 'amount_too_large' => [409, 'amount_too_large', Exception\AmountTooLargeException::class];
        yield 'stk_in_progress' => [409, 'stk_in_progress', Exception\StkInProgressException::class];
        yield 'mpesa_unavailable' => [503, 'mpesa_unavailable', Exception\MpesaUnavailableException::class];
        yield 'mpesa_error' => [502, 'mpesa_error', Exception\MpesaErrorException::class];
        yield 'not_implemented' => [501, 'not_implemented', Exception\ServerException::class];
        yield 'internal_error' => [500, 'internal_error', Exception\ServerException::class];
        yield 'unknown code, 409' => [409, 'brand_new_conflict', Exception\ConflictException::class];
        yield 'unknown code, 403' => [403, 'brand_new_denial', Exception\PermissionDeniedException::class];
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
            $this->client(RetryPolicy::none())->getCharge('x');
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

    public function testHierarchy(): void
    {
        self::assertTrue(is_subclass_of(Exception\IpNotAllowedException::class, Exception\PermissionDeniedException::class));
        self::assertTrue(is_subclass_of(Exception\AppSuspendedException::class, Exception\PermissionDeniedException::class));
        self::assertTrue(is_subclass_of(Exception\DuplicateReferenceException::class, Exception\ConflictException::class));
        self::assertTrue(is_subclass_of(Exception\MpesaErrorException::class, Exception\ServerException::class));
        self::assertTrue(is_subclass_of(Exception\MpesaUnavailableException::class, Exception\ServerException::class));
    }

    public function testValidationFieldErrorsAndMessage(): void
    {
        $this->http->pushJson(422, [
            'type' => 'about:blank', 'title' => 'Validation failed', 'status' => 422, 'code' => 'validation_failed',
            'detail' => 'Some fields are invalid.', 'request_id' => 'req-2',
            'errors' => [['field' => 'amount', 'message' => 'must be at least 1'], ['field' => 'account_number', 'message' => 'must start with HOST-'], ['bogus' => 1]],
        ]);
        try {
            $this->client()->createCharge(0, 'r', accountNumber: 'X-1');
            self::fail('expected exception');
        } catch (Exception\ValidationException $e) {
            self::assertCount(2, $e->fieldErrors);
            self::assertSame('amount', $e->fieldErrors[0]->field);
            self::assertSame('must be at least 1', $e->fieldErrors[0]->message);
            self::assertSame('account_number', $e->fieldErrors[1]->field);
            self::assertSame('Talksasa Pay API error 422 (validation_failed): Some fields are invalid. [request_id req-2]', $e->getMessage());
        }
        self::assertCount(1, $this->http->requests);
    }

    public function testDuplicateReferenceOnCreateIsNotRetried(): void
    {
        $this->http->pushJson(409, ['type' => 'about:blank', 'title' => 'Duplicate', 'status' => 409, 'code' => 'duplicate_reference']);
        $this->expectException(Exception\DuplicateReferenceException::class);
        try {
            $this->client()->createCharge(1500, 'INV-1042');
        } finally {
            self::assertCount(1, $this->http->requests);
        }
    }

    public function testRequestIdFallsBackToHeader(): void
    {
        $this->http->push(new Response(404, ['X-Request-Id' => 'hdr-id', 'Content-Type' => 'application/problem+json'], '{"status":404,"code":"not_found","title":"Not found"}'));
        try {
            $this->client()->getCharge('nope');
            self::fail('expected exception');
        } catch (Exception\NotFoundException $e) {
            self::assertSame('hdr-id', $e->requestId);
            self::assertNull($e->detail);
            self::assertSame('Not found', $e->title);
        }
    }
}
