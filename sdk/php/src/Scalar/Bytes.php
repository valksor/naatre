<?php

declare(strict_types=1);

namespace Naatre\Sdk\Scalar;

use Naatre\Sdk\Attribute\WireScalar;
use Naatre\Sdk\Exception\ClientException;

#[WireScalar('Bytes', 'base64url-unpadded')]
final readonly class Bytes
{
    public function __construct(public string $bytes)
    {
    }

    public function wire(): string
    {
        return rtrim(strtr(base64_encode($this->bytes), '+/', '-_'), '=');
    }

    public static function fromWire(string $wire): self
    {
        if (preg_match('/^[A-Za-z0-9_-]*$/', $wire) !== 1 || strlen($wire) % 4 === 1) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $padded = strtr($wire, '-_', '+/') . str_repeat('=', (4 - strlen($wire) % 4) % 4);
        $decoded = base64_decode($padded, true);
        if ($decoded === false || (new self($decoded))->wire() !== $wire) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        return new self($decoded);
    }
}
