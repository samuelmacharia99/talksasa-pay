<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

/**
 * One page of the event feed (oldest first). After processing it, store
 * $nextCursor and pass it as `after` next time. $nextCursor is null when
 * the page is empty: keep your previous cursor.
 */
final readonly class EventPage implements \IteratorAggregate, \Countable
{
    /**
     * @param list<Event> $data
     */
    public function __construct(
        public array $data,
        public ?string $nextCursor,
        public bool $hasMore,
    ) {
    }

    /** @return \ArrayIterator<int,Event> */
    public function getIterator(): \ArrayIterator
    {
        return new \ArrayIterator($this->data);
    }

    public function count(): int
    {
        return count($this->data);
    }
}
