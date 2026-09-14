<?php

declare(strict_types=1);

namespace Naatre\Sdk\Pagination;

/** @template T */
final readonly class Page
{
    /** @param list<T> $items */
    public function __construct(public array $items, public ?string $nextCursor)
    {
    }
}
