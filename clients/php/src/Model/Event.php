<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Exception\UnexpectedResponseException;
use Talksasa\Pay\Internal\Data;

/**
 * An event, from a webhook body or GET /v1/events. $data is the state of
 * the subject at the time of the event:
 *   charge.*             -> Charge
 *   stk_push.*           -> StkPush
 *   payment.received     -> PaymentReceived
 *   transaction.reversed -> Transaction (the reversal; reversesTransactionId
 *                           names the payment, chargeId the reversed charge)
 * An event type this version does not know has $data = null; its raw JSON
 * is always in $rawData.
 *
 * Webhooks are at least once and may arrive out of order: de-duplicate by
 * $id, and re-read the resource when ordering matters.
 */
final readonly class Event
{
    /** Event type => class of $data. */
    public const DATA_CLASSES = [
        EventType::CHARGE_SUCCEEDED => Charge::class,
        EventType::CHARGE_NEEDS_REVIEW => Charge::class,
        EventType::CHARGE_EXPIRED => Charge::class,
        EventType::CHARGE_CANCELLED => Charge::class,
        EventType::STK_PUSH_SENT => StkPush::class,
        EventType::STK_PUSH_FAILED => StkPush::class,
        EventType::STK_PUSH_SUCCEEDED => StkPush::class,
        EventType::STK_PUSH_NEEDS_REVIEW => StkPush::class,
        EventType::PAYMENT_RECEIVED => PaymentReceived::class,
        EventType::TRANSACTION_REVERSED => Transaction::class,
    ];

    /**
     * @param array<string,mixed> $rawData
     */
    public function __construct(
        public string $id,
        public string $type,
        public string $apiVersion,
        public DateTimeImmutable $createdAt,
        public Charge|StkPush|Transaction|null $data,
        public array $rawData,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        $type = Data::str($a, 'type');
        $raw = Data::obj($a, 'data');

        $class = self::DATA_CLASSES[$type] ?? null;
        $data = $class === null ? null : $class::fromArray($raw);

        return new self(
            Data::str($a, 'id'),
            $type,
            Data::str($a, 'api_version'),
            Data::date($a, 'created_at'),
            $data,
            $raw,
        );
    }

    /** Decodes a webhook body. Prefer Webhook\Verifier::verify(), which also checks the signature. */
    public static function fromJson(string $json): self
    {
        try {
            $a = json_decode($json, true, 64, JSON_THROW_ON_ERROR);
        } catch (\JsonException $e) {
            throw new UnexpectedResponseException('Event is not valid JSON: ' . $e->getMessage(), 0, $e);
        }
        if (!is_array($a) || array_is_list($a)) {
            throw new UnexpectedResponseException('Event is not a JSON object.');
        }
        return self::fromArray($a);
    }

    /** True for a type this SDK version knows (and decodes into $data). */
    public function isKnownType(): bool
    {
        return isset(self::DATA_CLASSES[$this->type]);
    }

    /** The charge of a charge.* event. */
    public function charge(): Charge
    {
        if (!$this->data instanceof Charge) {
            throw new \LogicException("Event {$this->type} does not carry a charge.");
        }
        return $this->data;
    }

    /** The prompt of a stk_push.* event. */
    public function stkPush(): StkPush
    {
        if (!$this->data instanceof StkPush) {
            throw new \LogicException("Event {$this->type} does not carry an STK push.");
        }
        return $this->data;
    }

    /** The payment of a payment.received event. */
    public function paymentReceived(): PaymentReceived
    {
        if (!$this->data instanceof PaymentReceived) {
            throw new \LogicException("Event {$this->type} does not carry a received payment.");
        }
        return $this->data;
    }

    /** The reversal transaction of transaction.reversed (also the payment of payment.received). */
    public function transaction(): Transaction
    {
        if (!$this->data instanceof Transaction) {
            throw new \LogicException("Event {$this->type} does not carry a transaction.");
        }
        return $this->data;
    }
}
