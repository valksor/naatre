<?php

declare(strict_types=1);

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Integration\Laravel\NaatreClientFactory as LaravelClientFactory;
use Naatre\Sdk\Integration\RequestScopedClientProvider;
use Naatre\Sdk\Integration\Symfony\NaatreClientFactory as SymfonyClientFactory;
use Naatre\Sdk\Protocol\Operation;
use Naatre\Sdk\Tests\TestFactory;
use Naatre\Sdk\Tests\TestResponse;
use Naatre\Sdk\Tests\TestStream;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;
use Naatre\Sdk\Wire\Json;
use Psr\Http\Client\ClientInterface;
use Psr\Http\Message\RequestFactoryInterface;
use Psr\Http\Message\StreamFactoryInterface;

require __DIR__ . '/bootstrap.php';

function adapterCheck(bool $condition, string $message): void
{
    if ($condition) {
        return;
    }
    throw new RuntimeException("adapter fixture failed: {$message}");
}

/** @return ClientException */
function adapterRejects(callable $function, string $code): ClientException
{
    try {
        $function();
    } catch (ClientException $error) {
        adapterCheck($error->errorCode === $code, "expected {$code}, got {$error->errorCode}");
        adapterCheck($error->getMessage() === $code, 'public message is the stable code');
        return $error;
    }
    throw new RuntimeException("expected {$code}");
}

/** @return Operation */
function adapterOperation(ObjectValue $variables): Operation
{
    return new Operation(
        'AdapterFixture',
        'query',
        str_repeat('a', 64),
        $variables,
        static fn (ObjectValue $data): ObjectValue => $data,
    );
}

/** @return array{0: ObjectValue, 1: ObjectValue} */
function adapterVariables(): array
{
    return [
        new ObjectValue([
            new MapEntry('nullable', null),
            new MapEntry('numericMap', new ObjectValue([new MapEntry('0', 'first')])),
        ]),
        new ObjectValue([
            new MapEntry('numericMap', new ObjectValue([new MapEntry('0', 'second')])),
        ]),
    ];
}

/** @param class-string<RequestScopedClientProvider> $factoryClass */
function exerciseRequestIsolation(string $factoryClass): void
{
    $responseBody = '{"complete":true,"data":{"nullable":null,"numericMap":{"0":"response"}}}';
    $responseHeaders = ['content-type' => 'application/vnd.naatre.response+json;version=1'];
    $http = new TestFactory([
        new TestResponse($responseHeaders, $responseBody),
        new TestResponse($responseHeaders, $responseBody),
    ]);
    $psrFactory = new TestFactory();
    $factory = new $factoryClass(
        'https://example.test/v1/execute',
        $http,
        $psrFactory,
        $psrFactory,
    );
    [$firstVariables, $secondVariables] = adapterVariables();
    $first = $factory->forRequest(static fn (): array => ['Authorization' => 'Bearer request-one']);
    $second = $factory->forRequest(static fn (): array => ['Authorization' => 'Bearer request-two']);
    $firstResult = $first->execute(adapterOperation($firstVariables), 1);
    $secondResult = $second->execute(adapterOperation($secondVariables), 86_400_000);

    adapterCheck(count($http->requests) === 2, 'two isolated requests');
    adapterCheck($http->requests[0]->getHeaderLine('Authorization') === 'Bearer request-one', 'first credential isolated');
    adapterCheck($http->requests[1]->getHeaderLine('Authorization') === 'Bearer request-two', 'second credential isolated');
    adapterCheck($http->requests[0]->getHeaderLine('Naatre-Timeout-Ms') === '1', 'minimum deadline');
    adapterCheck($http->requests[1]->getHeaderLine('Naatre-Timeout-Ms') === '86400000', 'maximum deadline');

    $firstWire = Json::decode((string) $http->requests[0]->getBody());
    $secondWire = Json::decode((string) $http->requests[1]->getBody());
    adapterCheck($firstWire instanceof ObjectValue && $secondWire instanceof ObjectValue, 'request objects');
    $firstEncodedVariables = $firstWire->selected('variables')->value;
    $secondEncodedVariables = $secondWire->selected('variables')->value;
    adapterCheck($firstEncodedVariables instanceof ObjectValue && $secondEncodedVariables instanceof ObjectValue, 'variable objects');
    adapterCheck($firstEncodedVariables->selected('nullable')->presence === Presence::Null, 'explicit null preserved');
    adapterCheck($secondEncodedVariables->selected('nullable')->presence === Presence::Missing, 'missing preserved');
    adapterCheck(Json::encode($firstEncodedVariables->selected('numericMap')->value) === '{"0":"first"}', 'first numeric map key');
    adapterCheck(Json::encode($secondEncodedVariables->selected('numericMap')->value) === '{"0":"second"}', 'second numeric map key');
    adapterCheck($firstResult->data instanceof ObjectValue && $firstResult->data->selected('nullable')->presence === Presence::Null, 'response null preserved');
    adapterCheck($secondResult->data instanceof ObjectValue && $secondResult->data->selected('missing')->presence === Presence::Missing, 'response missing preserved');
}

/** @param class-string<RequestScopedClientProvider> $factoryClass */
function exerciseFpmIsolation(string $factoryClass): void
{
    $responseBody = '{"complete":true,"data":{}}';
    $responseHeaders = ['content-type' => 'application/vnd.naatre.response+json;version=1'];
    $psrFactory = new TestFactory();
    foreach (['fpm-one', 'fpm-two'] as $token) {
        $http = new TestFactory([new TestResponse($responseHeaders, $responseBody)]);
        $factory = new $factoryClass(
            'https://example.test/v1/execute',
            $http,
            $psrFactory,
            $psrFactory,
        );
        $factory->forRequest(static fn (): array => ['Authorization' => "Bearer {$token}"])
            ->execute(adapterOperation(new ObjectValue()));
        adapterCheck($http->requests[0]->getHeaderLine('Authorization') === "Bearer {$token}", 'FPM credential isolated');
    }
}

$fixturePath = dirname(__DIR__, 3) . '/conformance/v1/php-adapters.json';
$fixture = json_decode((string) file_get_contents($fixturePath), true, 512, JSON_THROW_ON_ERROR);
adapterCheck($fixture['profile'] === 'sdk.php.adapters-1', 'adapter profile');
adapterCheck($fixture['runtimeBoundary']['executedFixtureSapi'] === 'cli-model', 'lifecycle fixture boundary');

// Reusing each framework factory models a persistent worker. Constructing it per
// call models FPM; both fixtures stay deterministic and listener-free.
exerciseRequestIsolation(SymfonyClientFactory::class);
exerciseRequestIsolation(LaravelClientFactory::class);
exerciseFpmIsolation(SymfonyClientFactory::class);
exerciseFpmIsolation(LaravelClientFactory::class);

$bridgeResponse = new TestResponse(
    ['content-type' => 'application/vnd.naatre.response+json;version=1'],
    '{"complete":true,"data":{}}',
);
$bridgeFactory = new TestFactory([$bridgeResponse]);
$symfonyBridge = SymfonyClientFactory::fromPsr18Bridge(
    'https://example.test/v1/execute',
    $bridgeFactory,
);
adapterCheck($symfonyBridge->forRequest()->execute(adapterOperation(new ObjectValue()))->complete, 'Symfony PSR bridge');

$containerHTTP = new TestFactory([new TestResponse(
    ['content-type' => 'application/vnd.naatre.response+json;version=1'],
    '{"complete":true,"data":{}}',
)]);
$containerFactory = new TestFactory();
$container = new class($containerHTTP, $containerFactory) {
    public function __construct(
        private readonly ClientInterface $http,
        private readonly TestFactory $factory,
    ) {
    }

    public function make(string $id): object
    {
        return match ($id) {
            ClientInterface::class => $this->http,
            RequestFactoryInterface::class, StreamFactoryInterface::class => $this->factory,
            default => throw new RuntimeException('unsupported fixture service'),
        };
    }
};
$laravelContainer = LaravelClientFactory::fromContainer($container, 'https://example.test/v1/execute');
adapterCheck($laravelContainer->forRequest()->execute(adapterOperation(new ObjectValue()))->complete, 'Laravel container bridge');
adapterRejects(
    static fn (): mixed => LaravelClientFactory::fromContainer(new stdClass(), 'https://example.test/v1/execute'),
    'CLIENT_CONFIGURATION_INVALID',
);

$factory = new TestFactory();
$validBody = '{"complete":true,"data":{}}';
$headers = ['content-type' => 'application/vnd.naatre.response+json;version=1'];
$exactResponse = new TestResponse($headers, $validBody);
$exactHTTP = new TestFactory([$exactResponse]);
$exactClient = new SymfonyClientFactory(
    'https://example.test/v1/execute',
    $exactHTTP,
    $factory,
    $factory,
    strlen($validBody),
);
adapterCheck($exactClient->forRequest()->execute(adapterOperation(new ObjectValue()))->complete, 'response at byte limit');

$largeBody = '{"complete":true,"data":{"value":"' . str_repeat('x', 128) . '"}}';
$largeResponse = new TestResponse($headers, $largeBody);
$largeClient = new LaravelClientFactory(
    'https://example.test/v1/execute',
    new TestFactory([$largeResponse]),
    $factory,
    $factory,
    strlen($validBody),
);
adapterRejects(static fn (): mixed => $largeClient->forRequest()->execute(adapterOperation(new ObjectValue())), 'CLIENT_RESPONSE_TOO_LARGE');
adapterCheck(!$largeResponse->body->isReadable(), 'oversized response closed');

$mediaResponse = new TestResponse(['content-type' => 'text/plain'], $validBody);
$mediaClient = new SymfonyClientFactory(
    'https://example.test/v1/execute',
    new TestFactory([$mediaResponse]),
    $factory,
    $factory,
);
adapterRejects(static fn (): mixed => $mediaClient->forRequest()->execute(adapterOperation(new ObjectValue())), 'CLIENT_MEDIA_TYPE_INVALID');
adapterCheck(!$mediaResponse->body->isReadable(), 'invalid media response closed');

$stalled = new TestStream('', stall: true);
$stalledResponse = new TestResponse($headers, $stalled);
$stalledClient = new SymfonyClientFactory(
    'https://example.test/v1/execute',
    new TestFactory([$stalledResponse]),
    $factory,
    $factory,
);
adapterRejects(static fn (): mixed => $stalledClient->forRequest()->execute(adapterOperation(new ObjectValue())), 'CLIENT_RESPONSE_STALLED');
adapterCheck(!$stalled->isReadable(), 'stalled response closed');

$closeFailure = new TestStream($validBody, failClose: true);
$closeFailureResponse = new TestResponse($headers, $closeFailure);
$closeFailureClient = new LaravelClientFactory(
    'https://example.test/v1/execute',
    new TestFactory([$closeFailureResponse]),
    $factory,
    $factory,
);
$closeError = adapterRejects(
    static fn (): mixed => $closeFailureClient->forRequest()->execute(adapterOperation(new ObjectValue())),
    'CLIENT_TRANSPORT_ERROR',
);
adapterCheck($closeError->getPrevious() === null, 'cleanup detail not chained');
adapterCheck(!str_contains($closeError->getMessage(), 'protected-close-detail'), 'cleanup detail redacted');

$transport = new TestFactory(
    new TestResponse($headers, $validBody),
    1,
    'Bearer protected-transport-detail',
);
$transportClient = new LaravelClientFactory('https://example.test/v1/execute', $transport, $factory, $factory);
$transportFailure = adapterRejects(
    static fn (): mixed => $transportClient->forRequest()->execute(adapterOperation(new ObjectValue())),
    'CLIENT_TRANSPORT_ERROR',
);
adapterCheck($transportFailure->getPrevious() === null, 'transport detail not chained');
adapterCheck(!str_contains($transportFailure->getMessage(), 'protected-transport-detail'), 'transport detail redacted');

$authClient = new SymfonyClientFactory(
    'https://example.test/v1/execute',
    new TestFactory([new TestResponse($headers, $validBody)]),
    $factory,
    $factory,
);
$authFailure = adapterRejects(
    static fn (): mixed => $authClient->forRequest(static function (): array {
        throw new RuntimeException('Bearer protected-auth-detail');
    })->execute(adapterOperation(new ObjectValue())),
    'CLIENT_AUTHENTICATION_ERROR',
);
adapterCheck($authFailure->getPrevious() === null, 'authentication detail not chained');
adapterCheck(!str_contains($authFailure->getMessage(), 'protected-auth-detail'), 'authentication detail redacted');

adapterRejects(
    static fn (): mixed => $authClient->forRequest(static fn (): array => ['Content-Type' => 'text/plain'])
        ->execute(adapterOperation(new ObjectValue())),
    'CLIENT_AUTH_HEADER_INVALID',
);
adapterRejects(
    static fn (): mixed => $authClient->forRequest(static fn (): array => ['X-Value' => "safe\r\nAuthorization: leaked"])
        ->execute(adapterOperation(new ObjectValue())),
    'CLIENT_AUTH_HEADER_INVALID',
);
adapterRejects(
    static fn (): mixed => $authClient->forRequest()->execute(adapterOperation(new ObjectValue()), 0),
    'CLIENT_DEADLINE_INVALID',
);
adapterRejects(
    static fn (): mixed => $authClient->forRequest()->execute(adapterOperation(new ObjectValue()), 86_400_001),
    'CLIENT_DEADLINE_INVALID',
);
adapterRejects(
    static fn (): mixed => $authClient->forRequest()->execute(adapterOperation(new ObjectValue()), maximumAttempts: 9),
    'CLIENT_RETRY_INVALID',
);

$retryResponse = new TestResponse($headers, $validBody);
$retryHTTP = new TestFactory($retryResponse, 7);
$retryClient = new LaravelClientFactory('https://example.test/v1/execute', $retryHTTP, $factory, $factory);
adapterCheck(
    $retryClient->forRequest()->execute(adapterOperation(new ObjectValue()), maximumAttempts: 8)->complete,
    'maximum query retry succeeds',
);
adapterCheck($retryHTTP->attempts === 8, 'maximum query retry attempts');

$mutation = new Operation('MutationFixture', 'mutation', str_repeat('b', 64), new ObjectValue(), static fn (ObjectValue $data): ObjectValue => $data);
adapterRejects(
    static fn (): mixed => $authClient->forRequest()->execute($mutation, maximumAttempts: 2),
    'CLIENT_RETRY_INVALID',
);

adapterCheck($fixture['profiles'][0]['activeAbort'] === false, 'no active abort claim');
adapterCheck($fixture['unsupported'] !== [], 'unsupported capabilities enumerated');

fwrite(STDOUT, json_encode([
    'profile' => $fixture['profile'],
    'status' => 'passed',
    'runtime' => PHP_VERSION,
    'dependencyRevisions' => $fixture['dependencyRevisions'],
    'vectors' => $fixture['fixtures'],
], JSON_THROW_ON_ERROR) . "\n");
