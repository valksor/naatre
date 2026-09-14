<?php

declare(strict_types=1);

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Generated\GetAccountResult;
use Naatre\Sdk\Generated\GetAccountVariables;
use Naatre\Sdk\Generated\Operations;
use Naatre\Sdk\Generated\Status;
use Naatre\Sdk\Pagination\Page;
use Naatre\Sdk\Pagination\Paginator;
use Naatre\Sdk\Scalar\Bytes;
use Naatre\Sdk\Scalar\Decimal;
use Naatre\Sdk\Scalar\Int64;
use Naatre\Sdk\Scalar\Timestamp;
use Naatre\Sdk\Scalar\UInt64;
use Naatre\Sdk\Stream\CloseableStream;
use Naatre\Sdk\Tests\TestFactory;
use Naatre\Sdk\Tests\TestHttpClient;
use Naatre\Sdk\Tests\TestResponse;
use Naatre\Sdk\Transport\Psr18Client;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\OpenUnionValue;
use Naatre\Sdk\Value\Presence;
use Naatre\Sdk\Value\Selected;
use Naatre\Sdk\Wire\Json;
require __DIR__ . '/bootstrap.php';

function check(bool $condition, string $message): void
{
    if (!$condition) {
        throw new RuntimeException($message);
    }
}

function rejects(callable $function, string $code): void
{
    try {
        $function();
    } catch (ClientException $error) {
        check($error->errorCode === $code, "expected {$code}, got {$error->errorCode}");
        return;
    }
    throw new RuntimeException("expected {$code}");
}

$fixturePath = dirname(__DIR__, 3) . '/conformance/v1/php-sdk.json';
$fixture = json_decode((string) file_get_contents($fixturePath), true, 512, JSON_THROW_ON_ERROR);
check($fixture['profile'] === 'sdk.php.core-1', 'profile');

check((string) new Int64('-9223372036854775808') === '-9223372036854775808', 'int64 minimum');
check((string) new UInt64('18446744073709551615') === '18446744073709551615', 'uint64 maximum');
rejects(static fn (): Int64 => new Int64('9223372036854775808'), 'CLIENT_SCALAR_INVALID');
rejects(static fn (): UInt64 => new UInt64('18446744073709551616'), 'CLIENT_SCALAR_INVALID');
check((string) new Decimal('001.2300') === '1.23', 'decimal canonicalization');
check((string) new Timestamp($fixture['wireFixtures']['highPrecisionTimestamp']['input']) === $fixture['wireFixtures']['highPrecisionTimestamp']['canonical'], 'timestamp precision');
check((string) new Timestamp('0000-02-29T00:30:00+01:00') === '0000-02-28T23:30:00Z', 'timestamp proleptic leap year');
check(Bytes::fromWire((new Bytes("\x00\xff"))->wire())->bytes === "\x00\xff", 'bytes round trip');

$emptyObject = new ObjectValue();
$emptyList = new ListValue();
$numericObject = ObjectValue::fromPairs([['0', 'x']]);
check(Json::encode($emptyObject) === $fixture['wireFixtures']['emptyObject'], 'empty object');
check(Json::encode($emptyList) === $fixture['wireFixtures']['emptyList'], 'empty list');
check(Json::encode($numericObject) === $fixture['wireFixtures']['numericStringObject'], 'numeric-string object key');
check(Json::encode(Json::decode($fixture['wireFixtures']['numericStringObject'])) === $fixture['wireFixtures']['numericStringObject'], 'numeric-string decode identity');
rejects(static fn (): mixed => Json::decode('{"a":1,"\\u0061":2}'), 'CLIENT_JSON_DUPLICATE_KEY');

check(Selected::missing()->presence === Presence::Missing, 'missing state');
check(Selected::null()->presence === Presence::Null, 'null state');
check(Selected::pending()->presence === Presence::Pending, 'pending state');
check(Selected::failed([['code' => 'FAILED']])->presence === Presence::Failed, 'failed state');
check(Selected::skipped('condition')->presence === Presence::Skipped, 'skipped state');
$futureStatus = Status::fromWire('FUTURE');
check(!$futureStatus->value->known && $futureStatus->value->raw === 'FUTURE', 'unknown enum');
$unknown = new OpenUnionValue('FutureAccount', $numericObject, false);
check(!$unknown->known && $unknown->discriminator === 'FutureAccount', 'unknown union');

$variables = new GetAccountVariables(
    'acct-1',
    Selected::null(),
    Selected::present(new ListValue()),
    Selected::present(new ObjectValue()),
);
$operation = Operations::getAccount($variables);
check($operation->persistedDigest === $fixture['persisted']['digest'], 'persisted digest');
check($operation->canonicalRequest() === $fixture['wireFixtures']['canonicalRequest'], 'canonical request');

$partial = \Naatre\Sdk\Protocol\OperationResult::decode(
    '{"complete":false,"data":{"profile":{"display":"Ada"}},"errors":[{"code":"PARTIAL"}]}',
    $operation,
);
check($partial->data instanceof GetAccountResult && $partial->data->profile->presence === Presence::Present, 'partial data');
check(count($partial->errors) === 1 && !$partial->complete, 'partial errors');
check($partial->data->later->presence === Presence::Pending, 'pending field');

$factory = new TestFactory();
$response = new TestResponse(['content-type' => Psr18Client::RESPONSE_MEDIA_TYPE], '{"complete":true,"data":{"profile":{"display":"Ada"}}}');
$http = new TestHttpClient($response);
$client = new Psr18Client(
    'https://example.test/v1/execute',
    $http,
    $factory,
    $factory,
    static fn (): array => ['Authorization' => 'Bearer local-fixture'],
);
$unary = $client->execute($operation, 1250);
check($unary->complete && $http->request?->getHeaderLine('Naatre-Timeout-Ms') === '1250', 'deadline header');
check($http->request?->getHeaderLine('Authorization') === 'Bearer local-fixture', 'auth hook');
check(!$response->body->isReadable(), 'response body closed');

$pages = new Paginator(static fn (?string $cursor): Page => $cursor === null ? new Page(['a'], 'next') : new Page(['b'], null));
check(iterator_to_array($pages->items()) === ['a', 'b'], 'pagination');

$reasons = [];
$cancelled = new CloseableStream(new ArrayIterator([]), static function (string $reason) use (&$reasons): void { $reasons[] = $reason; });
$cancelled->close();
$cancelled->close();
$timedOut = new CloseableStream(new ArrayIterator([]), static function (string $reason) use (&$reasons): void { $reasons[] = $reason; });
$timedOut->timeout();
$timedOut->timeout();
$completed = new CloseableStream(new ArrayIterator(['x']), static function (string $reason) use (&$reasons): void { $reasons[] = $reason; });
foreach ($completed as $_) {}
$emptyCompleted = new CloseableStream(new ArrayIterator([]), static function (string $reason) use (&$reasons): void { $reasons[] = $reason; });
foreach ($emptyCompleted as $_) {}
$failingSource = new class implements Iterator {
    public function current(): mixed { return 'x'; }
    public function key(): int { return 0; }
    public function next(): void { throw new RuntimeException('fixture failure'); }
    public function rewind(): void {}
    public function valid(): bool { return true; }
};
$failedStream = new CloseableStream($failingSource, static function (string $reason) use (&$reasons): void { $reasons[] = $reason; });
rejects(static function () use ($failedStream): void { $failedStream->next(); }, 'CLIENT_STREAM_ERROR');
$abandoned = new CloseableStream(new ArrayIterator(['x']), static function (string $reason) use (&$reasons): void { $reasons[] = $reason; });
unset($abandoned);
gc_collect_cycles();
unset($cancelled, $timedOut, $completed, $emptyCompleted, $failedStream);
sort($reasons);
check($reasons === ['abandoned', 'cancelled', 'complete', 'complete', 'error', 'timeout'], 'stream closure reasons');

$sourceBefore = (string) file_get_contents(dirname(__DIR__) . '/generated/Operations.php');
$manifestBefore = (string) file_get_contents(dirname(__DIR__) . '/generated/operations.json');
$temporarySource = tempnam(sys_get_temp_dir(), 'naatre-php-source-');
$temporaryManifest = tempnam(sys_get_temp_dir(), 'naatre-php-manifest-');
check($temporarySource !== false && $temporaryManifest !== false, 'temporary generator output');
$command = sprintf(
    '%s %s %s %s %s %s',
    escapeshellarg(PHP_BINARY),
    escapeshellarg(dirname(__DIR__) . '/sdkgen/generate.php'),
    escapeshellarg(dirname(__DIR__, 3) . '/conformance/v1/generator-model.json'),
    escapeshellarg(dirname(__DIR__, 3) . '/conformance/v1/generator-output.json'),
    escapeshellarg($temporarySource),
    escapeshellarg($temporaryManifest),
);
exec($command, $generatorOutput, $generatorStatus);
check($generatorStatus === 0, 'generator execution');
check((string) file_get_contents($temporarySource) === $sourceBefore, 'byte-identical source');
check((string) file_get_contents($temporaryManifest) === $manifestBefore, 'byte-identical manifest');
unlink($temporarySource);
unlink($temporaryManifest);

$model = json_decode((string) file_get_contents(dirname(__DIR__, 3) . '/conformance/v1/generator-model.json'), true, 512, JSON_THROW_ON_ERROR);
unset($model['configuration']['scalarMappings']['Money']);
$invalidModel = tempnam(sys_get_temp_dir(), 'naatre-php-model-');
$invalidSource = tempnam(sys_get_temp_dir(), 'naatre-php-source-');
$invalidManifest = tempnam(sys_get_temp_dir(), 'naatre-php-manifest-');
check($invalidModel !== false && $invalidSource !== false && $invalidManifest !== false, 'temporary invalid generator output');
file_put_contents($invalidModel, json_encode($model, JSON_THROW_ON_ERROR));
$invalidCommand = sprintf(
    '%s %s %s %s %s %s',
    escapeshellarg(PHP_BINARY),
    escapeshellarg(dirname(__DIR__) . '/sdkgen/generate.php'),
    escapeshellarg($invalidModel),
    escapeshellarg(dirname(__DIR__, 3) . '/conformance/v1/generator-output.json'),
    escapeshellarg($invalidSource),
    escapeshellarg($invalidManifest),
);
exec($invalidCommand . ' 2>/dev/null', $invalidOutput, $invalidStatus);
check($invalidStatus !== 0, 'unmapped scalar rejected');
unlink($invalidModel);
unlink($invalidSource);
unlink($invalidManifest);

fwrite(STDOUT, json_encode(['profile' => $fixture['profile'], 'status' => 'passed'], JSON_THROW_ON_ERROR) . "\n");
