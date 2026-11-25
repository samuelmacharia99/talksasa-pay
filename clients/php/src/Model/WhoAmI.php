<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use Talksasa\Pay\Internal\Data;

/** The app an API key belongs to (GET /v1/whoami). */
final readonly class WhoAmI
{
    public function __construct(
        public string $appId,
        public string $appName,
        /** Your paybill account-number prefix, e.g. HOST. */
        public string $accountPrefix,
        public string $keyId,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        return new self(
            Data::str($a, 'app_id'),
            Data::str($a, 'app_name'),
            Data::str($a, 'account_prefix'),
            Data::str($a, 'key_id'),
        );
    }
}
