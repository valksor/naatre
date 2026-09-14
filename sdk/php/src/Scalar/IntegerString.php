<?php

declare(strict_types=1);

namespace Naatre\Sdk\Scalar;

use Naatre\Sdk\Exception\ClientException;

final class IntegerString
{
    private function __construct()
    {
    }

    public static function canonical(string $value, bool $signed): string
    {
        $pattern = $signed ? '/^-?(?:0|[1-9][0-9]*)$/' : '/^(?:0|[1-9][0-9]*)$/';
        if (preg_match($pattern, $value) !== 1) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        return $value === '-0' ? '0' : $value;
    }

    public static function compare(string $left, string $right): int
    {
        $leftNegative = str_starts_with($left, '-');
        $rightNegative = str_starts_with($right, '-');
        if ($leftNegative !== $rightNegative) {
            return $leftNegative ? -1 : 1;
        }
        $leftMagnitude = $leftNegative ? substr($left, 1) : $left;
        $rightMagnitude = $rightNegative ? substr($right, 1) : $right;
        $order = strlen($leftMagnitude) <=> strlen($rightMagnitude);
        if ($order === 0) {
            $order = strcmp($leftMagnitude, $rightMagnitude) <=> 0;
        }
        return $leftNegative ? -$order : $order;
    }
}
