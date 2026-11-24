<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Support;

use GuzzleHttp\Psr7\Response;
use Psr\Http\Client\ClientInterface;
use Psr\Http\Client\NetworkExceptionInterface;
use Psr\Http\Message\RequestInterface;
use Psr\Http\Message\ResponseInterface;

/** PSR-18 client that replays queued responses/exceptions and records requests. */
final class MockHttpClient implements ClientInterface
{
    /** @var list<RequestInterface> */
    public array $requests = [];
    /** @var list<ResponseInterface|\Throwable> */
    private array $queue = [];

    public function push(ResponseInterface|\Throwable ...$items): self
    {
        foreach ($items as $i) {
            $this->queue[] = $i;
        }
        return $this;
    }

    /** @param array<string,mixed> $body */
    public function pushJson(int $status, array $body, array $headers = []): self
    {
        $ct = $status >= 400 ? 'application/problem+json' : 'application/json';
        return $this->push(new Response($status, ['Content-Type' => $ct] + $headers, json_encode($body, JSON_THROW_ON_ERROR)));
    }

    public function pushNetworkError(string $message = 'Connection refused'): self
    {
        return $this->push(new class ($message) extends \RuntimeException implements NetworkExceptionInterface {
            public function getRequest(): RequestInterface
            {
                return new \GuzzleHttp\Psr7\Request('GET', 'https://example.invalid');
            }
        });
    }

    public function sendRequest(RequestInterface $request): ResponseInterface
    {
        $this->requests[] = $request;
        $next = array_shift($this->queue);
        if ($next === null) {
            throw new \LogicException('MockHttpClient: no response queued for ' . $request->getMethod() . ' ' . $request->getUri());
        }
        if ($next instanceof \Throwable) {
            throw $next;
        }
        return $next;
    }

    public function last(): RequestInterface
    {
        $r = end($this->requests);
        if ($r === false) {
            throw new \LogicException('No request sent.');
        }
        return $r;
    }

    /** @return array<string,mixed> */
    public function lastJson(): array
    {
        return json_decode((string) $this->last()->getBody(), true, 64, JSON_THROW_ON_ERROR);
    }
}
