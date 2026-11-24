<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 403: ip_not_allowed (the key's IP allow-list) or product_suspended. */
final class PermissionDeniedException extends ApiException
{
}
