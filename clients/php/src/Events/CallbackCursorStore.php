<?php

declare(strict_types=1);

namespace Talksasa\Pay\Events;

/**
 * Adapts two callables, e.g. reading and writing a row in your own table:
 *
 *     new CallbackCursorStore(
 *         fn () => $db->fetchOne('SELECT cursor FROM tsp_cursor WHERE id = 1') ?: null,
 *         fn (string $c) => $db->executeStatement('UPDATE tsp_cursor SET cursor = ? WHERE id = 1', [$c]),
 *     );
 */
final class CallbackCursorStore implements CursorStore
{
    /** @var \Closure(): ?string */
    private readonly \Closure $get;
    /** @var \Closure(string): mixed */
    private readonly \Closure $set;

    /**
     * @param callable(): ?string $get
     * @param callable(string): mixed $set
     */
    public function __construct(callable $get, callable $set)
    {
        $this->get = \Closure::fromCallable($get);
        $this->set = \Closure::fromCallable($set);
    }

    public function get(): ?string
    {
        $c = ($this->get)();
        return is_string($c) && $c !== '' ? $c : null;
    }

    public function set(string $cursor): void
    {
        ($this->set)($cursor);
    }
}
