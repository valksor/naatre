<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

final readonly class FpmRequestHandler
{
    public function __construct(private Dispatcher $dispatcher)
    {
    }

    public function profile(): RuntimeProfile
    {
        return RuntimeProfile::fpmUnary();
    }

    public function handle(Invocation $invocation): WorkerResult
    {
        return $this->dispatcher->dispatch($invocation);
    }
}
