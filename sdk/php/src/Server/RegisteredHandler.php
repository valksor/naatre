<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Closure;
use Naatre\Sdk\Value\ObjectValue;

final readonly class RegisteredHandler
{
    /**
     * @param list<string> $requiredCapabilities
     * @param Closure(ObjectValue): object $decodeInput
     * @param Closure(object, RequestContext): object $invoke
     * @param Closure(object): ObjectValue $encodeOutput
     * @param Closure(ObjectValue): void $validateOutput
     */
    public function __construct(
        public string $id,
        public string $inputSchema,
        public string $outputSchema,
        public Effect $effect,
        public array $requiredCapabilities,
        public Closure $decodeInput,
        public Closure $invoke,
        public Closure $encodeOutput,
        public Closure $validateOutput,
        public bool $usesTransaction = false,
    ) {
        if (preg_match('/^[A-Za-z][A-Za-z0-9._:-]{0,127}$/', $id) !== 1 || $inputSchema === '' || $outputSchema === '') {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
        if ($usesTransaction && !in_array(Protocol::TRANSACTION_PROVIDER, $requiredCapabilities, true)) {
            throw new ServerException('REMOTE_CAPABILITY_MISMATCH');
        }
    }

    public function descriptor(): ObjectValue
    {
        return ObjectValue::fromPairs([
            ['id', $this->id],
            ['inputSchema', $this->inputSchema],
            ['outputSchema', $this->outputSchema],
            ['codec', Protocol::CODEC],
            ['effect', $this->effect->value],
            ['requiredCapabilities', new \Naatre\Sdk\Value\ListValue($this->requiredCapabilities)],
        ]);
    }
}
