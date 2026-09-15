<?php

declare(strict_types=1);

namespace Naatre\Sdk\Native;

use InvalidArgumentException;

final readonly class Limits
{
    public function __construct(
        public int $maximumDepth = 128,
        public int $maximumNodes = 1_000_000,
        public int $maximumBytes = 16_777_216,
        public int $maximumOutputBytes = 16_777_216,
    ) {
        foreach (['maximumDepth' => $this->maximumDepth, 'maximumNodes' => $this->maximumNodes, 'maximumBytes' => $this->maximumBytes, 'maximumOutputBytes' => $this->maximumOutputBytes] as $name => $value) {
            if ($value < 1) {
                throw new InvalidArgumentException($name . ' must be positive');
            }
        }
    }

    /** @return array{0: int, 1: int, 2: int, 3: int} */
    public function arguments(): array
    {
        return [$this->maximumDepth, $this->maximumNodes, $this->maximumBytes, $this->maximumOutputBytes];
    }
}
