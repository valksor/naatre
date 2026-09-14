<?php

declare(strict_types=1);

namespace Naatre\Sdk\Exception;

use RuntimeException;
use Throwable;

final class ClientException extends RuntimeException
{
    public function __construct(public readonly string $errorCode, ?Throwable $previous = null)
    {
        parent::__construct($errorCode, 0, $previous);
    }
}
