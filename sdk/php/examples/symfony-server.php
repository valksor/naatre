<?php

declare(strict_types=1);

use Naatre\Sdk\Server\Integration\Symfony\RequestContextFactory;
use Naatre\Sdk\Server\Invocation;
use Naatre\Sdk\Server\Principal;
use Naatre\Sdk\Value\ObjectValue;

require is_file(__DIR__ . '/../vendor/autoload.php')
    ? __DIR__ . '/../vendor/autoload.php'
    : __DIR__ . '/../tests/bootstrap.php';

// Register this factory as an explicit Symfony service. A controller or worker
// adapter supplies the request's verified delegated identity on every call.
$factory = new RequestContextFactory(static fn (string $token): Principal => new Principal('user-1', $token));
$invocation = new Invocation('request-1', 'invocation-1', 'attempt-1', 'example.echo', 'schema-1', 1_800_000_000_000, 'tenant-symfony', new ObjectValue());
$context = $factory->create($invocation);
fwrite(STDOUT, $context->principal()->tenant . "\n");
$context->close(false);
