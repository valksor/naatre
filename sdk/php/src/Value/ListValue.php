<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

final readonly class ListValue
{
    /** @var list<mixed> */
    private array $values;

    /** @param list<mixed> $values */
    public function __construct(array $values = [])
    {
        $this->values = $values;
    }

    /** @return list<mixed> */
    public function values(): array
    {
        return $this->values;
    }
}
