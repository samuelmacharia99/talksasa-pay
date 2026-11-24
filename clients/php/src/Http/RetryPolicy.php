<?php

declare(strict_types=1);

namespace Talksasa\Pay\Http;

/**
 * When and how long to wait before retrying. Only idempotent requests are
 * retried (every /v1 operation is: GET, the PUT upsert, POST create with an
 * Idempotency-Key, and cancel), and only on network errors, 429, 502, 503
 * and 504.
 *
 * Delay for retry n (1-based): exponential backoff base * 2^(n-1), capped at
 * $maxDelay, with "equal jitter" (half fixed, half random) so many clients
 * do not retry in lock-step. A Retry-After header is a floor: the client
 * never waits less. A Retry-After above $maxRetryAfter is not waited out:
 * the error is thrown at once so a queue/cron can reschedule instead of
 * holding a web request open.
 */
final class RetryPolicy
{
    /** @var \Closure(): float */
    private readonly \Closure $random;

    /**
     * @param (callable(): float)|null $random returns [0, 1); injectable for tests
     */
    public function __construct(
        public readonly int $maxRetries = 2,
        public readonly float $baseDelay = 0.5,
        public readonly float $maxDelay = 8.0,
        public readonly int $maxRetryAfter = 60,
        ?callable $random = null,
    ) {
        if ($maxRetries < 0 || $baseDelay < 0 || $maxDelay < 0 || $maxRetryAfter < 0) {
            throw new \InvalidArgumentException('Retry settings must not be negative.');
        }
        $this->random = $random !== null
            ? \Closure::fromCallable($random)
            : static fn (): float => mt_rand() / (mt_getrandmax() + 1);
    }

    public static function none(): self
    {
        return new self(0);
    }

    public static function isRetryableStatus(int $status): bool
    {
        return $status === 429 || $status === 502 || $status === 503 || $status === 504;
    }

    /**
     * Seconds to wait before retry number $retry (1-based), or null to give up.
     */
    public function delay(int $retry, ?int $retryAfter = null): ?float
    {
        if ($retry < 1 || $retry > $this->maxRetries) {
            return null;
        }
        if ($retryAfter !== null && $retryAfter > $this->maxRetryAfter) {
            return null;
        }
        $cap = min($this->maxDelay, $this->baseDelay * (2 ** ($retry - 1)));
        $r = ($this->random)();
        $r = max(0.0, min(1.0, $r));
        $backoff = $cap / 2 + $r * $cap / 2;

        return $retryAfter !== null ? max((float) $retryAfter, $backoff) : $backoff;
    }
}
