<?php

declare(strict_types=1);

namespace Talksasa\Pay\Webhook;

use Talksasa\Pay\Exception\UnexpectedResponseException;
use Talksasa\Pay\Model\Event;

/**
 * Verifies Talksasa Pay webhook signatures.
 *
 * Canonical algorithm (identical in every Talksasa Pay SDK):
 *
 *  1. Header `Talksasa-Signature: t=<unix seconds>,v1=<hex>[,v1=<hex>...]`.
 *     Split on ",", trim spaces around each item, split each item at the
 *     first "=". Exactly one `t` (1-12 ASCII digits) is required. `v1`
 *     values that are not 64 hex digits are ignored (compared lowercase);
 *     at least one well-formed `v1` is required. Other keys are ignored
 *     (room for future schemes). Anything else is malformed.
 *  2. Reject if |now - t| > tolerance (default 300 s), in either direction.
 *  3. signed_payload = t + "." + raw request body (exact bytes, before any
 *     JSON decoding or re-encoding).
 *  4. expected = lowercase hex(HMAC-SHA256(key = secret string as bytes,
 *     incl. its "whsec_" prefix, message = signed_payload)).
 *  5. Accept if ANY v1 equals expected for ANY configured secret (two v1
 *     values appear for 24 h after a rotation; configure both secrets
 *     while you roll). Compare in constant time.
 *
 * tests/fixtures/webhook_signature_vectors.json holds test vectors.
 */
final class Verifier
{
    public const DEFAULT_TOLERANCE = 300;
    public const HEADER = 'Talksasa-Signature';
    private const MAX_HEADER_LENGTH = 4096;

    /**
     * Verifies the signature and returns the decoded event.
     *
     * @param string $payload the raw request body, byte for byte
     * @param string|list<string> $secrets current (and, during rotation, previous) secret
     * @throws SignatureException not from Talksasa Pay (or replayed/stale): answer 400
     * @throws InvalidPayloadException signed but not a valid event: answer 400
     */
    public static function verify(
        string $payload,
        string $signatureHeader,
        #[\SensitiveParameter] string|array $secrets,
        ?int $now = null,
        int $tolerance = self::DEFAULT_TOLERANCE,
    ): Event {
        self::verifySignature($payload, $signatureHeader, $secrets, $now, $tolerance);

        try {
            return Event::fromJson($payload);
        } catch (UnexpectedResponseException $e) {
            throw new InvalidPayloadException(InvalidPayloadException::INVALID_PAYLOAD, 'Webhook body is not a valid event: ' . $e->getMessage());
        }
    }

    /**
     * Signature and timestamp checks only (no JSON decoding).
     *
     * @param string|list<string> $secrets
     * @throws SignatureException
     */
    public static function verifySignature(
        string $payload,
        string $signatureHeader,
        #[\SensitiveParameter] string|array $secrets,
        ?int $now = null,
        int $tolerance = self::DEFAULT_TOLERANCE,
    ): void {
        $keys = self::secrets($secrets);
        if ($tolerance < 0) {
            throw new \InvalidArgumentException('Tolerance must not be negative.');
        }
        [$t, $signatures] = self::parseHeader($signatureHeader);

        $now ??= time();
        if (abs($now - $t) > $tolerance) {
            throw new SignatureException(
                SignatureException::TIMESTAMP_OUT_OF_TOLERANCE,
                sprintf('Webhook timestamp is %d s away from now (tolerance %d s).', $now - $t, $tolerance),
            );
        }

        $signed = $t . '.' . $payload;
        $ok = false;
        foreach ($keys as $key) {
            $expected = hash_hmac('sha256', $signed, $key);
            foreach ($signatures as $sig) {
                // No early exit: the work done does not depend on which pair matched.
                $ok = hash_equals($expected, $sig) || $ok;
            }
        }
        if (!$ok) {
            throw new SignatureException(SignatureException::NO_MATCHING_SIGNATURE, 'No webhook signature matches.');
        }
    }

    /**
     * Builds a Talksasa-Signature header, e.g. to test your own webhook
     * endpoint. One v1 per secret.
     *
     * @param string|list<string> $secrets
     */
    public static function sign(string $payload, #[\SensitiveParameter] string|array $secrets, ?int $timestamp = null): string
    {
        $t = $timestamp ?? time();
        $parts = ['t=' . $t];
        foreach (self::secrets($secrets) as $key) {
            $parts[] = 'v1=' . hash_hmac('sha256', $t . '.' . $payload, $key);
        }
        return implode(',', $parts);
    }

    /**
     * @param string|array<mixed> $secrets
     * @return list<string>
     */
    private static function secrets(#[\SensitiveParameter] string|array $secrets): array
    {
        $list = [];
        foreach ((array) $secrets as $s) {
            if (is_string($s) && trim($s) !== '') {
                $list[] = trim($s);
            }
        }
        if ($list === []) {
            throw new \InvalidArgumentException('No Talksasa Pay webhook secret configured.');
        }
        return $list;
    }

    /** @return array{0:int,1:list<string>} */
    private static function parseHeader(string $header): array
    {
        $header = trim($header);
        if ($header === '') {
            throw new SignatureException(SignatureException::MISSING_HEADER, 'Talksasa-Signature header is missing.');
        }
        if (strlen($header) > self::MAX_HEADER_LENGTH) {
            throw new SignatureException(SignatureException::MALFORMED_HEADER, 'Talksasa-Signature header is too long.');
        }

        $t = null;
        $signatures = [];
        foreach (explode(',', $header) as $item) {
            $item = trim($item, " \t");
            $eq = strpos($item, '=');
            if ($eq === false || $eq === 0) {
                throw new SignatureException(SignatureException::MALFORMED_HEADER, 'Talksasa-Signature header is malformed.');
            }
            $k = substr($item, 0, $eq);
            $v = substr($item, $eq + 1);
            if ($k === 't') {
                if ($t !== null || preg_match('/^[0-9]{1,12}$/D', $v) !== 1) {
                    throw new SignatureException(SignatureException::MALFORMED_HEADER, 'Talksasa-Signature timestamp is malformed.');
                }
                $t = (int) $v;
            } elseif ($k === 'v1' && preg_match('/^[0-9a-fA-F]{64}$/D', $v) === 1) {
                $signatures[] = strtolower($v);
            }
        }
        if ($t === null || $signatures === []) {
            throw new SignatureException(SignatureException::MALFORMED_HEADER, 'Talksasa-Signature header needs t= and at least one v1=.');
        }
        return [$t, $signatures];
    }
}
