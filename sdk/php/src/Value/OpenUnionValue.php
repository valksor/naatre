<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

final readonly class OpenUnionValue
{
    /** @param non-empty-string $discriminator */
    public function __construct(
        public string $discriminator,
        public mixed $value,
        public bool $known,
    ) {
    }
}
