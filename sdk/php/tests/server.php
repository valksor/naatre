<?php

declare(strict_types=1);

use Naatre\Sdk\Generated\GetAccountHandler;
use Naatre\Sdk\Generated\GetAccountHandlerBinding;
use Naatre\Sdk\Generated\GetAccountHandlerInput;
use Naatre\Sdk\Generated\GetAccountHandlerOutput;
use Naatre\Sdk\Generated\GetAccountHandlerProfileOutput;
use Naatre\Sdk\Server\ApplicationException;
use Naatre\Sdk\Server\Dispatcher;
use Naatre\Sdk\Server\Effect;
use Naatre\Sdk\Server\EnvelopeServer;
use Naatre\Sdk\Server\FramedWorker;
use Naatre\Sdk\Server\HandlerRegistry;
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
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;
use Naatre\Sdk\Value\Selected;
use Naatre\Sdk\Wire\Json;

require __DIR__ . '/bootstrap.php';

function serverCheck(bool $condition, string $message): void
{
    if (!$condition) {
        throw new RuntimeException($message);
    }
}

function serverRejects(callable $callable, string $code): void
{
    try {
        $callable();
    } catch (ServerException $error) {
        serverCheck($error->errorCode === $code, "expected {$code}, got {$error->errorCode}");
        return;
    }
    throw new RuntimeException("expected {$code}");
}

$contexts = new class implements RequestContextFactory {
    /** @var list<RequestContext> */
    public array $created = [];

    public function create(Invocation $invocation): RequestContext
    {
        [$tenant, $subject] = explode(':', $invocation->delegatedContext, 2);
        $context = new RequestContext($invocation->requestId, $invocation->invocationId, new Principal($subject, $tenant));
        $this->created[] = $context;
        return $context;
    }
};

$transactions = new class implements TransactionProvider {
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
};

$seen = [];
$scopeHandler = new RegisteredHandler(
    'fixture.scope',
    'ScopeInput',
    'ScopeOutput',
    Effect::Mutation,
    [Protocol::TRANSACTION_PROVIDER],
    static fn (ObjectValue $input): object => (object) ['input' => $input],
    static function (object $_input, RequestContext $context) use (&$seen): object {
        $tenant = $context->principal()->tenant;
        $cached = $context->loaders()->load('tenant', 'same-key', static fn (): string => $tenant);
        $seen[] = [$tenant, $cached];
        if ($tenant === 'tenant-error') {
            throw new ApplicationException('TENANT_REJECTED', 'tenant was rejected');
        }
        return (object) ['tenant' => $cached];
    },
    static fn (object $output): ObjectValue => ObjectValue::fromPairs([['tenant', $output->tenant]]),
    static function (ObjectValue $output): void {
        if ($output->selected('tenant')->presence !== Presence::Present) {
            throw new ServerException('OUTPUT_COMPLETION');
        }
    },
    true,
);
$registry = new HandlerRegistry();
$registry->register($scopeHandler);
$dispatcher = new Dispatcher($registry, RuntimeProfile::longLived([Protocol::TRANSACTION_PROVIDER]), $contexts, $transactions);

$invoke = static fn (string $id, string $delegation): Invocation => new Invocation(
    "request-{$id}",
    "invocation-{$id}",
    "attempt-{$id}",
    'fixture.scope',
    'schema-1',
    1_800_000_000_000,
    $delegation,
    new ObjectValue(),
);

$first = $dispatcher->dispatch($invoke('1', 'tenant-a:alice'));
$second = $dispatcher->dispatch($invoke('2', 'tenant-b:bob'));
$failed = $dispatcher->dispatch($invoke('3', 'tenant-error:eve'));
serverCheck(Json::encode($first->data) === '{"tenant":"tenant-a"}', 'first tenant');
serverCheck(Json::encode($second->data) === '{"tenant":"tenant-b"}', 'second tenant');
serverCheck($seen === [['tenant-a', 'tenant-a'], ['tenant-b', 'tenant-b'], ['tenant-error', 'tenant-error']], 'request loader isolation');
serverCheck(count($contexts->created) === 3 && count($transactions->states) === 3, 'request transaction isolation');
serverCheck($transactions->states[0]->commits === 1 && $transactions->states[1]->commits === 1, 'transactions committed independently');
serverCheck($transactions->states[2]->rollbacks === 1 && $failed->errors !== [], 'application failure rolled back');
foreach ($contexts->created as $context) {
    serverRejects(static fn (): Principal => $context->principal(), 'REMOTE_CONTEXT_CLOSED');
}

$calls = 0;
$streamRegistry = new HandlerRegistry();
$streamRegistry->register(new RegisteredHandler(
    'fixture.stream',
    'StreamInput',
    'StreamOutput',
    Effect::Subscription,
    [Protocol::SERVER_STREAMING],
    static fn (ObjectValue $input): object => (object) ['input' => $input],
    static function (object $_input, RequestContext $_context) use (&$calls): object { $calls++; return new stdClass(); },
    static fn (object $_output): ObjectValue => new ObjectValue(),
    static function (ObjectValue $_output): void {},
));
$fpm = new Dispatcher($streamRegistry, RuntimeProfile::fpmUnary(), $contexts);
$streamInvocation = new Invocation('request-stream', 'invocation-stream', 'attempt-stream', 'fixture.stream', 'schema-1', 1_800_000_000_000, 'tenant-a:alice', new ObjectValue());
serverRejects(static fn (): mixed => $fpm->dispatch($streamInvocation), 'REMOTE_CAPABILITY_MISMATCH');
serverCheck($calls === 0 && RuntimeProfile::fpmUnary()->capabilities === [Protocol::UNARY], 'FPM rejects streaming before source');

$generatedRegistry = new HandlerRegistry();
$generatedRegistry->register(GetAccountHandlerBinding::register(new class implements GetAccountHandler {
    public function handle(GetAccountHandlerInput $input, RequestContext $_context): GetAccountHandlerOutput
    {
        return new GetAccountHandlerOutput(new GetAccountHandlerProfileOutput($input->id, Selected::null()), Selected::pending());
    }
}));
$generatedDispatcher = new Dispatcher($generatedRegistry, RuntimeProfile::fpmUnary(), $contexts);
$generated = $generatedDispatcher->dispatch(new Invocation(
    'request-generated',
    'invocation-generated',
    'attempt-generated',
    'GetAccount',
    'schema-1',
    1_800_000_000_000,
    'tenant-a:alice',
    new ObjectValue([new MapEntry('id', 'acct-1')]),
));
serverCheck(Json::encode($generated->data) === '{"profile":{"display":"acct-1","nickname":null}}', 'generated handler contracts');

$workerProfile = RuntimeProfile::longLived();
$envelopeServer = new EnvelopeServer(
    $generatedRegistry,
    new Dispatcher($generatedRegistry, $workerProfile, $contexts),
    $workerProfile,
    'fixture-worker',
    'spiffe://example/fixture-worker',
    'naatre-gateway',
    'fixture-stdio',
    'schema-1',
    str_repeat('a', 64),
    'php-session-1',
);
$registered = $envelopeServer->handle(ObjectValue::fromPairs([
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
        ['capabilities', new ListValue([Protocol::UNARY, Protocol::CANCELLATION_ACK])],
        ['limits', ObjectValue::fromPairs([
            ['maxInFlight', 1], ['maxRequestBytes', 4096], ['maxResponseBytes', 4096], ['maxStreamFrames', 4], ['maxStreamBytes', 16_384],
        ])],
        ['handlers', new ListValue(array_map(static fn (RegisteredHandler $handler): ObjectValue => $handler->descriptor(), $generatedRegistry->all()))],
    ])],
]));
serverCheck($registered->selected('kind')->value === 'registered', 'long-lived registration');
$cancelled = $envelopeServer->handle(ObjectValue::fromPairs([
    ['protocol', Protocol::VERSION],
    ['kind', 'cancel'],
    ['payload', ObjectValue::fromPairs([['requestId', 'request-generated'], ['invocationId', 'invocation-generated']])],
]));
serverCheck($cancelled->selected('kind')->value === 'cancelled', 'long-lived cancellation acknowledgement');

$ownedInput = fopen('php://temp', 'r+');
$ownedOutput = fopen('php://temp', 'r+');
serverCheck(is_resource($ownedInput) && is_resource($ownedOutput), 'owned streams opened');
(new FramedWorker($envelopeServer, $ownedInput, $ownedOutput, ownsStreams: true))->run();
serverCheck(!is_resource($ownedInput) && !is_resource($ownedOutput), 'owned streams closed at shutdown');

$invalidRegistry = new HandlerRegistry();
$invalidRegistry->register(new RegisteredHandler(
    'fixture.invalid', 'Input', 'Output', Effect::Query, [],
    static fn (ObjectValue $_input): object => new stdClass(),
    static fn (object $_input, RequestContext $_context): object => new stdClass(),
    static fn (object $_output): ObjectValue => new ObjectValue(),
    static function (ObjectValue $_output): void { throw new ServerException('OUTPUT_COMPLETION'); },
));
$invalidDispatcher = new Dispatcher($invalidRegistry, RuntimeProfile::fpmUnary(), $contexts);
$invalidInvocation = new Invocation('request-invalid', 'invocation-invalid', 'attempt-invalid', 'fixture.invalid', 'schema-1', 1_800_000_000_000, 'tenant-a:alice', new ObjectValue());
serverRejects(static fn (): mixed => $invalidDispatcher->dispatch($invalidInvocation), 'OUTPUT_COMPLETION');

$numeric = Json::decode('{"empty":{},"list":[],"0":"zero","missingOrNull":null,"large":18446744073709551615}');
serverCheck($numeric instanceof ObjectValue, 'shared vector object');
serverCheck(Json::encode($numeric) === '{"0":"zero","empty":{},"large":"18446744073709551615","list":[],"missingOrNull":null}', 'shared vectors preserve wire shapes');
serverCheck($numeric->selected('missing')->presence === Presence::Missing && $numeric->selected('missingOrNull')->presence === Presence::Null, 'missing and null distinct');

fwrite(STDOUT, json_encode(['profile' => 'sdk.php.server-1', 'status' => 'passed'], JSON_THROW_ON_ERROR) . "\n");
