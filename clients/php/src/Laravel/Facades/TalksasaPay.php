<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Facades;

use Illuminate\Support\Facades\Facade;
use Talksasa\Pay\Client;

/**
 * @method static \Talksasa\Pay\Model\WhoAmI whoami()
 * @method static \Talksasa\Pay\Model\Charge createCharge(int $amount, string $reference, ?string $description = null, ?string $phone = null, ?string $accountNumber = null, ?array $metadata = null, ?int $expiresIn = null, ?string $idempotencyKey = null)
 * @method static \Talksasa\Pay\Model\Charge getCharge(string $id)
 * @method static \Talksasa\Pay\Model\Page listCharges(?string $status = null, ?string $reference = null, ?string $cursor = null, ?int $limit = null)
 * @method static \Generator charges(?string $status = null, ?string $reference = null, ?int $pageSize = null)
 * @method static \Talksasa\Pay\Model\Charge cancelCharge(string $id)
 * @method static \Talksasa\Pay\Model\StkPush stkPush(string $chargeId, string $phone, ?string $idempotencyKey = null)
 * @method static \Talksasa\Pay\Model\StkPush getStkPush(string $id)
 * @method static \Talksasa\Pay\Model\Page listTransactions(?string $chargeId = null, ?\DateTimeInterface $since = null, ?string $cursor = null, ?int $limit = null)
 * @method static \Generator transactions(?string $chargeId = null, ?\DateTimeInterface $since = null, ?int $pageSize = null)
 * @method static \Talksasa\Pay\Model\Transaction getTransaction(string $id)
 * @method static \Talksasa\Pay\Model\EventPage listEvents(?string $after = null, ?int $limit = null)
 * @method static \Generator events(?string $after = null, ?int $pageSize = null)
 *
 * @see Client
 */
final class TalksasaPay extends Facade
{
    protected static function getFacadeAccessor(): string
    {
        return Client::class;
    }
}
