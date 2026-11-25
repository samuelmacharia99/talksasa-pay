<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 stk_in_progress: a prompt for this charge is already on the
 * customer's phone. Wait for its stk_push.* event (or poll getStkPush())
 * instead of sending another: only one prompt per charge can be live.
 */
final class StkInProgressException extends ConflictException
{
    /** The live prompt's id, when the server put it in the problem detail. */
    public function liveStkPushId(): ?string
    {
        return $this->uuidInDetail();
    }
}
