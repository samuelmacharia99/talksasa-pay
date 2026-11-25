<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/**
 * 403: the key may not do this. Subclasses: IpNotAllowedException
 * (ip_not_allowed) and AppSuspendedException (app_suspended).
 */
class PermissionDeniedException extends ApiException
{
}
