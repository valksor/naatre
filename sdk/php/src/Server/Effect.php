<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

enum Effect: string
{
    case Query = 'query';
    case Mutation = 'mutation';
    case Transaction = 'transaction';
    case Subscription = 'subscription';
}
