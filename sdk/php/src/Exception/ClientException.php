<?php

declare(strict_types=1);

namespace Naatre\Sdk\Exception;

use RuntimeException;
use Throwable;

final class ClientException extends RuntimeException
{
    /** Underlying failures are deliberately not retained on the public exception. */
    public function __construct(public readonly string $errorCode, ?Throwable $previous = null)
    {
        parent::__construct($errorCode);
    }
}
