<?php

declare(strict_types=1);

namespace Naatre\Sdk\Stream;

use Closure;
use Iterator;
use Naatre\Sdk\Exception\ClientException;
use Throwable;

/**
 * @template T
 * @implements Iterator<int, T>
 */
final class CloseableStream implements Iterator
{
    /** @var Iterator<int, T> */
    private Iterator $source;
    private bool $closed = false;
    private int $index = 0;

    /** @var Closure(string): void */
    private Closure $closer;

    /**
     * @param Iterator<int, T> $source
     * @param Closure(string): void $closer
     */
    public function __construct(Iterator $source, Closure $closer)
    {
        $this->source = $source;
        $this->closer = $closer;
    }

    public function __destruct()
    {
        try {
            $this->close('abandoned');
        } catch (Throwable) {
            // Destructors cannot report cleanup failures safely.
        }
    }

    public function close(string $reason = 'cancelled'): void
    {
        if (!$this->closed) {
            $this->closed = true;
            ($this->closer)($reason);
        }
    }

    #[\Override]
    public function current(): mixed
    {
        try {
            return $this->source->current();
        } catch (Throwable $error) {
            $this->close('error');
            throw new ClientException('CLIENT_STREAM_ERROR', $error);
        }
    }

    #[\Override]
    public function key(): int
    {
        return $this->index;
    }

    #[\Override]
    public function next(): void
    {
        try {
            $this->source->next();
            ++$this->index;
            if (!$this->source->valid()) {
                $this->close('complete');
            }
        } catch (Throwable $error) {
            $this->close('error');
            throw new ClientException('CLIENT_STREAM_ERROR', $error);
        }
    }

    #[\Override]
    public function rewind(): void
    {
        try {
            $this->source->rewind();
            $this->index = 0;
        } catch (Throwable $error) {
            $this->close('error');
            throw new ClientException('CLIENT_STREAM_ERROR', $error);
        }
    }

    #[\Override]
    public function valid(): bool
    {
        if ($this->closed) {
            return false;
        }
        try {
            $valid = $this->source->valid();
            if (!$valid) {
                $this->close('complete');
            }
            return $valid;
        } catch (Throwable $error) {
            $this->close('error');
            throw new ClientException('CLIENT_STREAM_ERROR', $error);
        }
    }

    public function timeout(): void
    {
        $this->close('timeout');
    }
}
