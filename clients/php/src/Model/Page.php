<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

/**
 * One page of a list. Pass $nextCursor as `cursor` for the next page; it is
 * null on the last page. Client::charges()/transactions() iterate
 * every page for you.
 *
 * @template T
 */
final readonly class Page implements \IteratorAggregate, \Countable
{
    /**
     * @param list<T> $data
     */
    public function __construct(
        public array $data,
        public ?string $nextCursor,
    ) {
    }

    public function hasMore(): bool
    {
        return $this->nextCursor !== null;
    }

    /** @return \ArrayIterator<int,T> */
    public function getIterator(): \ArrayIterator
    {
        return new \ArrayIterator($this->data);
    }

    public function count(): int
    {
        return count($this->data);
    }
}
