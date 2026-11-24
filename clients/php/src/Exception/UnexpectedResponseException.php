<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** The server answered 2xx but the body did not match the contract. */
final class UnexpectedResponseException extends \UnexpectedValueException implements TalksasaPayException
{
}
