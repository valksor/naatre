<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

final readonly class Principal
{
    public function __construct(public string $subject, public string $tenant)
    {
        if ($subject === '' || $tenant === '') {
            throw new ServerException('UNAUTHORIZED');
        }
    }
}
