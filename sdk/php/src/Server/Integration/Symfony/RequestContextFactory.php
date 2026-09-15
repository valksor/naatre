<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server\Integration\Symfony;

use Closure;
use Naatre\Sdk\Server\Invocation;
use Naatre\Sdk\Server\Principal;
use Naatre\Sdk\Server\RequestContext;

final readonly class RequestContextFactory implements \Naatre\Sdk\Server\RequestContextFactory
{
    /** @param Closure(string): Principal $resolvePrincipal */
    public function __construct(private Closure $resolvePrincipal)
    {
    }

    #[\Override]
    public function create(Invocation $invocation): RequestContext
    {
        return new RequestContext($invocation->requestId, $invocation->invocationId, ($this->resolvePrincipal)($invocation->delegatedContext));
    }
}
