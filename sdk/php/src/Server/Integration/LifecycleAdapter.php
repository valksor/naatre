<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server\Integration;

use Closure;
use Naatre\Sdk\Server\Dispatcher;
use Naatre\Sdk\Server\Invocation;
use Naatre\Sdk\Server\ServerException;
use Naatre\Sdk\Server\WorkerResult;
use Throwable;

/**
 * Applies finite admission, draining, and host cleanup around the core dispatcher.
 */
final class LifecycleAdapter
{
    private bool $accepting = true;
    private bool $active = false;
    private int $handledRequests = 0;

    /** @param Closure(): void $afterRequest */
    public function __construct(
        public readonly IntegrationProfile $profile,
        private readonly Dispatcher $dispatcher,
        private readonly Closure $afterRequest,
        private readonly int $maximumRequests,
    ) {
        if ($maximumRequests < 1) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
    }

    public function handle(Invocation $invocation): WorkerResult
    {
        if (!$this->accepting || $this->active || $this->handledRequests >= $this->maximumRequests) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }

        $this->active = true;
        $result = null;
        $failure = null;
        try {
            $result = $this->dispatcher->dispatch($invocation);
        } catch (Throwable $error) {
            $failure = $error;
        }

        $cleanupFailed = false;
        try {
            ($this->afterRequest)();
        } catch (Throwable) {
            $cleanupFailed = true;
        } finally {
            $this->active = false;
            $this->handledRequests++;
            if ($this->handledRequests >= $this->maximumRequests) {
                $this->accepting = false;
            }
        }

        if ($failure instanceof ServerException) {
            throw $failure;
        }
        if ($failure !== null) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        if ($cleanupFailed || !$result instanceof WorkerResult) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }

        return $result;
    }

    public function drain(): void
    {
        $this->accepting = false;
    }

    public function accepting(): bool
    {
        return $this->accepting;
    }

    public function handledRequests(): int
    {
        return $this->handledRequests;
    }
}
