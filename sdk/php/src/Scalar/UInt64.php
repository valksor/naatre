<?php

declare(strict_types=1);

namespace Naatre\Sdk\Scalar;

use Naatre\Sdk\Attribute\WireScalar;
use Naatre\Sdk\Exception\ClientException;

#[WireScalar('UInt64', 'decimal-string')]
final readonly class UInt64
{
    public string $value;

    public function __construct(string $value)
    {
        $canonical = IntegerString::canonical($value, false);
        if (IntegerString::compare($canonical, '18446744073709551615') > 0) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $this->value = $canonical;
    }

    public function __toString(): string
    {
        return $this->value;
    }
}
