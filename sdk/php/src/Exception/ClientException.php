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
        // The cause is accepted for call-site ergonomics but intentionally discarded:
        // it is never chained into the parent, so getPrevious() stays null and no
        // internal failure detail leaks across the SDK's public exception boundary.
        unset($previous);
        parent::__construct($errorCode);
    }
}
