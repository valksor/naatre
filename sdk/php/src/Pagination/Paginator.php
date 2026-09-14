<?php

declare(strict_types=1);

namespace Naatre\Sdk\Pagination;

use Closure;
use Generator;
use Naatre\Sdk\Exception\ClientException;

/** @template T */
final readonly class Paginator
{
    /** @var Closure(?string): Page<T> */
    private Closure $fetch;

    /** @param Closure(?string): Page<T> $fetch */
    public function __construct(Closure $fetch, private int $maximumPages = 1_000)
    {
        if ($maximumPages < 1) {
            throw new ClientException('CLIENT_PAGINATION_INVALID');
        }
        $this->fetch = $fetch;
    }

    /** @return Generator<int, T> */
    public function items(): Generator
    {
        $cursor = null;
        $seen = [];
        for ($pageNumber = 0; $pageNumber < $this->maximumPages; ++$pageNumber) {
            $page = ($this->fetch)($cursor);
            foreach ($page->items as $item) {
                yield $item;
            }
            if ($page->nextCursor === null) {
                return;
            }
            if ($page->nextCursor === '' || isset($seen[$page->nextCursor])) {
                throw new ClientException('CLIENT_PAGINATION_INVALID');
            }
            $seen[$page->nextCursor] = true;
            $cursor = $page->nextCursor;
        }
        throw new ClientException('CLIENT_PAGINATION_LIMIT');
    }
}
