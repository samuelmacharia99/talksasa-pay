<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

use Psr\Http\Message\ResponseInterface;

/**
 * The API answered with an error (RFC 9457 problem+json). Catch a subclass
 * for a specific case, or this class and branch on $errorCode (stable).
 *
 * Quote $requestId when asking Talksasa for help.
 */
class ApiException extends \RuntimeException implements TalksasaPayException
{
    /** Code => class. Codes are stable; statuses are the fallback. */
    private const BY_CODE = [
        'unauthorized' => AuthenticationException::class,
        'ip_not_allowed' => PermissionDeniedException::class,
        'product_suspended' => PermissionDeniedException::class,
        'not_found' => NotFoundException::class,
        'validation_failed' => ValidationException::class,
        'invalid_json' => ValidationException::class,
        'unsupported_media_type' => ValidationException::class,
        'idempotency_key_required' => ValidationException::class,
        'payload_too_large' => ValidationException::class,
        'idempotency_conflict' => IdempotencyConflictException::class,
        'duplicate_reference' => DuplicateReferenceException::class,
        'not_cancellable' => NotCancellableException::class,
        'payer_closed' => PayerClosedException::class,
        'rate_limited' => RateLimitedException::class,
        'overloaded' => ServerException::class,
        'internal_error' => ServerException::class,
    ];

    /**
     * @param list<FieldError> $fieldErrors
     */
    final public function __construct(
        public readonly int $status,
        public readonly ?string $errorCode,
        public readonly ?string $title,
        public readonly ?string $detail,
        public readonly ?string $type,
        public readonly ?string $requestId,
        public readonly array $fieldErrors = [],
        public readonly ?int $retryAfter = null,
        ?\Throwable $previous = null,
    ) {
        $msg = sprintf('Talksasa Pay API error %d', $status);
        if ($errorCode !== null) {
            $msg .= " ({$errorCode})";
        }
        $text = $detail ?? $title;
        if ($text !== null && $text !== '') {
            $msg .= ': ' . $text;
        }
        if ($requestId !== null) {
            $msg .= " [request_id {$requestId}]";
        }
        parent::__construct($msg, $status, $previous);
    }

    /** Builds the most specific exception for an error response. */
    public static function fromResponse(ResponseInterface $response): self
    {
        $status = $response->getStatusCode();
        $raw = (string) $response->getBody();
        $body = json_decode($raw, true);
        $p = is_array($body) && !array_is_list($body) ? $body : [];

        $str = static fn (string $k): ?string => isset($p[$k]) && is_string($p[$k]) && $p[$k] !== '' ? $p[$k] : null;

        $fieldErrors = [];
        if (isset($p['errors']) && is_array($p['errors'])) {
            foreach ($p['errors'] as $e) {
                if (is_array($e) && is_string($e['field'] ?? null) && is_string($e['message'] ?? null)) {
                    $fieldErrors[] = new FieldError($e['field'], $e['message']);
                }
            }
        }

        $code = $str('code');
        $requestId = $str('request_id');
        if ($requestId === null && $response->getHeaderLine('X-Request-Id') !== '') {
            $requestId = $response->getHeaderLine('X-Request-Id');
        }
        $retryAfter = self::parseRetryAfter($response->getHeaderLine('Retry-After'));

        $class = ($code !== null ? (self::BY_CODE[$code] ?? null) : null) ?? self::classForStatus($status);

        return new $class(
            $status,
            $code,
            $str('title'),
            $str('detail'),
            $str('type'),
            $requestId,
            $fieldErrors,
            $retryAfter,
        );
    }

    /** Retry-After as whole seconds (delta-seconds or HTTP-date), or null. */
    public static function parseRetryAfter(string $value, ?int $now = null): ?int
    {
        $value = trim($value);
        if ($value === '') {
            return null;
        }
        if (ctype_digit($value)) {
            return (int) $value;
        }
        $t = strtotime($value);
        if ($t === false) {
            return null;
        }
        return max(0, $t - ($now ?? time()));
    }

    /** @return class-string<self> */
    private static function classForStatus(int $status): string
    {
        return match (true) {
            $status === 401 => AuthenticationException::class,
            $status === 403 => PermissionDeniedException::class,
            $status === 404 => NotFoundException::class,
            $status === 409 => ConflictException::class,
            $status === 429 => RateLimitedException::class,
            $status === 400, $status === 413, $status === 415, $status === 422 => ValidationException::class,
            $status >= 500 => ServerException::class,
            default => self::class,
        };
    }
}
