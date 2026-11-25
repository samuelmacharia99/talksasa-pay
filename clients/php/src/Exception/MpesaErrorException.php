<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 502 mpesa_error: Safaricom refused or failed to send the prompt. Nothing
 * was charged and the charge stays pending. Never retried automatically: ask
 * the customer to pay at the paybill, or to try again (a new attempt needs a
 * new idempotency key, because the same key returns this failed prompt).
 */
final class MpesaErrorException extends ServerException
{
    /** The failed push's id, when the server put it in the problem detail. */
    public function stkPushId(): ?string
    {
        return $this->uuidInDetail();
    }
}
