<?php

declare(strict_types=1);

namespace Naatre\Sdk\Attribute;

use Attribute;

#[Attribute(Attribute::TARGET_CLASS)]
final readonly class WireScalar
{
    public function __construct(public string $name, public string $mapping)
    {
    }
}
