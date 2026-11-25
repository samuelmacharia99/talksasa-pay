<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 403 ip_not_allowed: the API key is restricted to an IP allow-list that does not include this server. */
final class IpNotAllowedException extends PermissionDeniedException
{
}
