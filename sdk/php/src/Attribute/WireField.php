<?php

declare(strict_types=1);

namespace Naatre\Sdk\Attribute;

use Attribute;

#[Attribute(Attribute::TARGET_PROPERTY | Attribute::TARGET_PARAMETER)]
final readonly class WireField
{
    public function __construct(
        public string $name,
        public bool $required = true,
        public bool $nullable = false,
        public bool $pending = false,
    ) {
    }
}
