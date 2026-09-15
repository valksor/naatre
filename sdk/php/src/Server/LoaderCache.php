<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Closure;

final class LoaderCache
{
    /** @var array<string, mixed> */
    private array $values = [];

    /** @param Closure(): mixed $load */
    public function load(string $loader, string $key, Closure $load): mixed
    {
        $cacheKey = $loader . "\0" . $key;
        if (!array_key_exists($cacheKey, $this->values)) {
            $this->values[$cacheKey] = $load();
        }
        return $this->values[$cacheKey];
    }

    public function clear(): void
    {
        $this->values = [];
    }

    public function count(): int
    {
        return count($this->values);
    }
}
