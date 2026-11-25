<?php

declare(strict_types=1);

namespace Talksasa\Pay\Exception;

/** 404 not_found: no such resource for this app (another app's ids are 404 too). */
final class NotFoundException extends ApiException
{
}
