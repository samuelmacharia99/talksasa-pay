<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use DateTimeImmutable;
use DateTimeZone;
use GuzzleHttp\Psr7\HttpFactory;
use Talksasa\Pay\Client;
use Talksasa\Pay\Exception\UnexpectedResponseException;
use Talksasa\Pay\Model\Allocation;
use Talksasa\Pay\Model\CreditChange;
use Talksasa\Pay\Model\PaymentRequest;
use Talksasa\Pay\Model\Transaction;
use Talksasa\Pay\Tests\Support\Fixtures;

final class ClientTest extends ClientTestCase
{
    private function assertCommonHeaders(): void
    {
        $r = $this->http->last();
        self::assertSame('Bearer ' . Fixtures::API_KEY, $r->getHeaderLine('Authorization'));
        self::assertSame('application/json', $r->getHeaderLine('Accept'));
        self::assertStringStartsWith('talksasa-pay-php/' . Client::VERSION, $r->getHeaderLine('User-Agent'));
    }

    public function testWhoami(): void
    {
        $this->http->pushJson(200, ['product_id' => 'p-1', 'product_name' => 'Cloud Hosting', 'account_prefix' => 'HOST', 'key_id' => 'k1a2b3']);
        $me = $this->client()->whoami();

        self::assertSame('GET', $this->http->last()->getMethod());
        self::assertSame(Fixtures::BASE_URL . '/v1/whoami', (string) $this->http->last()->getUri());
        $this->assertCommonHeaders();
        self::assertSame('HOST', $me->accountPrefix);
        self::assertSame('Cloud Hosting', $me->productName);
        self::assertSame('k1a2b3', $me->keyId);
    }

    public function testUpsertPayerSendsFullReplacementAndEncodesRef(): void
    {
        $this->http->pushJson(201, Fixtures::payer());
        $payer = $this->client()->upsertPayer('cust/77 ü', 'Wanjiku Kamau', 'wanjiku@example.com', ['0712345678', '+254722000000']);

        $r = $this->http->last();
        self::assertSame('PUT', $r->getMethod());
        self::assertSame('/v1/payers/cust%2F77%20%C3%BC', $r->getUri()->getPath());
        self::assertSame('application/json', $r->getHeaderLine('Content-Type'));
        self::assertSame(
            ['display_name' => 'Wanjiku Kamau', 'email' => 'wanjiku@example.com', 'phones' => ['0712345678', '+254722000000']],
            $this->http->lastJson(),
        );
        self::assertFalse($r->hasHeader('Idempotency-Key'));
        $this->assertCommonHeaders();

        self::assertSame('HOST-1042', $payer->accountNumber);
        self::assertSame(200, $payer->creditBalance);
        self::assertSame(['2547****5678'], $payer->phones);
        self::assertTrue($payer->isActive());
        self::assertSame('2026-11-02T10:00:00.500000+00:00', $payer->updatedAt->format('Y-m-d\TH:i:s.uP'));
    }

    public function testUpsertPayerDefaultsClearEmailAndPhones(): void
    {
        $this->http->pushJson(200, Fixtures::payer(['email' => null, 'phones' => []]));
        $payer = $this->client()->upsertPayer('cust-77', 'Wanjiku');
        self::assertSame(['display_name' => 'Wanjiku', 'email' => null, 'phones' => []], $this->http->lastJson());
        self::assertNull($payer->email);
    }

    public function testGetPayer(): void
    {
        $this->http->pushJson(200, Fixtures::payer());
        $this->client()->getPayer('cust-77');
        self::assertSame('GET', $this->http->last()->getMethod());
        self::assertSame('/v1/payers/cust-77', $this->http->last()->getUri()->getPath());
        self::assertSame('', (string) $this->http->last()->getBody());
    }

    public function testCreatePaymentRequestGeneratesUuidV4IdempotencyKey(): void
    {
        $this->http->pushJson(201, Fixtures::paymentRequest());
        $pr = $this->client()->createPaymentRequest('cust-77', 'INV-1042', 'Cloud hosting, December 2026', 1500);

        $r = $this->http->last();
        self::assertSame('POST', $r->getMethod());
        self::assertSame('/v1/payment-requests', $r->getUri()->getPath());
        self::assertMatchesRegularExpression('/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/', $r->getHeaderLine('Idempotency-Key'));
        self::assertSame(
            ['payer' => 'cust-77', 'reference' => 'INV-1042', 'description' => 'Cloud hosting, December 2026', 'amount' => 1500],
            $this->http->lastJson(),
        );
        $this->assertCommonHeaders();

        self::assertSame(1300, $pr->outstanding);
        self::assertSame(PaymentRequest::STATUS_PARTIALLY_PAID, $pr->status);
        self::assertTrue($pr->isPayable());
        self::assertSame('https://pay.talksasa.co.ke/tok_abc', $pr->payUrl);
        self::assertSame('4123456', $pr->paybill?->businessNumber);
        self::assertSame('cust-77', $pr->payer->externalRef);
        // Go nanoseconds are truncated to microseconds.
        self::assertSame('2026-11-29T08:00:00.123456', $pr->createdAt->format('Y-m-d\TH:i:s.u'));
        self::assertNull($pr->paidAt);
    }

    public function testCreatePaymentRequestUsesGivenKeyAndExpiry(): void
    {
        $this->http->pushJson(200, Fixtures::paymentRequest(), ['Idempotent-Replayed' => 'true']);
        $expires = new DateTimeImmutable('2026-12-31 23:59:59', new DateTimeZone('Africa/Nairobi'));
        $this->client()->createPaymentRequest(
            payer: 'cust-77',
            reference: 'INV-1042',
            description: 'x',
            amount: 1500,
            expiresAt: $expires,
            idempotencyKey: 'invoice-1042-v1',
        );

        self::assertSame('invoice-1042-v1', $this->http->last()->getHeaderLine('Idempotency-Key'));
        self::assertSame('2026-12-31T20:59:59Z', $this->http->lastJson()['expires_at']);
    }

    public function testCreatePaymentRequestRejectsBadKeyLocally(): void
    {
        $this->expectException(\InvalidArgumentException::class);
        $this->client()->createPaymentRequest('c', 'r', 'd', 1, idempotencyKey: "bad\nkey");
    }

    public function testCustomIdempotencyKeyGenerator(): void
    {
        $this->http->pushJson(201, Fixtures::paymentRequest());
        $this->client(keys: static fn (): string => 'fixed-key')->createPaymentRequest('c', 'r', 'd', 1);
        self::assertSame('fixed-key', $this->http->last()->getHeaderLine('Idempotency-Key'));
    }

    public function testGetAndCancelPaymentRequest(): void
    {
        $this->http->pushJson(200, Fixtures::paymentRequest());
        $this->http->pushJson(200, Fixtures::paymentRequest(['status' => 'cancelled', 'paybill' => null, 'closed_at' => '2026-11-30T00:00:00Z']));
        $c = $this->client();

        $c->getPaymentRequest(Fixtures::REQUEST_ID);
        self::assertSame('GET', $this->http->last()->getMethod());
        self::assertSame('/v1/payment-requests/' . Fixtures::REQUEST_ID, $this->http->last()->getUri()->getPath());

        $pr = $c->cancelPaymentRequest(Fixtures::REQUEST_ID);
        self::assertSame('POST', $this->http->last()->getMethod());
        self::assertSame('/v1/payment-requests/' . Fixtures::REQUEST_ID . '/cancel', $this->http->last()->getUri()->getPath());
        self::assertSame('', (string) $this->http->last()->getBody());
        self::assertSame('cancelled', $pr->status);
        self::assertNull($pr->paybill);
        self::assertFalse($pr->isPayable());
        self::assertNotNull($pr->closedAt);
    }

    public function testListPaymentRequestsQuery(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::paymentRequest()], 'next_cursor' => 'c2']);
        $page = $this->client()->listPaymentRequests(status: 'open', reference: 'INV 1', payer: 'cust-77', cursor: 'c1', limit: 10);

        parse_str($this->http->last()->getUri()->getQuery(), $q);
        self::assertSame(['status' => 'open', 'reference' => 'INV 1', 'payer' => 'cust-77', 'cursor' => 'c1', 'limit' => '10'], $q);
        self::assertStringContainsString('reference=INV%201', $this->http->last()->getUri()->getQuery());
        self::assertCount(1, $page);
        self::assertSame('c2', $page->nextCursor);
        self::assertTrue($page->hasMore());
    }

    public function testListOmitsNullFilters(): void
    {
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null]);
        $page = $this->client()->listPaymentRequests();
        self::assertSame('', $this->http->last()->getUri()->getQuery());
        self::assertFalse($page->hasMore());
    }

    public function testPaymentRequestsGeneratorFollowsCursors(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::paymentRequest(['id' => 'a']), Fixtures::paymentRequest(['id' => 'b'])], 'next_cursor' => 'c2']);
        $this->http->pushJson(200, ['data' => [Fixtures::paymentRequest(['id' => 'c'])], 'next_cursor' => null]);

        $gen = $this->client()->paymentRequests(status: 'open', pageSize: 2);
        self::assertCount(0, $this->http->requests, 'generator is lazy');
        $ids = array_map(static fn (PaymentRequest $p) => $p->id, iterator_to_array($gen, false));

        self::assertSame(['a', 'b', 'c'], $ids);
        self::assertCount(2, $this->http->requests);
        parse_str($this->http->requests[0]->getUri()->getQuery(), $q0);
        parse_str($this->http->requests[1]->getUri()->getQuery(), $q1);
        self::assertSame(['status' => 'open', 'limit' => '2'], $q0);
        self::assertSame(['status' => 'open', 'cursor' => 'c2', 'limit' => '2'], $q1);
    }

    public function testTransactions(): void
    {
        $this->http->pushJson(200, ['data' => [Fixtures::transaction()], 'next_cursor' => 'n']);
        $this->http->pushJson(200, ['data' => [], 'next_cursor' => null]);
        $since = new DateTimeImmutable('2026-11-01T03:00:00+03:00');

        $all = iterator_to_array($this->client()->transactions(payer: 'cust-77', since: $since), false);

        parse_str($this->http->requests[0]->getUri()->getQuery(), $q);
        self::assertSame('/v1/transactions', $this->http->requests[0]->getUri()->getPath());
        self::assertSame(['payer' => 'cust-77', 'since' => '2026-11-01T00:00:00Z'], $q);
        self::assertCount(1, $all);
        $t = $all[0];
        self::assertInstanceOf(Transaction::class, $t);
        self::assertSame(2000, $t->amount);
        self::assertSame('TKR12ABC34', $t->receipt);
        self::assertCount(2, $t->allocations);
        self::assertSame(1300, $t->allocations[0]->amount);
        self::assertTrue($t->allocations[1]->isToCredit());
        self::assertSame(Allocation::KIND_PAYMENT, $t->allocations[1]->kind);
        self::assertSame('2026-11-30T06:15:00+00:00', $t->occurredAt?->format(DATE_ATOM));
    }

    public function testGetTransaction(): void
    {
        $this->http->pushJson(200, Fixtures::transaction(['payer' => null, 'receipt' => null, 'phone' => null, 'occurred_at' => null]));
        $t = $this->client()->getTransaction(Fixtures::TXN_ID);
        self::assertSame('/v1/transactions/' . Fixtures::TXN_ID, $this->http->last()->getUri()->getPath());
        self::assertNull($t->payer);
        self::assertNull($t->receipt);
    }

    public function testListEventsAndTypedData(): void
    {
        $this->http->pushJson(200, [
            'data' => [
                Fixtures::event('payment_request.paid', id: 'e1'),
                Fixtures::event('transaction.allocated', id: 'e2'),
                Fixtures::event('payer.credit_changed', id: 'e3'),
                Fixtures::event('something.new', ['whatever' => 1], 'e4'),
            ],
            'next_cursor' => 'cur-4',
            'has_more' => false,
        ]);
        $page = $this->client()->listEvents(after: 'cur-0', limit: 50);

        parse_str($this->http->last()->getUri()->getQuery(), $q);
        self::assertSame('/v1/events', $this->http->last()->getUri()->getPath());
        self::assertSame(['after' => 'cur-0', 'limit' => '50'], $q);
        self::assertFalse($page->hasMore);
        self::assertSame('cur-4', $page->nextCursor);
        [$e1, $e2, $e3, $e4] = $page->data;
        self::assertTrue($e1->paymentRequest()->isPaid());
        self::assertSame(2000, $e2->transaction()->amount);
        self::assertInstanceOf(CreditChange::class, $e3->data);
        self::assertSame(900, $e3->creditChange()->creditBalance);
        self::assertSame(700, $e3->creditChange()->delta);
        self::assertNull($e4->data);
        self::assertSame(['whatever' => 1], $e4->rawData);
        self::assertSame('2026-11-24', $e1->apiVersion);
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
        $bad = Fixtures::paymentRequest();
        unset($bad['amount']);
        $this->http->pushJson(200, $bad);
        $this->expectException(UnexpectedResponseException::class);
        $this->expectExceptionMessage('"amount"');
        $this->client()->getPaymentRequest('x');
    }

    public function testAmountAsFloatIsRejected(): void
    {
        $this->http->pushJson(200, Fixtures::paymentRequest(['amount' => 1500.5]));
        $this->expectException(UnexpectedResponseException::class);
        $this->client()->getPaymentRequest('x');
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
