<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use DateTimeImmutable;
use GuzzleHttp\Psr7\HttpFactory;
use Talksasa\Pay\Client;
use Talksasa\Pay\Exception\UnexpectedResponseException;
use Talksasa\Pay\Model\Charge;
use Talksasa\Pay\Model\PaymentReceived;
use Talksasa\Pay\Model\StkPush;
use Talksasa\Pay\Model\Transaction;
use Talksasa\Pay\Tests\Support\Fixtures;

final class ClientTest extends ClientTestCase
{
    private const UUID4 = '/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/';

    private function assertCommonHeaders(): void
    {
        $r = $this->http->last();
        self::assertSame('Bearer ' . Fixtures::API_KEY, $r->getHeaderLine('Authorization'));
        self::assertSame('application/json', $r->getHeaderLine('Accept'));
        self::assertStringStartsWith('talksasa-pay-php/' . Client::VERSION, $r->getHeaderLine('User-Agent'));
    }

    public function testWhoami(): void
    {
        $this->http->pushJson(200, Fixtures::whoami());
        $me = $this->client()->whoami();

        self::assertSame('GET', $this->http->last()->getMethod());
        self::assertSame(Fixtures::BASE_URL . '/v1/whoami', (string) $this->http->last()->getUri());
        $this->assertCommonHeaders();
        self::assertSame('HOST', $me->accountPrefix);
        self::assertSame('Cloud Hosting', $me->appName);
        self::assertSame('0193a1b2-0000-7000-8000-0000000000a1', $me->appId);
        self::assertSame('k1a2b3', $me->keyId);
    }

    public function testCreateChargeMinimalGeneratesUuidV4IdempotencyKey(): void
    {
        $this->http->pushJson(201, Fixtures::charge());
        $charge = $this->client()->createCharge(1500, 'INV-1042');

        $r = $this->http->last();
        self::assertSame('POST', $r->getMethod());
        self::assertSame('/v1/charges', $r->getUri()->getPath());
        self::assertSame('application/json', $r->getHeaderLine('Content-Type'));
        self::assertMatchesRegularExpression(self::UUID4, $r->getHeaderLine('Idempotency-Key'));
        self::assertSame(['reference' => 'INV-1042', 'amount' => 1500], $this->http->lastJson());
        $this->assertCommonHeaders();

        self::assertSame(Fixtures::CHARGE_ID, $charge->id);
        self::assertSame(1500, $charge->amount);
        self::assertSame('KES', $charge->currency);
        self::assertSame(Charge::STATUS_PENDING, $charge->status);
        self::assertTrue($charge->isPending());
        self::assertFalse($charge->isSucceeded());
        self::assertFalse($charge->needsReview());
        self::assertSame('HOST-4K7QZ9M', $charge->accountNumber);
        self::assertSame('https://pay.talksasa.co.ke/c/tok_abc', $charge->checkoutUrl);
        self::assertSame('4123456', $charge->paybill?->businessNumber);
        self::assertSame('HOST-4K7QZ9M', $charge->paybill?->accountNumber);
        self::assertSame(['invoice_id' => 42, 'customer' => 'cust-77'], $charge->metadata);
        self::assertNull($charge->latestStkPush);
        self::assertNull($charge->transactionId);
        self::assertNull($charge->succeededAt);
        // Go nanoseconds are truncated to microseconds.
        self::assertSame('2026-11-29T08:00:00.123456', $charge->createdAt->format('Y-m-d\TH:i:s.u'));
        self::assertSame('2026-11-30T08:00:00+00:00', $charge->expiresAt->format(DATE_ATOM));
    }

    public function testCreateChargeWithEverything(): void
    {
        $this->http->pushJson(201, Fixtures::charge([
            'account_number' => 'HOST-1042',
            'latest_stk_push' => Fixtures::stkPush(),
        ]));
        $charge = $this->client()->createCharge(
            amount: 1500,
            reference: 'INV-1042',
            description: 'Cloud hosting, December 2026',
            phone: ' 0712345678 ',
            accountNumber: 'HOST-1042',
            metadata: ['invoice_id' => 42],
            expiresIn: 3600,
            idempotencyKey: 'invoice-42-charge-1',
        );

        self::assertSame('invoice-42-charge-1', $this->http->last()->getHeaderLine('Idempotency-Key'));
        self::assertSame([
            'reference' => 'INV-1042',
            'amount' => 1500,
            'description' => 'Cloud hosting, December 2026',
            'phone' => '0712345678',
            'account_number' => 'HOST-1042',
            'metadata' => ['invoice_id' => 42],
            'expires_in' => 3600,
        ], $this->http->lastJson());
        self::assertInstanceOf(StkPush::class, $charge->latestStkPush);
        self::assertSame(StkPush::STATUS_SENT, $charge->latestStkPush->status);
        self::assertSame(Fixtures::CHARGE_ID, $charge->latestStkPush->chargeId);
    }

    public function testCreateChargeEmptyMetadataIsAnObject(): void
    {
        $this->http->pushJson(201, Fixtures::charge(['metadata' => null, 'description' => null]));
        $charge = $this->client()->createCharge(10, 'r', metadata: []);
        self::assertSame('{"reference":"r","amount":10,"metadata":{}}', (string) $this->http->last()->getBody());
        self::assertNull($charge->metadata);
        self::assertNull($charge->description);
    }

    public function testCreateChargeRejectsBadInputLocally(): void
    {
        $cases = [
            static fn (Client $c) => $c->createCharge(1, 'r', metadata: ['a', 'b']),
            static fn (Client $c) => $c->createCharge(1, 'r', phone: '  '),
            static fn (Client $c) => $c->createCharge(1, 'r', idempotencyKey: "bad\nkey"),
            static fn (Client $c) => $c->createCharge(1, 'r', idempotencyKey: str_repeat('k', 256)),
        ];
        foreach ($cases as $i => $call) {
            try {
                $call($this->client());
                self::fail("case {$i}: expected InvalidArgumentException");
            } catch (\InvalidArgumentException) {
            }
        }
        self::assertSame([], $this->http->requests);
    }

    public function testEachCreateGetsItsOwnKey(): void
    {
        $this->http->pushJson(201, Fixtures::charge())->pushJson(201, Fixtures::charge());
        $c = $this->client();
        $c->createCharge(1, 'a');
        $c->createCharge(1, 'b');
        self::assertNotSame($this->http->requests[0]->getHeaderLine('Idempotency-Key'), $this->http->requests[1]->getHeaderLine('Idempotency-Key'));
    }

    public function testCustomIdempotencyKeyGenerator(): void
    {
        $this->http->pushJson(201, Fixtures::charge());
        $this->client(keys: static fn (): string => 'fixed-key')->createCharge(1, 'r');
        self::assertSame('fixed-key', $this->http->last()->getHeaderLine('Idempotency-Key'));
    }

    public function testGetAndCancelCharge(): void
    {
        $this->http->pushJson(200, Fixtures::succeededCharge());
        $this->http->pushJson(200, Fixtures::charge(['status' => 'cancelled', 'paybill' => null]));
        $c = $this->client();

        $got = $c->getCharge(Fixtures::CHARGE_ID);
        self::assertSame('GET', $this->http->last()->getMethod());
        self::assertSame('/v1/charges/' . Fixtures::CHARGE_ID, $this->http->last()->getUri()->getPath());
        self::assertTrue($got->isSucceeded());
        self::assertSame(Fixtures::TXN_ID, $got->transactionId);
        self::assertNotNull($got->succeededAt);
        self::assertTrue($got->latestStkPush?->isSucceeded());

        $cancelled = $c->cancelCharge('a/b');
        self::assertSame('POST', $this->http->last()->getMethod());
        self::assertSame('/v1/charges/a%2Fb/cancel', $this->http->last()->getUri()->getPath());
        self::assertSame('', (string) $this->http->last()->getBody());
        self::assertFalse($this->http->last()->hasHeader('Idempotency-Key'));
        self::assertSame(Charge::STATUS_CANCELLED, $cancelled->status);
        self::assertNull($cancelled->paybill);
        self::assertFalse($cancelled->isPending());
    }

    public function testEmptyIdIsRejectedLocally(): void
    {
        $this->expectException(\InvalidArgumentException::class);
        try {
            $this->client()->getCharge('');
        } finally {
            self::assertSame([], $this->http->requests);
        }
    }

    public function testListChargesQuery(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::charge()], 'next_cursor' => 'c2']);
        $page = $this->client()->listCharges(status: 'pending', reference: 'INV 1', cursor: 'c1', limit: 10);

        self::assertSame('/v1/charges', $this->http->last()->getUri()->getPath());
        parse_str($this->http->last()->getUri()->getQuery(), $q);
        self::assertSame(['status' => 'pending', 'reference' => 'INV 1', 'cursor' => 'c1', 'limit' => '10'], $q);
        self::assertStringContainsString('reference=INV%201', $this->http->last()->getUri()->getQuery());
        self::assertCount(1, $page);
        self::assertInstanceOf(Charge::class, $page->data[0]);
        self::assertSame('c2', $page->nextCursor);
        self::assertTrue($page->hasMore());
    }

    public function testListOmitsNullFilters(): void
    {
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null]);
        $page = $this->client()->listCharges();
        self::assertSame('', $this->http->last()->getUri()->getQuery());
        self::assertFalse($page->hasMore());
    }

    public function testChargesGeneratorFollowsCursors(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::charge(['id' => 'a']), Fixtures::charge(['id' => 'b'])], 'next_cursor' => 'c2']);
        $this->http->pushJson(200, ['data' => [Fixtures::charge(['id' => 'c'])], 'next_cursor' => null]);

        $gen = $this->client()->charges(status: 'pending', pageSize: 2);
        self::assertCount(0, $this->http->requests, 'generator is lazy');
        $ids = array_map(static fn (Charge $c) => $c->id, iterator_to_array($gen, false));

        self::assertSame(['a', 'b', 'c'], $ids);
        self::assertCount(2, $this->http->requests);
        parse_str($this->http->requests[0]->getUri()->getQuery(), $q0);
        parse_str($this->http->requests[1]->getUri()->getQuery(), $q1);
        self::assertSame(['status' => 'pending', 'limit' => '2'], $q0);
        self::assertSame(['status' => 'pending', 'cursor' => 'c2', 'limit' => '2'], $q1);
    }

    public function testChargesGeneratorStopsOnRepeatedCursor(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::charge(['id' => 'a'])], 'next_cursor' => 'same']);
        $this->http->pushJson(200, ['data' => [Fixtures::charge(['id' => 'b'])], 'next_cursor' => 'same']);
        $ids = [];
        try {
            foreach ($this->client()->charges() as $c) {
                $ids[] = $c->id;
            }
            self::fail('expected exception');
        } catch (UnexpectedResponseException $e) {
            self::assertStringContainsString('repeated', $e->getMessage());
        }
        self::assertSame(['a', 'b'], $ids);
        self::assertCount(2, $this->http->requests);
    }

    public function testTransactionsFilterByCharge(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::transaction()], 'next_cursor' => 'n']);
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null]);
        $since = new DateTimeImmutable('2026-11-01T03:00:00+03:00');

        $all = iterator_to_array($this->client()->transactions(chargeId: Fixtures::CHARGE_ID, since: $since), false);

        parse_str($this->http->requests[0]->getUri()->getQuery(), $q);
        self::assertSame('/v1/transactions', $this->http->requests[0]->getUri()->getPath());
        self::assertSame(['charge_id' => Fixtures::CHARGE_ID, 'since' => '2026-11-01T00:00:00Z'], $q);
        self::assertCount(1, $all);
        $t = $all[0];
        self::assertInstanceOf(Transaction::class, $t);
        self::assertSame(1500, $t->amount);
        self::assertSame('TKR12ABC34', $t->receipt);
        self::assertSame(Fixtures::CHARGE_ID, $t->chargeId);
        self::assertSame('HOST-4K7QZ9M', $t->accountNumber);
        self::assertFalse($t->isReversal());
        self::assertSame('2026-11-30T06:15:00+00:00', $t->occurredAt?->format(DATE_ATOM));
    }

    public function testListTransactionsPage(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::reversal()], 'next_cursor' => null]);
        $page = $this->client()->listTransactions(cursor: 'c1', limit: 5);
        parse_str($this->http->last()->getUri()->getQuery(), $q);
        self::assertSame(['cursor' => 'c1', 'limit' => '5'], $q);
        self::assertTrue($page->data[0]->isReversal());
        self::assertSame(Fixtures::TXN_ID, $page->data[0]->reversesTransactionId);
    }

    public function testGetTransaction(): void
    {
        $this->http->pushJson(200, Fixtures::transaction(['charge_id' => null, 'receipt' => null, 'phone' => null, 'account_number' => null, 'occurred_at' => null]));
        $t = $this->client()->getTransaction(Fixtures::TXN_ID);
        self::assertSame('/v1/transactions/' . Fixtures::TXN_ID, $this->http->last()->getUri()->getPath());
        self::assertNull($t->chargeId);
        self::assertNull($t->receipt);
        self::assertNull($t->accountNumber);
    }

    public function testListEventsAndTypedData(): void
    {
        $this->http->pushJson(200, [
            'data' => [
                Fixtures::event('charge.succeeded', id: 'e1'),
                Fixtures::event('stk_push.failed', id: 'e2'),
                Fixtures::event('payment.received', Fixtures::paymentReceived(['duplicate_of_charge' => Fixtures::CHARGE_ID]), 'e3'),
                Fixtures::event('transaction.reversed', id: 'e4'),
                Fixtures::event('something.new', ['whatever' => 1], 'e5'),
            ],
            'next_cursor' => 'cur-5',
            'has_more' => false,
        ]);
        $page = $this->client()->listEvents(after: 'cur-0', limit: 50);

        parse_str($this->http->last()->getUri()->getQuery(), $q);
        self::assertSame('/v1/events', $this->http->last()->getUri()->getPath());
        self::assertSame(['after' => 'cur-0', 'limit' => '50'], $q);
        self::assertFalse($page->hasMore);
        self::assertSame('cur-5', $page->nextCursor);
        [$e1, $e2, $e3, $e4, $e5] = $page->data;
        self::assertTrue($e1->charge()->isSucceeded());
        self::assertSame('2026-11-25', $e1->apiVersion);
        self::assertTrue($e2->stkPush()->isFailed());
        self::assertInstanceOf(PaymentReceived::class, $e3->data);
        self::assertTrue($e3->paymentReceived()->isDuplicate());
        self::assertSame(Fixtures::CHARGE_ID, $e3->paymentReceived()->duplicateOfCharge);
        self::assertSame(700, $e3->transaction()->amount);
        self::assertTrue($e4->transaction()->isReversal());
        self::assertNotInstanceOf(PaymentReceived::class, $e4->data);
        self::assertTrue($e4->isKnownType());
        self::assertNull($e5->data);
        self::assertFalse($e5->isKnownType());
        self::assertSame(['whatever' => 1], $e5->rawData);
    }

    public function testEventsGeneratorDrainsFeed(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e1')], 'next_cursor' => 'c1', 'has_more' => true]);
        $this->http->pushJson(200, ['data' => [Fixtures::event(id: 'e2')], 'next_cursor' => 'c2', 'has_more' => false]);

        $seen = [];
        foreach ($this->client()->events() as $cursor => $event) {
            $seen[] = [$cursor, $event->id];
        }
        self::assertSame([['c1', 'e1'], ['c2', 'e2']], $seen);
        self::assertSame('', $this->http->requests[0]->getUri()->getQuery());
        self::assertSame('after=c1', $this->http->requests[1]->getUri()->getQuery());
    }

    public function testMissingRequiredFieldThrows(): void
    {
        $bad = Fixtures::charge();
        unset($bad['amount']);
        $this->http->pushJson(200, $bad);
        $this->expectException(UnexpectedResponseException::class);
        $this->expectExceptionMessage('"amount"');
        $this->client()->getCharge('x');
    }

    public function testAmountAsFloatIsRejected(): void
    {
        $this->http->pushJson(200, Fixtures::charge(['amount' => 1500.5]));
        $this->expectException(UnexpectedResponseException::class);
        $this->client()->getCharge('x');
    }

    public function testNonJsonSuccessBodyThrows(): void
    {
        $this->http->push(new \GuzzleHttp\Psr7\Response(200, [], '<html>'));
        $this->expectException(UnexpectedResponseException::class);
        $this->client()->whoami();
    }

    public function testBaseUrlNormalisation(): void
    {
        $f = new HttpFactory();
        $mk = fn (string $u, bool $insecure = false) => (new Client(Fixtures::API_KEY, $u, $this->http, $f, $f, allowInsecureHttp: $insecure))->baseUrl();

        self::assertSame('https://a.example/v1', $mk('https://a.example'));
        self::assertSame('https://a.example/v1', $mk('https://a.example/'));
        self::assertSame('https://a.example/v1', $mk('https://a.example/v1/'));
        self::assertSame('https://a.example/pay/v1', $mk('https://a.example/pay'));
        self::assertSame('http://localhost:8088/v1', $mk('http://localhost:8088'));
        self::assertSame('http://127.0.0.1:8088/v1', $mk('http://127.0.0.1:8088'));
        self::assertSame('http://api:8088/v1', $mk('http://api:8088', true));
    }

    public function testPlainHttpToRemoteHostRefused(): void
    {
        $f = new HttpFactory();
        $this->expectException(\InvalidArgumentException::class);
        new Client(Fixtures::API_KEY, 'http://pay-api.example.com', $this->http, $f, $f);
    }

    public function testEmptyApiKeyRefused(): void
    {
        $f = new HttpFactory();
        $this->expectException(\InvalidArgumentException::class);
        new Client('', Fixtures::BASE_URL, $this->http, $f, $f);
    }
}
