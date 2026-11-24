<?php

declare(strict_types=1);

namespace Talksasa\Pay\Webhook;

/**
 * The Talksasa-Signature header is missing, malformed, outside the time
 * tolerance, or no v1 signature matches. $reason is one of the constants.
 * Messages never contain the secret or the expected signature.
 */
final class SignatureException extends WebhookException
{
    public const MISSING_HEADER = 'missing_header';
    public const MALFORMED_HEADER = 'malformed_header';
    public const TIMESTAMP_OUT_OF_TOLERANCE = 'timestamp_out_of_tolerance';
    public const NO_MATCHING_SIGNATURE = 'no_matching_signature';
}
