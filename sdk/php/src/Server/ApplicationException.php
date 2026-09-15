<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Naatre\Sdk\Value\ObjectValue;
use RuntimeException;

final class ApplicationException extends RuntimeException
{
    public function __construct(
        public readonly string $errorCode,
        string $safeMessage,
        public readonly bool $retryable = false,
        public readonly ?ObjectValue $details = null,
    ) {
        if (preg_match('/^[A-Z][A-Z0-9_]{2,63}$/', $errorCode) !== 1 || $safeMessage === '' || strlen($safeMessage) > 32_768) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        parent::__construct($safeMessage);
    }
}
