<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Cursor;

use Illuminate\Contracts\Cache\Repository as Cache;
use Talksasa\Pay\Events\CursorStore;

/** Cursor in the Laravel cache, stored forever. Use a store that does not evict. */
final class CacheCursorStore implements CursorStore
{
    public function __construct(
        private readonly Cache $cache,
        private readonly string $key,
    ) {
    }

    public function get(): ?string
    {
        $v = $this->cache->get($this->key);
        return is_string($v) && $v !== '' ? $v : null;
    }

    public function set(string $cursor): void
    {
        $this->cache->forever($this->key, $cursor);
    }
}
