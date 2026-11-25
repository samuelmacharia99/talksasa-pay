<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 403 app_suspended: Talksasa suspended this app; it cannot create charges or send prompts. */
final class AppSuspendedException extends PermissionDeniedException
{
}
