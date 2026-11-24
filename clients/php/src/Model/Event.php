<?php

declare(strict_types=1);

namespace Talksasa\Pay\Model;

use DateTimeImmutable;
use Talksasa\Pay\Exception\UnexpectedResponseException;
use Talksasa\Pay\Internal\Data;

/**
 * An event, from a webhook body or GET /v1/events. $data is the state of
 * the subject at the time of the event:
 *   payment_request.* -> PaymentRequest
 *   transaction.*     -> Transaction
 *   payer.credit_changed -> CreditChange
 * An event type this version does not know has $data = null; its raw JSON
 * is always in $rawData.
 *
 * Webhooks are at least once and may arrive out of order: de-duplicate by
 * $id, and re-read the resource when ordering matters.
 */
final readonly class Event
{
    /**
     * @param array<string,mixed> $rawData
     */
    public function __construct(
        public string $id,
        public string $type,
        public string $apiVersion,
        public DateTimeImmutable $createdAt,
        public PaymentRequest|Transaction|CreditChange|null $data,
        public array $rawData,
    ) {
    }

    /** @param array<string,mixed> $a */
    public static function fromArray(array $a): self
    {
        $type = Data::str($a, 'type');
        $raw = Data::obj($a, 'data');

        $data = match (true) {
            str_starts_with($type, 'payment_request.') => PaymentRequest::fromArray($raw),
            str_starts_with($type, 'transaction.') => Transaction::fromArray($raw),
            $type === EventType::PAYER_CREDIT_CHANGED => CreditChange::fromArray($raw),
            default => null,
        };

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

    public function paymentRequest(): PaymentRequest
    {
        if (!$this->data instanceof PaymentRequest) {
            throw new \LogicException("Event {$this->type} does not carry a payment request.");
        }
        return $this->data;
    }

    public function transaction(): Transaction
    {
        if (!$this->data instanceof Transaction) {
            throw new \LogicException("Event {$this->type} does not carry a transaction.");
        }
        return $this->data;
    }

    public function creditChange(): CreditChange
    {
        if (!$this->data instanceof CreditChange) {
            throw new \LogicException("Event {$this->type} does not carry a credit change.");
        }
        return $this->data;
    }
}
