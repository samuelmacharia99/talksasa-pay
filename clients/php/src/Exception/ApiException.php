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
    /**
     * Every problem code of api/app.openapi.yaml => its exception class.
     * Codes are stable; the HTTP status is the fallback for unknown codes.
     */
    public const CLASSES_BY_CODE = [
        'unauthorized' => AuthenticationException::class,
        'ip_not_allowed' => IpNotAllowedException::class,
        'app_suspended' => AppSuspendedException::class,
        'rate_limited' => RateLimitedException::class,
        'overloaded' => ServerException::class,
        'busy' => ServerException::class,
        'timeout' => ServerException::class,
        'not_found' => NotFoundException::class,
        'method_not_allowed' => ValidationException::class,
        'validation_failed' => ValidationException::class,
        'invalid_json' => ValidationException::class,
        'unsupported_media_type' => ValidationException::class,
        'payload_too_large' => ValidationException::class,
        'idempotency_key_required' => ValidationException::class,
        'idempotency_conflict' => IdempotencyConflictException::class,
        'duplicate_reference' => DuplicateReferenceException::class,
        'not_cancellable' => NotCancellableException::class,
        'not_payable' => NotPayableException::class,
        'amount_too_large' => AmountTooLargeException::class,
        'stk_in_progress' => StkInProgressException::class,
        'mpesa_unavailable' => MpesaUnavailableException::class,
        'mpesa_error' => MpesaErrorException::class,
        'not_implemented' => ServerException::class,
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

        $class = ($code !== null ? (self::CLASSES_BY_CODE[$code] ?? null) : null) ?? self::classForStatus($status);

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

    /** A resource id (UUID) quoted in $detail, e.g. the live or failed STK push. */
    protected function uuidInDetail(): ?string
    {
        if ($this->detail !== null && preg_match('/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i', $this->detail, $m) === 1) {
            return strtolower($m[0]);
        }
        return null;
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
