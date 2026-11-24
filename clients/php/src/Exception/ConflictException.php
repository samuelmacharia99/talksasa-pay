<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 409 (or payer_closed): the request conflicts with the resource's current state. */
class ConflictException extends ApiException
{
}
