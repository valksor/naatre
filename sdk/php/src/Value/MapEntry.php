<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

final readonly class MapEntry
{
    public function __construct(public string $key, public mixed $value)
    {
    }
}
