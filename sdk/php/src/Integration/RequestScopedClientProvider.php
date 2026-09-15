<?php

declare(strict_types=1);

namespace Naatre\Sdk\Integration;

use Naatre\Sdk\Transport\AuthContext;
use Naatre\Sdk\Transport\Psr18Client;

interface RequestScopedClientProvider
{
    /** @param (callable(AuthContext): array<string, string>)|null $authenticate */
    public function forRequest(mixed $authenticate = null): Psr18Client;
}
