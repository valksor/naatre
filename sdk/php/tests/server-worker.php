<?php

declare(strict_types=1);

use Naatre\Sdk\Server\ApplicationException;
use Naatre\Sdk\Server\Dispatcher;
use Naatre\Sdk\Server\Effect;
use Naatre\Sdk\Server\EnvelopeServer;
use Naatre\Sdk\Server\FramedWorker;
use Naatre\Sdk\Server\HandlerRegistry;
use Naatre\Sdk\Server\Invocation;
use Naatre\Sdk\Server\Principal;
use Naatre\Sdk\Server\RegisteredHandler;
use Naatre\Sdk\Server\RequestContext;
use Naatre\Sdk\Server\RequestContextFactory;
use Naatre\Sdk\Server\RuntimeProfile;
use Naatre\Sdk\Server\ServerException;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;

require __DIR__ . '/bootstrap.php';

$adapter = 'neutral';
$terminateMutation = false;
foreach (array_slice($argv, 1) as $argument) {
    if (str_starts_with($argument, '--adapter=')) {
        $adapter = substr($argument, strlen('--adapter='));
    } elseif ($argument === '--terminate-mutation') {
        $terminateMutation = true;
    }
}

$resolve = static function (string $delegation): Principal {
    $parts = explode(':', $delegation, 2);
    if (count($parts) !== 2 || $parts[0] === '' || $parts[1] === '') {
        throw new ServerException('UNAUTHORIZED');
    }
    return new Principal($parts[1], $parts[0]);
};

$contexts = match ($adapter) {
    'neutral' => new class($resolve) implements RequestContextFactory {
        public function __construct(private readonly Closure $resolve)
        {
        }

        public function create(Invocation $invocation): RequestContext
        {
            return new RequestContext($invocation->requestId, $invocation->invocationId, ($this->resolve)($invocation->delegatedContext));
        }
    },
    'symfony' => new \Naatre\Sdk\Server\Integration\Symfony\RequestContextFactory($resolve),
    'laravel' => new \Naatre\Sdk\Server\Integration\Laravel\RequestContextFactory($resolve),
    default => throw new RuntimeException('unknown fixture adapter'),
};

$registry = new HandlerRegistry();
if ($terminateMutation) {
    $registry->register(new RegisteredHandler(
        'fixture.mutate',
        'GreetInput',
        'GreetOutput',
        Effect::Mutation,
        [],
        static fn (ObjectValue $input): object => (object) ['input' => $input],
        static function (object $_input, RequestContext $_context): object {
            exit(23);
        },
        static fn (object $_output): ObjectValue => new ObjectValue(),
        static function (ObjectValue $_output): void {},
    ));
} else {
    $registry->register(new RegisteredHandler(
        'fixture.greet',
        'GreetInput',
        'GreetOutput',
        Effect::Query,
        [],
        static function (ObjectValue $input): object {
            $name = $input->selected('name');
            if ($name->presence !== Presence::Present || !is_string($name->value)) {
                throw new ServerException('REMOTE_INVOCATION_INVALID');
            }
            return (object) ['name' => $name->value];
        },
        static function (object $input, RequestContext $context): object {
            /** @var string $name */
            $name = $input->name;
            if ($name === 'reject') {
                throw new ApplicationException('NAME_REJECTED', 'name was rejected');
            }
            $context->loaders()->load('greeting', $name, static fn (): string => $context->principal()->tenant);
            return (object) ['greeting' => "Hello, {$name}"];
        },
        static fn (object $output): ObjectValue => new ObjectValue([new MapEntry('greeting', $output->greeting)]),
        static function (ObjectValue $output): void {
            $greeting = $output->selected('greeting');
            if ($greeting->presence !== Presence::Present || !is_string($greeting->value)) {
                throw new ServerException('OUTPUT_COMPLETION');
            }
        },
    ));
}

$profile = RuntimeProfile::longLived();
$dispatcher = new Dispatcher($registry, $profile, $contexts);
$server = new EnvelopeServer(
    $registry,
    $dispatcher,
    $profile,
    'fixture-worker',
    'spiffe://example/fixture-worker',
    'naatre-gateway',
    'fixture-stdio',
    'schema-1',
    str_repeat('a', 64),
    'php-session-1',
);

(new FramedWorker($server, STDIN, STDOUT))->run();
