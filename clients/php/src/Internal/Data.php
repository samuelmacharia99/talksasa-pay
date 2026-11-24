<?php

declare(strict_types=1);

namespace Talksasa\Pay\Internal;

use DateTimeImmutable;
use DateTimeZone;
use Talksasa\Pay\Exception\UnexpectedResponseException;

/**
 * Strict readers for decoded JSON. A missing or mistyped required field
 * throws instead of silently becoming 0 or "" (this is money). Unknown
 * fields are ignored so new server fields never break old clients.
 *
 * @internal
 */
final class Data
{
    /** @param array<string,mixed> $a */
    public static function str(array $a, string $key): string
    {
        $v = $a[$key] ?? null;
        if (!is_string($v)) {
            throw self::bad($key, 'string');
        }
        return $v;
    }

    /** @param array<string,mixed> $a */
    public static function nullableStr(array $a, string $key): ?string
    {
        $v = $a[$key] ?? null;
        if ($v !== null && !is_string($v)) {
            throw self::bad($key, 'string or null');
        }
        return $v;
    }

    /** @param array<string,mixed> $a */
    public static function int(array $a, string $key): int
    {
        $v = $a[$key] ?? null;
        if (!is_int($v)) {
            throw self::bad($key, 'integer');
        }
        return $v;
    }

    /** @param array<string,mixed> $a */
    public static function bool(array $a, string $key): bool
    {
        $v = $a[$key] ?? null;
        if (!is_bool($v)) {
            throw self::bad($key, 'boolean');
        }
        return $v;
    }

    /**
     * @param array<string,mixed> $a
     * @return array<string,mixed>
     */
    public static function obj(array $a, string $key): array
    {
        $v = $a[$key] ?? null;
        if (!is_array($v) || ($v !== [] && array_is_list($v))) {
            throw self::bad($key, 'object');
        }
        return $v;
    }

    /**
     * @param array<string,mixed> $a
     * @return array<string,mixed>|null
     */
    public static function nullableObj(array $a, string $key): ?array
    {
        if (($a[$key] ?? null) === null) {
            return null;
        }
        return self::obj($a, $key);
    }

    /**
     * @param array<string,mixed> $a
     * @return list<mixed>
     */
    public static function list(array $a, string $key): array
    {
        $v = $a[$key] ?? null;
        if (!is_array($v) || !array_is_list($v)) {
            throw self::bad($key, 'array');
        }
        return $v;
    }

    /** @param array<string,mixed> $a */
    public static function date(array $a, string $key): DateTimeImmutable
    {
        return self::parseDate(self::str($a, $key), $key);
    }

    /** @param array<string,mixed> $a */
    public static function nullableDate(array $a, string $key): ?DateTimeImmutable
    {
        $s = self::nullableStr($a, $key);
        return $s === null ? null : self::parseDate($s, $key);
    }

    /**
     * Parses RFC 3339 as emitted by Go (up to nanosecond fractions) into a
     * UTC DateTimeImmutable (PHP keeps microseconds).
     */
    public static function parseDate(string $s, string $key = 'date'): DateTimeImmutable
    {
        if (!preg_match('/^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/i', $s, $m)) {
            throw new UnexpectedResponseException(sprintf('Field "%s" is not an RFC 3339 date-time.', $key));
        }
        $frac = $m[2] !== '' ? '.' . substr(str_pad($m[2], 6, '0'), 0, 6) : '.000000';
        $tz = strtoupper($m[3]) === 'Z' ? '+00:00' : $m[3];
        $d = DateTimeImmutable::createFromFormat('Y-m-d\TH:i:s.uP', $m[1] . $frac . $tz);
        if ($d === false) {
            throw new UnexpectedResponseException(sprintf('Field "%s" is not a valid date-time.', $key));
        }
        return $d->setTimezone(new DateTimeZone('UTC'));
    }

    private static function bad(string $key, string $want): UnexpectedResponseException
    {
        return new UnexpectedResponseException(sprintf('Expected field "%s" to be %s.', $key, $want));
    }
}
