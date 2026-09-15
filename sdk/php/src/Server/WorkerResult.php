<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;

final readonly class WorkerResult
{
    /** @param list<ObjectValue> $errors */
    public function __construct(
        public string $invocationId,
        public string $attemptId,
        public string $schemaRevision,
        public mixed $data,
        public array $errors = [],
    ) {
    }

    public static function applicationError(Invocation $invocation, ApplicationException $error): self
    {
        $entries = [
            new MapEntry('code', $error->errorCode),
            new MapEntry('message', $error->getMessage()),
            new MapEntry('retryable', $error->retryable),
        ];
        if ($error->details !== null) {
            $entries[] = new MapEntry('details', $error->details);
        }
        return new self($invocation->invocationId, $invocation->attemptId, $invocation->schemaRevision, null, [new ObjectValue($entries)]);
    }

    public function toWire(): ObjectValue
    {
        return new ObjectValue([
            new MapEntry('protocol', Protocol::VERSION),
            new MapEntry('invocationId', $this->invocationId),
            new MapEntry('attemptId', $this->attemptId),
            new MapEntry('schemaRevision', $this->schemaRevision),
            new MapEntry('data', $this->data),
            new MapEntry('errors', new ListValue($this->errors)),
        ]);
    }
}
