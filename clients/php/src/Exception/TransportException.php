<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * The request never got an HTTP response (DNS, connect, TLS, timeout), after
 * any retries. A write MAY still have been applied by the server: retry a
 * create with the SAME Idempotency-Key, never a new one.
 */
final class TransportException extends \RuntimeException implements TalksasaPayException
{
}
