<?php

declare(strict_types=1);

namespace Naatre\Sdk\Scalar;

use Naatre\Sdk\Attribute\WireScalar;
use Naatre\Sdk\Exception\ClientException;

#[WireScalar('Decimal', 'lossless-decimal-string')]
final readonly class Decimal
{
    public string $value;

    public function __construct(string $value)
    {
        if (preg_match('/^-?(?:0|[1-9][0-9]*|0[0-9]+)(?:\.[0-9]+)?$/', $value) !== 1) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $negative = str_starts_with($value, '-');
        $unsigned = $negative ? substr($value, 1) : $value;
        [$integer, $fraction] = array_pad(explode('.', $unsigned, 2), 2, '');
        $integer = ltrim($integer, '0');
        $integer = $integer === '' ? '0' : $integer;
        $fraction = rtrim($fraction, '0');
        $canonical = $fraction === '' ? $integer : $integer . '.' . $fraction;
        $this->value = $canonical === '0' ? '0' : ($negative ? '-' : '') . $canonical;
    }

    public function __toString(): string
    {
        return $this->value;
    }
}
