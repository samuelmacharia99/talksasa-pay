<?php

declare(strict_types=1);

namespace Talksasa\Pay;

use DateTimeInterface;
use DateTimeZone;
use Generator;
use Http\Discovery\Psr17FactoryDiscovery;
use Http\Discovery\Psr18ClientDiscovery;
use Psr\Http\Client\ClientExceptionInterface;
use Psr\Http\Client\ClientInterface;
use Psr\Http\Client\NetworkExceptionInterface;
use Psr\Http\Message\RequestFactoryInterface;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\StreamFactoryInterface;
use Talksasa\Pay\Exception\ApiException;
use Talksasa\Pay\Exception\MpesaErrorException;
use Talksasa\Pay\Exception\MpesaUnavailableException;
use Talksasa\Pay\Exception\TransportException;
use Talksasa\Pay\Exception\UnexpectedResponseException;
use Talksasa\Pay\Http\RetryPolicy;
use Talksasa\Pay\Http\Secret;
use Talksasa\Pay\Http\Sleeper;
use Talksasa\Pay\Http\SystemSleeper;
use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\EventPage;
use Talksasa\Pay\Model\Page;
use Talksasa\Pay\Model\Payer;
use Talksasa\Pay\Model\PaymentRequest;
use Talksasa\Pay\Model\StkPush;
use Talksasa\Pay\Model\Transaction;
use Talksasa\Pay\Model\WhoAmI;

/**
 * Talksasa Pay product API (/v1) client.
 *
 * HTTP goes through any PSR-18 client. When none is given, Guzzle 7 is used
 * if installed (so the timeouts apply), else php-http/discovery finds one.
 *
 * Not final so applications can mock it in their own tests.
 */
class Client
{
    public const VERSION = '0.1.0';

    private readonly Secret $apiKey;
    private readonly string $baseUrl;
    private readonly ClientInterface $http;
    private readonly RequestFactoryInterface $requestFactory;
    private readonly StreamFactoryInterface $streamFactory;
    private readonly RetryPolicy $retryPolicy;
    private readonly Sleeper $sleeper;
    /** @var \Closure(): string */
    private readonly \Closure $idempotencyKeys;

    /**
     * @param string $apiKey tsp_live_... / tsp_test_... (never logged)
     * @param string $baseUrl e.g. https://pay-api.example (with or without /v1)
     * @param float $timeout total seconds per attempt (only when the client builds Guzzle itself)
     * @param bool $allowInsecureHttp permit http:// to a non-loopback host (development only)
     * @param (callable(): string)|null $idempotencyKeyGenerator defaults to random UUIDv4
     */
    public function __construct(
        #[\SensitiveParameter] string $apiKey,
        string $baseUrl,
        ?ClientInterface $httpClient = null,
        ?RequestFactoryInterface $requestFactory = null,
        ?StreamFactoryInterface $streamFactory = null,
        ?RetryPolicy $retryPolicy = null,
        ?Sleeper $sleeper = null,
        float $timeout = 15.0,
        float $connectTimeout = 5.0,
        bool $allowInsecureHttp = false,
        ?callable $idempotencyKeyGenerator = null,
    ) {
        if ($apiKey === '' || preg_match('/[\s\x00-\x1f]/', $apiKey) === 1) {
            throw new \InvalidArgumentException('Talksasa Pay API key is empty or contains whitespace.');
        }
        $this->apiKey = new Secret($apiKey);
        $this->baseUrl = self::normalizeBaseUrl($baseUrl, $allowInsecureHttp);
        $this->http = $httpClient ?? self::defaultHttpClient($timeout, $connectTimeout);
        $this->requestFactory = $requestFactory ?? Psr17FactoryDiscovery::findRequestFactory();
        $this->streamFactory = $streamFactory ?? Psr17FactoryDiscovery::findStreamFactory();
        $this->retryPolicy = $retryPolicy ?? new RetryPolicy();
        $this->sleeper = $sleeper ?? new SystemSleeper();
        $this->idempotencyKeys = $idempotencyKeyGenerator !== null
            ? \Closure::fromCallable($idempotencyKeyGenerator)
            : self::uuid4(...);
    }

    // ---------------------------------------------------------------- API

    /** The product this key belongs to. Handy as a health/credentials check. */
    public function whoami(): WhoAmI
    {
        return WhoAmI::fromArray($this->request('GET', '/whoami'));
    }

    /**
     * Creates or updates a payer (your customer), keyed by your own id.
     * This is a full replacement (PUT): send the current email and the full
     * phone list every time; omitted values are cleared. Creating a payer
     * assigns its permanent M-Pesa account number.
     *
     * @param list<string> $phones Kenyan mobiles in any common format, first = primary, max 5
     */
    public function upsertPayer(string $externalRef, string $displayName, ?string $email = null, array $phones = []): Payer
    {
        return Payer::fromArray($this->request('PUT', '/payers/' . self::seg($externalRef), body: [
            'display_name' => $displayName,
            'email' => $email,
            'phones' => array_values($phones),
        ]));
    }

    public function getPayer(string $externalRef): Payer
    {
        return Payer::fromArray($this->request('GET', '/payers/' . self::seg($externalRef)));
    }

    /**
     * Asks a payer to pay $amount KES. Any credit the payer holds is applied
     * at once, so the result may already be partially_paid or paid.
     *
     * Pass a stable $idempotencyKey derived from your invoice (e.g.
     * "invoice-123-v1") so a retry after a crash or timeout returns the same
     * request. Without one a random key is generated, which only protects
     * the automatic retries inside this call.
     *
     * @throws Exception\DuplicateReferenceException an open request with this reference exists
     * @throws Exception\IdempotencyConflictException key reused with a different body
     */
    public function createPaymentRequest(
        string $payer,
        string $reference,
        string $description,
        int $amount,
        ?DateTimeInterface $expiresAt = null,
        ?string $idempotencyKey = null,
    ): PaymentRequest {
        $body = [
            'payer' => $payer,
            'reference' => $reference,
            'description' => $description,
            'amount' => $amount,
        ];
        if ($expiresAt !== null) {
            $body['expires_at'] = self::formatDate($expiresAt);
        }
        $key = $this->idempotencyKey($idempotencyKey);

        return PaymentRequest::fromArray($this->request('POST', '/payment-requests', body: $body, headers: [
            'Idempotency-Key' => $key,
        ]));
    }

    public function getPaymentRequest(string $id): PaymentRequest
    {
        return PaymentRequest::fromArray($this->request('GET', '/payment-requests/' . self::seg($id)));
    }

    /**
     * Cancels an open request on which nothing has been paid. Idempotent.
     *
     * @throws Exception\NotCancellableException money has been paid on it
     */
    public function cancelPaymentRequest(string $id): PaymentRequest
    {
        return PaymentRequest::fromArray($this->request('POST', '/payment-requests/' . self::seg($id) . '/cancel'));
    }

    /**
     * Sends an M-Pesa payment prompt (STK push) to $phone for the request's
     * full outstanding balance. Returns as soon as Safaricom has the prompt
     * (status sent) or Talksasa is still sending it (initiating).
     *
     * The prompt's outcome arrives as events: stk_push.succeeded together
     * with payment_request.paid, or stk_push.failed ($resultDesc says why,
     * e.g. cancelled by the customer). Mark the invoice paid ONLY on
     * payment_request.paid, never on stk_push.sent or stk_push.needs_review.
     *
     * Always sends an Idempotency-Key, and every automatic retry (network
     * error, timeout, 429, 503 overloaded/timeout) re-sends the SAME key, so
     * the customer never gets two prompts from one call. Pass your own key
     * (e.g. "invoice-{$id}-stk-{$attempt}") to make a retry after a crash
     * safe too; reuse a key only for the same phone.
     *
     * @param string $phone Safaricom number: 07XXXXXXXX, 01XXXXXXXX, 2547XXXXXXXX or +2547XXXXXXXX
     * @throws Exception\StkInProgressException a prompt for this request is already on a phone
     * @throws Exception\NotPayableException the request is paid, cancelled or expired
     * @throws Exception\AmountTooLargeException balance above KES 250,000: use the paybill
     * @throws Exception\ValidationException not a valid Safaricom number ($fieldErrors)
     * @throws Exception\MpesaUnavailableException M-Pesa is off in this environment
     * @throws Exception\MpesaErrorException Safaricom could not send the prompt (nothing charged)
     * @throws Exception\IdempotencyConflictException key reused with a different phone
     */
    public function stkPush(string $paymentRequestId, string $phone, ?string $idempotencyKey = null): StkPush
    {
        $phone = trim($phone);
        if ($phone === '' || strlen($phone) > 20) {
            throw new \InvalidArgumentException('Phone must be 1-20 characters, e.g. 0712345678.');
        }
        $key = $this->idempotencyKey($idempotencyKey);

        return StkPush::fromArray($this->request(
            'POST',
            '/payment-requests/' . self::seg($paymentRequestId) . '/stk-push',
            body: ['phone' => $phone],
            headers: ['Idempotency-Key' => $key],
        ));
    }

    /** One payment prompt, e.g. to check its status while waiting for events. */
    public function getStkPush(string $id): StkPush
    {
        return StkPush::fromArray($this->request('GET', '/stk-pushes/' . self::seg($id)));
    }

    /**
     * One page of payment requests, newest first.
     *
     * @param string|null $status open|partially_paid|paid|cancelled|expired
     * @param string|null $payer payer external_ref
     * @return Page<PaymentRequest>
     */
    public function listPaymentRequests(
        ?string $status = null,
        ?string $reference = null,
        ?string $payer = null,
        ?string $cursor = null,
        ?int $limit = null,
    ): Page {
        $res = $this->request('GET', '/payment-requests', query: [
            'status' => $status, 'reference' => $reference, 'payer' => $payer, 'cursor' => $cursor, 'limit' => $limit,
        ]);
        return self::page($res, PaymentRequest::fromArray(...));
    }

    /**
     * Every matching payment request, fetching pages lazily.
     *
     * @return Generator<int,PaymentRequest>
     */
    public function paymentRequests(?string $status = null, ?string $reference = null, ?string $payer = null, ?int $pageSize = null): Generator
    {
        $cursor = null;
        do {
            $page = $this->listPaymentRequests($status, $reference, $payer, $cursor, $pageSize);
            yield from $page->data;
            $cursor = $page->nextCursor;
        } while ($cursor !== null);
    }

    /**
     * One page of transactions, newest first.
     *
     * @param string|null $payer payer external_ref
     * @return Page<Transaction>
     */
    public function listTransactions(
        ?string $payer = null,
        ?DateTimeInterface $since = null,
        ?string $cursor = null,
        ?int $limit = null,
    ): Page {
        $res = $this->request('GET', '/transactions', query: [
            'payer' => $payer,
            'since' => $since !== null ? self::formatDate($since) : null,
            'cursor' => $cursor,
            'limit' => $limit,
        ]);
        return self::page($res, Transaction::fromArray(...));
    }

    /** @return Generator<int,Transaction> */
    public function transactions(?string $payer = null, ?DateTimeInterface $since = null, ?int $pageSize = null): Generator
    {
        $cursor = null;
        do {
            $page = $this->listTransactions($payer, $since, $cursor, $pageSize);
            yield from $page->data;
            $cursor = $page->nextCursor;
        } while ($cursor !== null);
    }

    public function getTransaction(string $id): Transaction
    {
        return Transaction::fromArray($this->request('GET', '/transactions/' . self::seg($id)));
    }

    /**
     * One page of the gap-free event feed, oldest first. Start with
     * $after = null to read from the beginning. See EventPoller.
     */
    public function listEvents(?string $after = null, ?int $limit = null): EventPage
    {
        $res = $this->request('GET', '/events', query: ['after' => $after, 'limit' => $limit]);
        $events = [];
        foreach (Internal\Data::list($res, 'data') as $row) {
            $events[] = Event::fromArray(self::row($row));
        }
        return new EventPage($events, Internal\Data::nullableStr($res, 'next_cursor'), Internal\Data::bool($res, 'has_more'));
    }

    /**
     * Every event after $after until the feed is drained. Keys are the
     * cursor to resume AFTER the yielded event's page (persist it only once
     * the whole page is handled; EventPoller does this for you).
     *
     * @return Generator<string|null,Event>
     */
    public function events(?string $after = null, ?int $pageSize = null): Generator
    {
        do {
            $page = $this->listEvents($after, $pageSize);
            foreach ($page->data as $event) {
                yield $page->nextCursor => $event;
            }
            $after = $page->nextCursor ?? $after;
        } while ($page->hasMore && $page->data !== []);
    }

    // ------------------------------------------------------------- plumbing

    /** @return array<string,mixed> */
    public function __debugInfo(): array
    {
        return [
            'baseUrl' => $this->baseUrl,
            'apiKey' => Secret::REDACTED,
            'retryPolicy' => $this->retryPolicy,
        ];
    }

    /** @return array<string,mixed> */
    public function __serialize(): array
    {
        throw new \LogicException('The Talksasa Pay client holds an API key and cannot be serialized; resolve it from the container instead.');
    }

    public function baseUrl(): string
    {
        return $this->baseUrl;
    }

    /**
     * @param array<string,scalar|null> $query
     * @param array<string,mixed>|null $body
     * @param array<string,string> $headers
     * @return array<string,mixed>
     */
    private function request(string $method, string $path, array $query = [], ?array $body = null, array $headers = []): array
    {
        $url = $this->baseUrl . $path;
        $query = array_filter($query, static fn ($v) => $v !== null);
        if ($query !== []) {
            $url .= '?' . http_build_query($query, '', '&', PHP_QUERY_RFC3986);
        }
        $json = $body !== null ? json_encode($body, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE) : null;

        // Every /v1 operation is idempotent (see RetryPolicy), POST only
        // with an Idempotency-Key or on the idempotent cancel endpoint.
        $idempotent = $method !== 'POST' || isset($headers['Idempotency-Key']) || str_ends_with($path, '/cancel');

        $retry = 0;
        while (true) {
            $request = $this->requestFactory->createRequest($method, $url)
                ->withHeader('Authorization', 'Bearer ' . $this->apiKey->reveal())
                ->withHeader('Accept', 'application/json')
                ->withHeader('User-Agent', sprintf('talksasa-pay-php/%s PHP/%s', self::VERSION, PHP_VERSION));
            foreach ($headers as $k => $v) {
                $request = $request->withHeader($k, $v);
            }
            if ($json !== null) {
                $request = $request->withHeader('Content-Type', 'application/json')
                    ->withBody($this->streamFactory->createStream($json));
            }

            try {
                $response = $this->http->sendRequest($request);
            } catch (NetworkExceptionInterface $e) {
                $retry++;
                $delay = $idempotent ? $this->retryPolicy->delay($retry) : null;
                if ($delay === null) {
                    throw new TransportException(sprintf('%s %s failed: %s', $method, $path, $e->getMessage()), 0, $e);
                }
                $this->sleeper->sleep($delay);
                continue;
            } catch (ClientExceptionInterface $e) {
                throw new TransportException(sprintf('%s %s failed: %s', $method, $path, $e->getMessage()), 0, $e);
            }

            $status = $response->getStatusCode();
            if ($status >= 200 && $status < 300) {
                return self::decode($response);
            }
            if ($idempotent && RetryPolicy::isRetryableStatus($status)) {
                $error = ApiException::fromResponse($response);
                if ($error instanceof MpesaUnavailableException || $error instanceof MpesaErrorException) {
                    // Final answers about M-Pesa, not overload: retrying would not help.
                    throw $error;
                }
                $retry++;
                $retryAfter = ApiException::parseRetryAfter($response->getHeaderLine('Retry-After'));
                $delay = $this->retryPolicy->delay($retry, $retryAfter);
                if ($delay !== null) {
                    $this->sleeper->sleep($delay);
                    continue;
                }
            }
            throw ApiException::fromResponse($response);
        }
    }

    private function idempotencyKey(?string $given): string
    {
        $key = $given ?? ($this->idempotencyKeys)();
        if ($key === '' || strlen($key) > 255 || preg_match('/[\x00-\x1f\x7f]/', $key) === 1) {
            throw new \InvalidArgumentException('Idempotency key must be 1-255 printable characters.');
        }
        return $key;
    }

    /** @return array<string,mixed> */
    private static function decode(ResponseInterface $response): array
    {
        try {
            $data = json_decode((string) $response->getBody(), true, 64, JSON_THROW_ON_ERROR);
        } catch (\JsonException $e) {
            throw new UnexpectedResponseException('Response is not valid JSON: ' . $e->getMessage(), 0, $e);
        }
        if (!is_array($data) || ($data !== [] && array_is_list($data))) {
            throw new UnexpectedResponseException('Response is not a JSON object.');
        }
        return $data;
    }

    /**
     * @template T
     * @param array<string,mixed> $res
     * @param callable(array<string,mixed>): T $map
     * @return Page<T>
     */
    private static function page(array $res, callable $map): Page
    {
        $items = [];
        foreach (Internal\Data::list($res, 'data') as $row) {
            $items[] = $map(self::row($row));
        }
        return new Page($items, Internal\Data::nullableStr($res, 'next_cursor'));
    }

    /** @return array<string,mixed> */
    private static function row(mixed $row): array
    {
        if (!is_array($row) || array_is_list($row)) {
            throw new UnexpectedResponseException('Expected list item to be an object.');
        }
        return $row;
    }

    private static function seg(string $s): string
    {
        if ($s === '') {
            throw new \InvalidArgumentException('Path parameter must not be empty.');
        }
        return rawurlencode($s);
    }

    private static function formatDate(DateTimeInterface $d): string
    {
        $utc = \DateTimeImmutable::createFromInterface($d)->setTimezone(new DateTimeZone('UTC'));
        return $utc->format('u') === '000000' ? $utc->format('Y-m-d\TH:i:s\Z') : $utc->format('Y-m-d\TH:i:s.u\Z');
    }

    private static function normalizeBaseUrl(string $url, bool $allowInsecureHttp): string
    {
        $url = rtrim(trim($url), '/');
        $parts = parse_url($url);
        if ($parts === false || !isset($parts['scheme'], $parts['host']) || isset($parts['query']) || isset($parts['user'])) {
            throw new \InvalidArgumentException('Talksasa Pay base URL must look like https://host[/path].');
        }
        $scheme = strtolower($parts['scheme']);
        $host = strtolower(trim($parts['host'], '[]'));
        $loopback = $host === 'localhost' || $host === '::1' || str_starts_with($host, '127.');
        if ($scheme !== 'https' && !($scheme === 'http' && ($loopback || $allowInsecureHttp))) {
            throw new \InvalidArgumentException('Talksasa Pay base URL must use https (the API key would be sent in clear text).');
        }
        if (!str_ends_with($url, '/v1')) {
            $url .= '/v1';
        }
        return $url;
    }

    private static function defaultHttpClient(float $timeout, float $connectTimeout): ClientInterface
    {
        if (class_exists(\GuzzleHttp\Client::class)) {
            return new \GuzzleHttp\Client([
                'timeout' => $timeout,
                'connect_timeout' => $connectTimeout,
                'http_errors' => false,
                'allow_redirects' => false,
            ]);
        }
        return Psr18ClientDiscovery::find();
    }

    /** Random RFC 4122 version 4 UUID. */
    public static function uuid4(): string
    {
        $b = random_bytes(16);
        $b[6] = chr((ord($b[6]) & 0x0f) | 0x40);
        $b[8] = chr((ord($b[8]) & 0x3f) | 0x80);
        return vsprintf('%s%s-%s-%s-%s-%s%s%s', str_split(bin2hex($b), 4));
    }
}
