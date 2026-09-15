<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

interface RequestContextFactory
{
    public function create(Invocation $invocation): RequestContext;
}
