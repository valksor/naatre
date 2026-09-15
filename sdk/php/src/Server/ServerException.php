<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use RuntimeException;

final class ServerException extends RuntimeException
{
    public function __construct(public readonly string $errorCode)
    {
        parent::__construct($errorCode);
    }
}
