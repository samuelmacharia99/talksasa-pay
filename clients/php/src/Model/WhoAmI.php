<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use Talksasa\Pay\Internal\Data;

/** The product an API key belongs to (GET /v1/whoami). */
final readonly class WhoAmI
{
    public function __construct(
        public string $productId,
        public string $productName,
        public string $accountPrefix,
        public string $keyId,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(
            Data::str($a, 'product_id'),
            Data::str($a, 'product_name'),
            Data::str($a, 'account_prefix'),
            Data::str($a, 'key_id'),
        );
    }
}
