<?php

declare(strict_types=1);

namespace Talksasa\Pay\Http;

/**
 * Holds a credential so it never shows up in var_dump/print_r, string
 * casts, logs or serialized objects. Call reveal() only where it is sent.
 */
final class Secret implements \Stringable, \JsonSerializable
{
    public const REDACTED = '[redacted]';

    public function __construct(#[\SensitiveParameter] private readonly string $value)
    {
    }

    public function reveal(): string
    {
        return $this->value;
    }

    public function isEmpty(): bool
    {
        return $this->value === '';
    }

    public function __toString(): string
    {
        return self::REDACTED;
    }

    public function jsonSerialize(): string
    {
        return self::REDACTED;
    }

    /** @return array<string,string> */
    public function __debugInfo(): array
    {
        return ['value' => self::REDACTED];
    }

    /** @return array<string,mixed> */
    public function __serialize(): array
    {
        throw new \LogicException('Secrets cannot be serialized.');
    }
}
