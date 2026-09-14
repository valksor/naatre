<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

use Naatre\Sdk\Exception\ClientException;

final readonly class ObjectValue
{
    /** @var list<MapEntry> */
    private array $entries;

    /** @param iterable<MapEntry> $entries */
    public function __construct(iterable $entries = [])
    {
        $copy = [];
        $seen = [];
        foreach ($entries as $entry) {
            if (str_contains($entry->key, "\0") || isset($seen[$entry->key])) {
                throw new ClientException('CLIENT_OBJECT_KEY_INVALID');
            }
            $seen[$entry->key] = true;
            $copy[] = $entry;
        }
        $this->entries = $copy;
    }

    /** @param list<array{0: string, 1: mixed}> $pairs */
    public static function fromPairs(array $pairs): self
    {
        return new self(array_map(
            static fn (array $pair): MapEntry => new MapEntry($pair[0], $pair[1]),
            $pairs,
        ));
    }

    /** @return list<MapEntry> */
    public function entries(): array
    {
        return $this->entries;
    }

    public function selected(string $key): Selected
    {
        foreach ($this->entries as $entry) {
            if ($entry->key === $key) {
                return $entry->value === null ? Selected::null() : Selected::present($entry->value);
            }
        }
        return Selected::missing();
    }
}
