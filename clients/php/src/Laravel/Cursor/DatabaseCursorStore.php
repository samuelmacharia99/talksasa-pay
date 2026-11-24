<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Cursor;

use Illuminate\Database\ConnectionInterface;
use Talksasa\Pay\Events\CursorStore;

/** Cursor in a database row (see the published talksasa_pay_cursors migration). */
final class DatabaseCursorStore implements CursorStore
{
    public function __construct(
        private readonly ConnectionInterface $db,
        private readonly string $table = 'talksasa_pay_cursors',
        private readonly string $name = 'events',
    ) {
    }

    public function get(): ?string
    {
        $v = $this->db->table($this->table)->where('name', $this->name)->value('cursor');
        return is_string($v) && $v !== '' ? $v : null;
    }

    public function set(string $cursor): void
    {
        $this->db->table($this->table)->upsert(
            [['name' => $this->name, 'cursor' => $cursor, 'updated_at' => date('Y-m-d H:i:s')]],
            ['name'],
            ['cursor', 'updated_at'],
        );
    }
}
