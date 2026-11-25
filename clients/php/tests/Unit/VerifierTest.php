<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\TestCase;
use Talksasa\Pay\Model\EventType;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Webhook\InvalidPayloadException;
use Talksasa\Pay\Webhook\SignatureException;
use Talksasa\Pay\Webhook\Verifier;

final class VerifierTest extends TestCase
{
    private const SECRET = 'whsec_test_secret_value';
    private const OLD = 'whsec_previous_secret';
    private const T = 1_800_000_000;

    /** @return array<string,mixed> */
    private static function vectors(): array
    {
        return json_decode((string) file_get_contents(__DIR__ . '/../fixtures/webhook_signature_vectors.json'), true, 64, JSON_THROW_ON_ERROR);
    }

    /** @return iterable<string,array{array<string,mixed>}> */
    public static function fixtureVectors(): iterable
    {
        foreach (self::vectors()['vectors'] as $v) {
            yield $v['name'] => [$v];
        }
    }

    /** @param array<string,mixed> $v */
    #[DataProvider('fixtureVectors')]
    public function testFixtureVector(array $v): void
    {
        try {
            $event = Verifier::verify($v['payload'], $v['header'], $v['secrets'], $v['now'], $v['tolerance']);
            self::assertSame('valid', $v['expected'], 'verified but expected ' . $v['expected']);
            // The shared vectors are frozen bytes from before the charges
            // API: their type is unknown to this version, so data is null and
            // the raw JSON is kept. Signature checks never look at the type.
            self::assertSame('0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b', $event->id);
            self::assertNotContains($event->type, EventType::ALL);
            self::assertNull($event->data);
            self::assertNotSame([], $event->rawData);
        } catch (SignatureException $e) {
            self::assertSame($v['expected'], $e->reason);
        }
    }

    public function testSigningExampleMatchesFixture(): void
    {
        $ex = self::vectors()['signing_example'];
        self::assertSame($ex['header'], Verifier::sign($ex['payload'], $ex['secret'], $ex['timestamp']));
        self::assertSame($ex['v1'], hash_hmac('sha256', $ex['signed_payload'], $ex['secret']));
    }

    private static function body(): string
    {
        return json_encode(Fixtures::event(), JSON_THROW_ON_ERROR);
    }

    public function testRoundTripReturnsEvent(): void
    {
        $body = self::body();
        $event = Verifier::verify($body, Verifier::sign($body, self::SECRET, self::T), self::SECRET, self::T);
        self::assertSame('0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b', $event->id);
        self::assertSame(1500, $event->charge()->amount);
        self::assertTrue($event->charge()->isSucceeded());
    }

    /** @return iterable<string,array{string,list<string>|string,int,string}> */
    public static function table(): iterable
    {
        $body = self::body();
        $sig = hash_hmac('sha256', self::T . '.' . $body, self::SECRET);
        $old = hash_hmac('sha256', self::T . '.' . $body, self::OLD);
        $t = self::T;

        yield 'valid' => ["t=$t,v1=$sig", self::SECRET, $t, 'valid'];
        yield 'valid, order v1 first' => ["v1=$sig,t=$t", self::SECRET, $t, 'valid'];
        yield 'rotation: either header signature' => ["t=$t,v1=$old,v1=$sig", self::SECRET, $t, 'valid'];
        yield 'rotation: either configured secret' => ["t=$t,v1=$old", [self::SECRET, self::OLD], $t, 'valid'];
        yield 'empty secret entries skipped' => ["t=$t,v1=$sig", ['', self::SECRET], $t, 'valid'];
        yield 'secret whitespace trimmed' => ["t=$t,v1=$sig", ' ' . self::SECRET . "\n", $t, 'valid'];
        yield 'stale 301' => ["t=$t,v1=$sig", self::SECRET, $t + 301, SignatureException::TIMESTAMP_OUT_OF_TOLERANCE];
        yield 'future 301' => ["t=$t,v1=$sig", self::SECRET, $t - 301, SignatureException::TIMESTAMP_OUT_OF_TOLERANCE];
        yield 'edge 300' => ["t=$t,v1=$sig", self::SECRET, $t + 300, 'valid'];
        yield 'wrong secret' => ["t=$t,v1=$sig", 'whsec_other', $t, SignatureException::NO_MATCHING_SIGNATURE];
        yield 'missing' => ['', self::SECRET, $t, SignatureException::MISSING_HEADER];
        yield 'blank' => ['   ', self::SECRET, $t, SignatureException::MISSING_HEADER];
        yield 'garbage' => ['garbage', self::SECRET, $t, SignatureException::MALFORMED_HEADER];
        yield 'empty key' => ["=1,t=$t,v1=$sig", self::SECRET, $t, SignatureException::MALFORMED_HEADER];
        yield 'negative t' => ["t=-1,v1=$sig", self::SECRET, $t, SignatureException::MALFORMED_HEADER];
        yield 't too long' => ['t=1234567890123,v1=' . $sig, self::SECRET, $t, SignatureException::MALFORMED_HEADER];
        yield 'too long header' => ["t=$t,v1=$sig," . str_repeat('x=y,', 1100) . 'z=1', self::SECRET, $t, SignatureException::MALFORMED_HEADER];
        yield 'sha1-length v1 only' => ["t=$t,v1=" . sha1('x'), self::SECRET, $t, SignatureException::MALFORMED_HEADER];
    }

    /** @param list<string>|string $secrets */
    #[DataProvider('table')]
    public function testTable(string $header, array|string $secrets, int $now, string $expected): void
    {
        try {
            Verifier::verify(self::body(), $header, $secrets, $now);
            self::assertSame('valid', $expected);
        } catch (SignatureException $e) {
            self::assertSame($expected, $e->reason);
            self::assertStringNotContainsString(is_array($secrets) ? implode('', $secrets) : trim($secrets), $e->getMessage());
        }
    }

    public function testCustomTolerance(): void
    {
        $body = self::body();
        $h = Verifier::sign($body, self::SECRET, self::T);
        Verifier::verify($body, $h, self::SECRET, self::T + 600, 600);
        $this->expectException(SignatureException::class);
        Verifier::verify($body, $h, self::SECRET, self::T + 61, 60);
    }

    public function testDefaultNowIsCurrentTime(): void
    {
        $body = self::body();
        Verifier::verify($body, Verifier::sign($body, self::SECRET), self::SECRET);
        $this->expectException(SignatureException::class);
        Verifier::verify($body, Verifier::sign($body, self::SECRET, time() - 3600), self::SECRET);
    }

    public function testNoSecretsIsAConfigurationError(): void
    {
        $this->expectException(\InvalidArgumentException::class);
        Verifier::verify(self::body(), 't=1,v1=' . str_repeat('a', 64), ['', ' '], 1);
    }

    public function testSignedButInvalidPayload(): void
    {
        $body = '{"not":"an event"}';
        try {
            Verifier::verify($body, Verifier::sign($body, self::SECRET, self::T), self::SECRET, self::T);
            self::fail('expected exception');
        } catch (InvalidPayloadException $e) {
            self::assertSame(InvalidPayloadException::INVALID_PAYLOAD, $e->reason);
        }
    }

    public function testVerifySignatureDoesNotDecode(): void
    {
        $body = 'not json';
        Verifier::verifySignature($body, Verifier::sign($body, self::SECRET, self::T), self::SECRET, self::T);
        $this->addToAssertionCount(1);
    }

    public function testSignWithTwoSecretsEmitsTwoV1(): void
    {
        $h = Verifier::sign('x', [self::SECRET, self::OLD], 5);
        self::assertMatchesRegularExpression('/^t=5,v1=[0-9a-f]{64},v1=[0-9a-f]{64}$/', $h);
    }
}
