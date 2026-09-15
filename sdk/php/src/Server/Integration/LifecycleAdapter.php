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
        $dispatched = false;
        try {
            $result = $this->dispatcher->dispatch($invocation);
            $dispatched = true;
        } catch (ServerException $failure) {
            throw $failure;
        } catch (Throwable) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        } finally {
            $cleanupFailed = false;
            try {
                ($this->afterRequest)();
            } catch (Throwable) {
                $cleanupFailed = true;
            }
            $this->active = false;
            $this->handledRequests++;
            if ($this->handledRequests >= $this->maximumRequests) {
                $this->accepting = false;
            }
            // A dispatch failure already propagates through this finally and takes
            // priority; only a successful dispatch is downgraded when cleanup fails.
            if ($dispatched && $cleanupFailed) {
                throw new ServerException('REMOTE_WORKER_MALFORMED');
            }
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
