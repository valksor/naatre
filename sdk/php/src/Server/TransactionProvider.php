<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

interface TransactionProvider
{
    public function begin(RequestContext $context): Transaction;
}
