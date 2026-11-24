<?php

declare(strict_types=1);

namespace Talksasa\Pay\Events;

/** Keeps the cursor in memory (tests, one-off scripts). */
final class InMemoryCursorStore implements CursorStore
{
    public function __construct(private ?string $cursor = null)
    {
    }

    public function get(): ?string
    {
        return $this->cursor;
    }

    public function set(string $cursor): void
    {
        $this->cursor = $cursor;
    }
}
