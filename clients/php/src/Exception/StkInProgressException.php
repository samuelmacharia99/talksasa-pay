<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 409 stk_in_progress: a prompt for this request is already on the
 * customer's phone. Wait for its stk_push.* event (or poll getStkPush())
 * instead of sending another: only one prompt per request can be live.
 */
final class StkInProgressException extends ConflictException
{
    /** The live prompt's id, when the server put it in the problem detail. */
    public function liveStkPushId(): ?string
    {
        if ($this->detail !== null && preg_match('/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i', $this->detail, $m) === 1) {
            return strtolower($m[0]);
        }
        return null;
    }
}
