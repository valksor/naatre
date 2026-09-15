<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

interface Transaction
{
    public function commit(): void;

    public function rollback(): void;
}
