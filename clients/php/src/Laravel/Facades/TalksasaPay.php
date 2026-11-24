<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Facades;

use Illuminate\Support\Facades\Facade;
use Talksasa\Pay\Client;

/**
 * @method static \Talksasa\Pay\Model\WhoAmI whoami()
 * @method static \Talksasa\Pay\Model\Payer upsertPayer(string $externalRef, string $displayName, ?string $email = null, array $phones = [])
 * @method static \Talksasa\Pay\Model\Payer getPayer(string $externalRef)
 * @method static \Talksasa\Pay\Model\PaymentRequest createPaymentRequest(string $payer, string $reference, string $description, int $amount, ?\DateTimeInterface $expiresAt = null, ?string $idempotencyKey = null)
 * @method static \Talksasa\Pay\Model\PaymentRequest getPaymentRequest(string $id)
 * @method static \Talksasa\Pay\Model\PaymentRequest cancelPaymentRequest(string $id)
 * @method static \Talksasa\Pay\Model\StkPush stkPush(string $paymentRequestId, string $phone, ?string $idempotencyKey = null)
 * @method static \Talksasa\Pay\Model\StkPush getStkPush(string $id)
 * @method static \Talksasa\Pay\Model\Page listPaymentRequests(?string $status = null, ?string $reference = null, ?string $payer = null, ?string $cursor = null, ?int $limit = null)
 * @method static \Generator paymentRequests(?string $status = null, ?string $reference = null, ?string $payer = null, ?int $pageSize = null)
 * @method static \Talksasa\Pay\Model\Page listTransactions(?string $payer = null, ?\DateTimeInterface $since = null, ?string $cursor = null, ?int $limit = null)
 * @method static \Generator transactions(?string $payer = null, ?\DateTimeInterface $since = null, ?int $pageSize = null)
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
