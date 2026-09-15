<?php

declare(strict_types=1);

use Naatre\Sdk\Server\Dispatcher;
use Naatre\Sdk\Server\Effect;
use Naatre\Sdk\Server\EnvelopeServer;
use Naatre\Sdk\Server\HandlerRegistry;
use Naatre\Sdk\Server\Integration\IntegrationProfile;
use Naatre\Sdk\Server\Integration\LifecycleAdapter;
use Naatre\Sdk\Server\Invocation;
use Naatre\Sdk\Server\Principal;
use Naatre\Sdk\Server\Protocol;
use Naatre\Sdk\Server\RegisteredHandler;
use Naatre\Sdk\Server\RequestContext;
use Naatre\Sdk\Server\RequestContextFactory;
use Naatre\Sdk\Server\RuntimeProfile;
use Naatre\Sdk\Server\ServerException;
use Naatre\Sdk\Server\Transaction;
use Naatre\Sdk\Server\TransactionProvider;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;

require __DIR__ . '/bootstrap.php';

function workerAdapterCheck(bool $condition, string $message): void
{
    match ($condition) {
        true => null,
        false => throw new LogicException("worker adapter conformance: {$message}"),
    };
}

function workerAdapterRejects(callable $operation, string $code): void
{
    $caught = null;
    try {
        $operation();
    } catch (ServerException $error) {
        $caught = $error;
    }
    workerAdapterCheck($caught instanceof ServerException, "expected {$code}");
    workerAdapterCheck($caught->errorCode === $code, "expected {$code}, got {$caught->errorCode}");
    workerAdapterCheck($caught->getMessage() === $code, 'failure message exposed implementation details');
}

function workerAdapterInvocation(string $profile, int $sequence, string $handler = 'fixture.profile'): Invocation
{
    return new Invocation(
        "request-{$profile}-{$sequence}",
        "invocation-{$profile}-{$sequence}",
        "attempt-{$profile}-{$sequence}",
        $handler,
        'schema-1',
        1_800_000_000_000,
        "tenant-{$sequence}:subject-{$sequence}",
        new ObjectValue(),
    );
}

function workerAdapterContexts(string $profile): RequestContextFactory
{
    $resolve = static function (string $delegation): Principal {
        [$tenant, $subject] = explode(':', $delegation, 2);
        return new Principal($subject, $tenant);
    };

    return match ($profile) {
        'symfony' => new \Naatre\Sdk\Server\Integration\Symfony\RequestContextFactory($resolve),
        'laravel' => new \Naatre\Sdk\Server\Integration\Laravel\RequestContextFactory($resolve),
        default => new \Naatre\Sdk\Server\Integration\Symfony\RequestContextFactory($resolve),
    };
}

final class WorkerAdapterTransactionProvider implements TransactionProvider
{
    /** @var list<object{commits: int, rollbacks: int}> */
    public array $states = [];

    public function begin(RequestContext $_context): Transaction
    {
        $state = new class {
            public int $commits = 0;
            public int $rollbacks = 0;
        };
        $this->states[] = $state;
        return new class($state) implements Transaction {
            public function __construct(private readonly object $state)
            {
            }

            public function commit(): void
            {
                $this->state->commits++;
            }

            public function rollback(): void
            {
                $this->state->rollbacks++;
            }
        };
    }
}

/** @return array{0: HandlerRegistry, 1: object{seen: list<array{string, string}>}} */
function workerAdapterRegistry(bool $transactions): array
{
    $state = (object) ['seen' => []];
    $registry = new HandlerRegistry();
    $registry->register(new RegisteredHandler(
        'fixture.profile',
        'ProfileInput',
        'ProfileOutput',
        Effect::Mutation,
        $transactions ? [Protocol::TRANSACTION_PROVIDER] : [],
        static fn (ObjectValue $input): object => (object) ['input' => $input],
        static function (object $_input, RequestContext $context) use ($state): object {
            $tenant = $context->principal()->tenant;
            $loaded = $context->loaders()->load('profile', 'same-key', static fn (): string => $tenant);
            $state->seen[] = [$tenant, $loaded];
            return (object) ['tenant' => $loaded];
        },
        static fn (object $output): ObjectValue => ObjectValue::fromPairs([['tenant', $output->tenant]]),
        static function (ObjectValue $output): void {
            if ($output->selected('tenant')->presence !== Presence::Present) {
                throw new ServerException('OUTPUT_COMPLETION');
            }
        },
        $transactions,
    ));

    return [$registry, $state];
}

function workerAdapterCancellationEvidence(IntegrationProfile $profile, HandlerRegistry $registry, Dispatcher $dispatcher): void
{
    $server = new EnvelopeServer(
        $registry,
        $dispatcher,
        $profile->runtime,
        'fixture-worker',
        'spiffe://example/fixture-worker',
        'naatre-gateway',
        'fixture-stdio',
        'schema-1',
        str_repeat('a', 64),
        'php-session-1',
    );
    $server->handle(ObjectValue::fromPairs([
        ['protocol', Protocol::VERSION],
        ['kind', 'register'],
        ['payload', ObjectValue::fromPairs([
            ['protocol', Protocol::VERSION],
            ['workerId', 'fixture-worker'],
            ['serviceIdentity', 'spiffe://example/fixture-worker'],
            ['audience', 'naatre-gateway'],
            ['endpoint', 'fixture-stdio'],
            ['schemaRevision', 'schema-1'],
            ['schemaDigest', str_repeat('a', 64)],
            ['capabilities', new ListValue($profile->runtime->capabilities)],
            ['limits', ObjectValue::fromPairs([
                ['maxInFlight', 1],
                ['maxRequestBytes', 4096],
                ['maxResponseBytes', 4096],
                ['maxStreamFrames', 4],
                ['maxStreamBytes', 16_384],
            ])],
            ['handlers', new ListValue(array_map(
                static fn (RegisteredHandler $handler): ObjectValue => $handler->descriptor(),
                $registry->all(),
            ))],
        ])],
    ]));

    $cancel = static fn (): ObjectValue => $server->handle(ObjectValue::fromPairs([
        ['protocol', Protocol::VERSION],
        ['kind', 'cancel'],
        ['payload', ObjectValue::fromPairs([
            ['requestId', 'request-cancel'],
            ['invocationId', 'invocation-cancel'],
        ])],
    ]));
    if ($profile->runtime->supports([Protocol::CANCELLATION_ACK])) {
        workerAdapterCheck($cancel()->selected('kind')->value === 'cancelled', "{$profile->id} cancellation acknowledgement");
    } else {
        workerAdapterRejects($cancel, 'REMOTE_CANCELLATION_INVALID');
    }
}

$profiles = [
    'symfony' => IntegrationProfile::symfony(RuntimeProfile::longLived([Protocol::TRANSACTION_PROVIDER])),
    'laravel' => IntegrationProfile::laravel(RuntimeProfile::longLived([Protocol::TRANSACTION_PROVIDER])),
    'fpm' => IntegrationProfile::fpm(),
    'roadrunner' => IntegrationProfile::roadRunner(),
    'swoole' => IntegrationProfile::swoole(),
];

$evidence = [];
foreach ($profiles as $name => $profile) {
    workerAdapterCheck($profile->id !== '' && $profile->layer() !== '', "{$name} profile identity");
    workerAdapterCheck($profile->lifecycle() !== '' && $profile->cancellation() !== '' && $profile->pool() !== '', "{$name} lifecycle evidence");
    workerAdapterCheck($profile->transaction() !== '' && $profile->streaming() !== '' && $profile->requestIsolation() !== '', "{$name} capability evidence");
    workerAdapterCheck($profile->supportedPlatforms() !== [] && $profile->supportedRuntimes !== [], "{$name} support boundary");
    workerAdapterCheck($profile->unsupportedCapabilities() !== [], "{$name} unsupported boundary");

    $usesTransactions = $profile->runtime->supports([Protocol::TRANSACTION_PROVIDER]);
    [$registry, $state] = workerAdapterRegistry($usesTransactions);
    $transactions = new WorkerAdapterTransactionProvider();
    $dispatcher = new Dispatcher(
        $registry,
        $profile->runtime,
        workerAdapterContexts($name),
        $usesTransactions ? $transactions : null,
    );
    $resets = 0;
    $adapter = new LifecycleAdapter($profile, $dispatcher, static function () use (&$resets): void { $resets++; }, 2);
    $adapter->handle(workerAdapterInvocation($name, 1));
    $adapter->handle(workerAdapterInvocation($name, 2));
    workerAdapterCheck($state->seen === [['tenant-1', 'tenant-1'], ['tenant-2', 'tenant-2']], "{$name} request isolation");
    workerAdapterCheck($resets === 2 && $adapter->handledRequests() === 2 && !$adapter->accepting(), "{$name} lifecycle and pool limit");
    workerAdapterRejects(static fn () => $adapter->handle(workerAdapterInvocation($name, 3)), 'REMOTE_REGISTRATION_INVALID');

    if ($usesTransactions) {
        workerAdapterCheck(count($transactions->states) === 2, "{$name} transaction count");
        workerAdapterCheck($transactions->states[0]->commits === 1 && $transactions->states[1]->commits === 1, "{$name} transaction commits");
    } else {
        workerAdapterCheck($transactions->states === [], "{$name} unsupported transactions");
    }

    $streamCalls = 0;
    $streamRegistry = new HandlerRegistry();
    $streamRegistry->register(new RegisteredHandler(
        'fixture.stream', 'StreamInput', 'StreamOutput', Effect::Subscription, [Protocol::SERVER_STREAMING],
        static fn (ObjectValue $_input): object => new stdClass(),
        static function (object $_input, RequestContext $_context) use (&$streamCalls): object { $streamCalls++; return new stdClass(); },
        static fn (object $_output): ObjectValue => new ObjectValue(),
        static function (ObjectValue $_output): void {},
    ));
    $streamDispatcher = new Dispatcher($streamRegistry, $profile->runtime, workerAdapterContexts($name));
    workerAdapterRejects(
        static fn () => $streamDispatcher->dispatch(workerAdapterInvocation($name, 1, 'fixture.stream')),
        'REMOTE_CAPABILITY_MISMATCH',
    );
    workerAdapterCheck($streamCalls === 0, "{$name} streaming rejection before source");

    workerAdapterCancellationEvidence($profile, $registry, $dispatcher);

    $secretRegistry = new HandlerRegistry();
    $secretRegistry->register(new RegisteredHandler(
        'fixture.secret', 'SecretInput', 'SecretOutput', Effect::Query, [],
        static fn (ObjectValue $_input): object => new stdClass(),
        static function (object $_input, RequestContext $_context): object {
            throw new RuntimeException('password=hunter2 delegatedContext=protected');
        },
        static fn (object $_output): ObjectValue => new ObjectValue(),
        static function (ObjectValue $_output): void {},
    ));
    $secretDispatcher = new Dispatcher($secretRegistry, $profile->runtime, workerAdapterContexts($name));
    $secretAdapter = new LifecycleAdapter($profile, $secretDispatcher, static function (): void {}, 1);
    workerAdapterRejects(
        static fn () => $secretAdapter->handle(workerAdapterInvocation($name, 1, 'fixture.secret')),
        'REMOTE_WORKER_MALFORMED',
    );

    $cleanupAdapter = new LifecycleAdapter(
        $profile,
        $dispatcher,
        static function (): void { throw new RuntimeException('dsn=mysql://user:secret@example'); },
        1,
    );
    workerAdapterRejects(
        static fn () => $cleanupAdapter->handle(workerAdapterInvocation($name, 4)),
        'REMOTE_WORKER_MALFORMED',
    );

    $evidence[$profile->id] = [
        'layer' => $profile->layer(),
        'status' => 'passed',
        'executedRuntime' => 'php-cli-lifecycle-model',
        'lifecycle' => $profile->lifecycle(),
        'cancellation' => $profile->cancellation(),
        'pool' => $profile->pool(),
        'transaction' => $profile->transaction(),
        'streaming' => $profile->streaming(),
        'requestIsolation' => $profile->requestIsolation(),
        'supportedPlatforms' => $profile->supportedPlatforms(),
        'supportedRuntimes' => $profile->supportedRuntimes,
        'evidence' => [
            'lifecycle' => "{$name}-lifecycle-and-drain",
            'cancellation' => "{$name}-cancellation-disposition",
            'pool' => "{$name}-finite-request-budget",
            'transaction' => "{$name}-transaction-boundary",
            'streaming' => "{$name}-streaming-rejected-before-source",
            'requestIsolation' => "{$name}-principal-loader-and-reset-isolation",
            'failureSafety' => "{$name}-stable-redacted-failure",
            'resourceLimit' => "{$name}-request-budget-boundary",
        ],
        'unsupported' => $profile->unsupportedCapabilities(),
    ];
}

workerAdapterRejects(
    static fn () => new LifecycleAdapter(
        IntegrationProfile::fpm(),
        new Dispatcher(new HandlerRegistry(), RuntimeProfile::fpmUnary(), workerAdapterContexts('fpm')),
        static function (): void {},
        0,
    ),
    'REMOTE_REGISTRATION_INVALID',
);

fwrite(STDOUT, json_encode([
    'profile' => 'sdk.php.worker-adapters-1',
    'status' => 'passed',
    'runtime' => PHP_VERSION,
    'dependencyRevision' => '10c560933509dacbc7ad987b864f86f2ee0994de',
    'profiles' => $evidence,
], JSON_THROW_ON_ERROR) . "\n");
