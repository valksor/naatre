<?php

declare(strict_types=1);

namespace Naatre\Sdk\Transport;

use Naatre\Sdk\Protocol\Operation;

final readonly class AuthContext
{
    public function __construct(public string $endpoint, public Operation $operation)
    {
    }
}
