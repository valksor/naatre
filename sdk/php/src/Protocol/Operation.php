<?php

declare(strict_types=1);

namespace Naatre\Sdk\Protocol;

use Closure;
use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Wire\Json;

final readonly class Operation
{
    /** @var Closure(ObjectValue): mixed */
    private Closure $decoder;

    /**
     * @param 'query'|'mutation'|'subscription' $kind
     * @param Closure(ObjectValue): mixed $decoder
     */
    public function __construct(
        public string $name,
        public string $kind,
        public string $persistedDigest,
        public ObjectValue $variables,
        Closure $decoder,
    ) {
        if (preg_match('/^[A-Fa-f0-9]{64}$/', $persistedDigest) !== 1) {
            throw new ClientException('CLIENT_PERSISTED_INVALID');
        }
        $this->decoder = $decoder;
    }

    public function canonicalRequest(): string
    {
        return Json::encode(new ObjectValue([
            new MapEntry('operation', $this->name),
            new MapEntry('persisted', new ObjectValue([
                new MapEntry('algorithm', 'sha-256'),
                new MapEntry('canonicalVersion', 'c14n-1'),
                new MapEntry('digest', strtolower($this->persistedDigest)),
            ])),
            new MapEntry('variables', $this->variables),
            new MapEntry('version', '1'),
        ]));
    }

    public function decode(ObjectValue $data): mixed
    {
        return ($this->decoder)($data);
    }
}
