<?php

declare(strict_types=1);

namespace Naatre\Sdk\Scalar;

use Naatre\Sdk\Attribute\WireScalar;
use Naatre\Sdk\Exception\ClientException;

#[WireScalar('Int64', 'decimal-string')]
final readonly class Int64
{
    public string $value;

    public function __construct(string $value)
    {
        $canonical = IntegerString::canonical($value, true);
        if (IntegerString::compare($canonical, '-9223372036854775808') < 0
            || IntegerString::compare($canonical, '9223372036854775807') > 0) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $this->value = $canonical;
    }

    public function __toString(): string
    {
        return $this->value;
    }
}
