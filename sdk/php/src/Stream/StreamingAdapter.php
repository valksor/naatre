<?php

declare(strict_types=1);

namespace Naatre\Sdk\Stream;

use Naatre\Sdk\Protocol\Operation;

interface StreamingAdapter
{
    /**
     * @return CloseableStream<mixed>
     */
    public function open(Operation $operation): CloseableStream;

    /** @return 'active-abort'|'cooperative'|'deadline-only' */
    public function cancellationSupport(): string;
}
