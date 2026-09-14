<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

final readonly class OpenEnumValue
{
    /** @param non-empty-string $raw */
    public function __construct(public string $raw, public bool $known)
    {
    }
}
