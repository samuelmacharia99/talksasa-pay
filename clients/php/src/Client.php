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
use Talksasa\Pay\Model\Charge;
use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\EventPage;
use Talksasa\Pay\Model\Page;
use Talksasa\Pay\Model\StkPush;
use Talksasa\Pay\Model\Transaction;
use Talksasa\Pay\Model\WhoAmI;

/**
 * Talksasa Pay app API (/v1) client: charges, M-Pesa prompts, transactions
 * and the event feed.
 *
 * HTTP goes through any PSR-18 client. When none is given, Guzzle 7 is used
 * if installed (so the timeouts apply), else php-http/discovery finds one.
 *
 * Not final so applications can mock it in their own tests.
 */
class Client
{
    public const VERSION = '0.2.0';

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

    /** The app this key belongs to. Handy as a health/credentials check. */
    public function whoami(): WhoAmI
    {
        return WhoAmI::fromArray($this->request('GET', '/whoami'));
    }

    /**
     * Creates a pending charge for exactly $amount KES (1 to 250,000). The
     * result carries $checkoutUrl (a hosted pay page to send the customer
     * to) and $paybill (manual M-Pesa instructions).
     *
     * With $phone, the first M-Pesa prompt is sent right away and returned
     * as $latestStkPush. The charge is created even if that prompt cannot be
     * sent ($latestStkPush is then the failed push, or null when M-Pesa is
     * unavailable): prompt again with stkPush() or let the customer pay at
     * the paybill.
     *
     * Every call sends an Idempotency-Key and the client's automatic retries
     * reuse it, so one call creates at most one charge. Without
     * $idempotencyKey a random key is generated per call; pass a stable one
     * (e.g. "invoice-42-charge-1") to make a retry after a crash or timeout
     * return the same charge too.
     *
     * @param string $reference your own reference (invoice number, order id); unique among your charges
     * @param string|null $phone Safaricom number to prompt now: 07XXXXXXXX, 01XXXXXXXX, 2547XXXXXXXX or +2547XXXXXXXX
     * @param string|null $accountNumber your own account number (your prefix, "-", a digit, then A-Z/0-9; max 12); omit to get one generated
     * @param array<string,mixed>|null $metadata any JSON object up to 4 KB, echoed back on the charge and its events
     * @param int|null $expiresIn seconds until a pending charge expires (900 to 604800; server default 86400)
     * @throws Exception\DuplicateReferenceException you already have a charge with this reference
     * @throws Exception\ValidationException invalid fields ($fieldErrors)
     * @throws Exception\IdempotencyConflictException key reused with a different body
     */
    public function createCharge(
        int $amount,
        string $reference,
        ?string $description = null,
        ?string $phone = null,
        ?string $accountNumber = null,
        ?array $metadata = null,
        ?int $expiresIn = null,
        ?string $idempotencyKey = null,
    ): Charge {
        $body = ['reference' => $reference, 'amount' => $amount];
        if ($description !== null) {
            $body['description'] = $description;
        }
        if ($phone !== null) {
            $body['phone'] = self::phone($phone);
        }
        if ($accountNumber !== null) {
            $body['account_number'] = $accountNumber;
        }
        if ($metadata !== null) {
            if ($metadata !== [] && array_is_list($metadata)) {
                throw new \InvalidArgumentException('Charge metadata must be a JSON object (string keys), not a list.');
            }
            $body['metadata'] = $metadata === [] ? new \stdClass() : $metadata;
        }
        if ($expiresIn !== null) {
            $body['expires_in'] = $expiresIn;
        }
        $key = $this->idempotencyKey($idempotencyKey);

        return Charge::fromArray($this->request('POST', '/charges', body: $body, headers: ['Idempotency-Key' => $key]));
    }

    /** One charge, with its latest prompt. */
    public function getCharge(string $id): Charge
    {
        return Charge::fromArray($this->request('GET', '/charges/' . self::seg($id)));
    }

    /**
     * One page of charges, newest first.
     *
     * @param string|null $status pending|needs_review|succeeded|expired|cancelled|reversed
     * @param string|null $reference exact reference
     * @return Page<Charge>
     */
    public function listCharges(
        ?string $status = null,
        ?string $reference = null,
        ?string $cursor = null,
        ?int $limit = null,
    ): Page {
        $res = $this->request('GET', '/charges', query: [
            'status' => $status, 'reference' => $reference, 'cursor' => $cursor, 'limit' => $limit,
        ]);
        return self::page($res, Charge::fromArray(...));
    }

    /**
     * Every matching charge, newest first, fetching pages lazily.
     *
     * @return Generator<int,Charge>
     */
    public function charges(?string $status = null, ?string $reference = null, ?int $pageSize = null): Generator
    {
        $cursor = null;
        $seen = [];
        do {
            $page = $this->listCharges($status, $reference, $cursor, $pageSize);
            yield from $page->data;
            $cursor = self::nextCursor($page, $seen);
        } while ($cursor !== null);
    }

    /**
     * Cancels a pending charge (emits charge.cancelled). Idempotent:
     * cancelling a cancelled charge returns it. A prompt already on the
     * customer's phone is not withdrawn: if they still pay, the charge
     * becomes succeeded (late money is never dropped).
     *
     * @throws Exception\NotCancellableException the charge is not pending
     */
    public function cancelCharge(string $id): Charge
    {
        return Charge::fromArray($this->request('POST', '/charges/' . self::seg($id) . '/cancel'));
    }

    /**
     * Sends an M-Pesa payment prompt (STK push) to $phone for the charge's
     * exact amount. Returns as soon as Safaricom has the prompt (status
     * sent) or Talksasa is still sending it (initiating).
     *
     * The outcome arrives as events: stk_push.succeeded together with
     * charge.succeeded, or stk_push.failed ($resultDesc says why, e.g.
     * cancelled by the customer; the charge stays pending and can be
     * prompted again). Mark the invoice paid ONLY on charge.succeeded.
     *
     * Always sends an Idempotency-Key, and every automatic retry (network
     * error, 429, 503 overloaded/busy/timeout) re-sends the SAME key, so the
     * customer never gets two prompts from one call. Pass your own key
     * (e.g. "invoice-{$id}-stk-{$attempt}") to make a retry after a crash
     * safe too; reuse a key only for the same phone.
     *
     * @param string $phone Safaricom number: 07XXXXXXXX, 01XXXXXXXX, 2547XXXXXXXX or +2547XXXXXXXX
     * @throws Exception\StkInProgressException a prompt for this charge is already on a phone
     * @throws Exception\NotPayableException the charge is not pending
     * @throws Exception\AmountTooLargeException above the M-Pesa prompt limit: use the paybill
     * @throws Exception\ValidationException not a valid Safaricom number ($fieldErrors)
     * @throws Exception\MpesaUnavailableException M-Pesa is off in this environment
     * @throws Exception\MpesaErrorException Safaricom could not send the prompt (nothing charged)
     * @throws Exception\IdempotencyConflictException key reused with a different phone
     */
    public function stkPush(string $chargeId, string $phone, ?string $idempotencyKey = null): StkPush
    {
        $phone = self::phone($phone);
        $key = $this->idempotencyKey($idempotencyKey);

        return StkPush::fromArray($this->request(
            'POST',
            '/charges/' . self::seg($chargeId) . '/stk-push',
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
     * One page of transactions (money received and reversed), newest first.
     *
     * @param string|null $chargeId only the money for this charge
     * @return Page<Transaction>
     */
    public function listTransactions(
        ?string $chargeId = null,
        ?DateTimeInterface $since = null,
        ?string $cursor = null,
        ?int $limit = null,
    ): Page {
        $res = $this->request('GET', '/transactions', query: [
            'charge_id' => $chargeId,
            'since' => $since !== null ? self::formatDate($since) : null,
            'cursor' => $cursor,
            'limit' => $limit,
        ]);
        return self::page($res, Transaction::fromArray(...));
    }

    /**
     * Every matching transaction, newest first, fetching pages lazily.
     *
     * @return Generator<int,Transaction>
     */
    public function transactions(?string $chargeId = null, ?DateTimeInterface $since = null, ?int $pageSize = null): Generator
    {
        $cursor = null;
        $seen = [];
        do {
            $page = $this->listTransactions($chargeId, $since, $cursor, $pageSize);
            yield from $page->data;
            $cursor = self::nextCursor($page, $seen);
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

        // Only safe requests are retried (see RetryPolicy): GETs, POSTs that
        // carry an Idempotency-Key (every retry re-sends the same key), and
        // cancel, which is idempotent by contract.
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

    /**
     * The next page's cursor, or null at the end. A server that repeats a
     * cursor must not make a generator loop forever.
     *
     * @param Page<mixed> $page
     * @param array<string,true> $seen
     */
    private static function nextCursor(Page $page, array &$seen): ?string
    {
        $cursor = $page->nextCursor;
        if ($cursor === null || $cursor === '' || $page->data === []) {
            return null;
        }
        if (isset($seen[$cursor])) {
            throw new UnexpectedResponseException('The server repeated a page cursor.');
        }
        $seen[$cursor] = true;
        return $cursor;
    }

    private static function phone(string $phone): string
    {
        $phone = trim($phone);
        if ($phone === '' || strlen($phone) > 20) {
            throw new \InvalidArgumentException('Phone must be 1-20 characters, e.g. 0712345678.');
        }
        return $phone;
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
