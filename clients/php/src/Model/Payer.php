<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Internal\Data;

/** A payer (your customer). Phones are masked by the server. */
final readonly class Payer
{
    public const STATUS_ACTIVE = 'active';
    public const STATUS_CLOSED = 'closed';

    /**
     * @param list<string> $phones masked, first is primary
     */
    public function __construct(
        public string $id,
        public string $externalRef,
        /** What the customer types as the M-Pesa account number, e.g. HOST-1042. */
        public string $accountNumber,
        public string $displayName,
        public ?string $email,
        public array $phones,
        public string $status,
        /** KES overpaid and not yet applied; used automatically on the next request. */
        public int $creditBalance,
        public DateTimeImmutable $createdAt,
        public DateTimeImmutable $updatedAt,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        $phones = array_values(array_filter(Data::list($a, 'phones'), 'is_string'));

        return new self(
            Data::str($a, 'id'),
            Data::str($a, 'external_ref'),
            Data::str($a, 'account_number'),
            Data::str($a, 'display_name'),
            Data::nullableStr($a, 'email'),
            $phones,
            Data::str($a, 'status'),
            Data::int($a, 'credit_balance'),
            Data::date($a, 'created_at'),
            Data::date($a, 'updated_at'),
        );
    }

    public function isActive(): bool
    {
        return $this->status === self::STATUS_ACTIVE;
    }
}
