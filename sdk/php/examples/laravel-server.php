<?php

declare(strict_types=1);

use Naatre\Sdk\Server\Integration\Laravel\RequestContextFactory;
use Naatre\Sdk\Server\Invocation;
use Naatre\Sdk\Server\Principal;
use Naatre\Sdk\Value\ObjectValue;

require is_file(__DIR__ . '/../vendor/autoload.php')
    ? __DIR__ . '/../vendor/autoload.php'
    : __DIR__ . '/../tests/bootstrap.php';

// Bind this factory explicitly in Laravel's container. Resolve it from request
// or job scope; never put a RequestContext into a singleton.
$factory = new RequestContextFactory(static fn (string $token): Principal => new Principal('user-1', $token));
$invocation = new Invocation('request-1', 'invocation-1', 'attempt-1', 'example.echo', 'schema-1', 1_800_000_000_000, 'tenant-laravel', new ObjectValue());
$context = $factory->create($invocation);
fwrite(STDOUT, $context->principal()->tenant . "\n");
$context->close(false);
