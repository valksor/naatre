<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Naatre\Sdk\Value\ObjectValue;

final readonly class Invocation
{
    public function __construct(
        public string $requestId,
        public string $invocationId,
        public string $attemptId,
        public string $handlerId,
        public string $schemaRevision,
        public int $deadlineUnixMilli,
        public string $delegatedContext,
        public mixed $input,
        public ?string $idempotencyKey = null,
        public ?ObjectValue $parent = null,
        public ?string $resumeCursor = null,
    ) {
    }
}
