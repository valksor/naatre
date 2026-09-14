<?php

declare(strict_types=1);

namespace Naatre\Sdk\Attribute;

use Attribute;

#[Attribute(Attribute::TARGET_CLASS_CONSTANT | Attribute::TARGET_CLASS)]
final readonly class WireVariant
{
    public function __construct(public string $name, public bool $open = false)
    {
    }
}
