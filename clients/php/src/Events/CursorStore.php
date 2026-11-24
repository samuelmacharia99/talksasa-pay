<?php

declare(strict_types=1);

namespace Talksasa\Pay\Events;

/**
 * Where EventPoller keeps its position in the feed. Use durable storage
 * (a database row): losing the cursor means re-reading the feed from the
 * beginning, which your handler must survive (it is idempotent anyway).
 */
interface CursorStore
{
    /** The saved cursor, or null to start from the beginning of the feed. */
    public function get(): ?string;

    public function set(string $cursor): void;
}
