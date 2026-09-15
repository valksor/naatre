<?php

declare(strict_types=1);

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Native\Accelerator;
use Naatre\Sdk\Native\Limits;
use Naatre\Sdk\Native\Mode;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Wire\Json;

require __DIR__ . '/bootstrap.php';

function nativeCheck(bool $condition, string $message): void
{
    $condition || throw new RuntimeException($message);
}

function nativeError(callable $operation): string
{
    try {
        $operation();
    } catch (ClientException $error) {
        return $error->errorCode;
    }
    throw new RuntimeException('expected ClientException');
}

function nativeExtensionError(callable $operation): string
{
    try {
        $operation();
    } catch (Throwable $error) {
        return $error->getMessage();
    }
    throw new RuntimeException('expected extension exception');
}

$root = dirname(__DIR__, 3);
$canonicalFixture = json_decode((string) file_get_contents($root . '/conformance/v1/canonical.json'), true, 512, JSON_THROW_ON_ERROR);
$limits = new Limits(maximumDepth: 128, maximumNodes: 1_000_000, maximumBytes: 16_777_216, maximumOutputBytes: 16_777_216);

Accelerator::configure(Mode::PurePhp);
foreach ($canonicalFixture['jsonVectors'] as $vector) {
    nativeCheck(Json::canonicalize($vector['input'], $limits) === $vector['canonical'], 'pure canonical vector ' . $vector['name']);
}
foreach ($canonicalFixture['hashVectors'] as $vector) {
    nativeCheck(Json::semanticHash($vector['purpose'], $vector['input'], $limits) === $vector['digest'], 'pure hash vector ' . $vector['name']);
}
nativeCheck(Json::encode(new ObjectValue()) === '{}', 'pure empty object');
nativeCheck(Json::encode(new ListValue()) === '[]', 'pure empty list');
nativeCheck(Accelerator::diagnostics()['selectedMode'] === 'pure-php', 'forced pure diagnostics');

Accelerator::reset();
nativeCheck(Accelerator::diagnostics()['selectedMode'] === 'pure-php', 'auto defaults to measured-safe pure path');

if (!extension_loaded('naatre')) {
    nativeCheck(nativeError(static fn () => Accelerator::configure(Mode::Native)) === 'CLIENT_NATIVE_UNAVAILABLE', 'missing extension diagnostic');
    Accelerator::reset();
    fwrite(STDOUT, json_encode(['profile' => 'sdk.php.native-1', 'status' => 'fallback-passed'], JSON_THROW_ON_ERROR) . "\n");
    exit(0);
}

$metadata = naatre_native_info();
nativeCheck($metadata['phpVersionId'] === PHP_VERSION_ID, 'exact runtime version metadata');
nativeCheck($metadata['defaultEnabled'] === [], 'no primitive enabled before benchmark evidence');
nativeCheck($metadata['preemptiveCancellation'] === false, 'no cancellation claim');
nativeCheck($metadata['cacheOwnership'] === 'request-only-no-persistent-cache', 'request-only ownership');

Accelerator::configure(Mode::Native);
foreach ($canonicalFixture['jsonVectors'] as $vector) {
    nativeCheck(Json::canonicalize($vector['input'], $limits) === $vector['canonical'], 'native canonical vector ' . $vector['name']);
}
foreach ($canonicalFixture['hashVectors'] as $vector) {
    nativeCheck(Json::semanticHash($vector['purpose'], $vector['input'], $limits) === $vector['digest'], 'native hash vector ' . $vector['name']);
}

$wireValues = ['{}', '[]', '{"0":[]}', '{"a":null,"b":[true,"x",1.25]}'];
foreach ($wireValues as $wire) {
    Accelerator::configure(Mode::PurePhp);
    $pure = Json::encode(Json::decode($wire));
    Accelerator::configure(Mode::Native);
    $native = Json::encode(Json::decode($wire));
    nativeCheck($native === $pure, 'forced-mode value parity ' . $wire);
}

Accelerator::configure(Mode::PurePhp);
$pureDuplicate = nativeError(static fn () => Json::decode('{"a":1,"\\u0061":2}'));
Accelerator::configure(Mode::Native);
$nativeDuplicate = nativeError(static fn () => Json::decode('{"a":1,"\\u0061":2}'));
nativeCheck($nativeDuplicate === $pureDuplicate, 'duplicate-key error parity');
$limitCases = [
    ['CLIENT_JSON_DEPTH_EXCEEDED', static fn () => Json::decode('{"a":{"b":1}}', limits: new Limits(maximumDepth: 2))],
    ['CLIENT_JSON_NODE_LIMIT_EXCEEDED', static fn () => Json::decode('[1,2]', limits: new Limits(maximumNodes: 2))],
    ['CLIENT_RESPONSE_TOO_LARGE', static fn () => Json::decode('{"value":1}', limits: new Limits(maximumBytes: 4))],
    ['CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED', static fn () => Json::encode(ObjectValue::fromPairs([['value', 'long']]), new Limits(maximumOutputBytes: 4))],
    ['CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED', static fn () => Json::encode(str_repeat('x', 1_048_576), new Limits(maximumOutputBytes: 4))],
];
foreach ($limitCases as [$expected, $operation]) {
    Accelerator::configure(Mode::PurePhp);
    nativeCheck(nativeError($operation) === $expected, 'pure resource limit ' . $expected);
    Accelerator::configure(Mode::Native);
    nativeCheck(nativeError($operation) === $expected, 'native resource limit ' . $expected);
}

$encodedValue = ObjectValue::fromPairs([['value', 'long']]);
$encodeLimits = new Limits(maximumBytes: 1, maximumOutputBytes: 64);
Accelerator::configure(Mode::PurePhp);
$pureEncoded = Json::encode($encodedValue, $encodeLimits);
Accelerator::configure(Mode::Native);
nativeCheck(Json::encode($encodedValue, $encodeLimits) === $pureEncoded, 'encode ignores wire-input byte limit in both modes');

$batch = naatre_native_batch_canonicalize(['{"b":2,"a":1}', '[]'], ...$limits->arguments());
nativeCheck($batch === ['{"a":1,"b":2}', '[]'], 'bounded batch canonicalization');
nativeCheck(
    nativeExtensionError(static fn () => naatre_native_batch_canonicalize(['0', '0'], 16, 1, 2, 2)) === 'CLIENT_JSON_NODE_LIMIT_EXCEEDED',
    'batch allocation obeys aggregate node limit',
);

mt_srand(110);
for ($case = 0; $case < 256; ++$case) {
    $value = ObjectValue::fromPairs([
        ['0', mt_rand(-1000, 1000)],
        ['text', 'case-' . $case],
        ['nested', new ListValue([null, (bool) ($case % 2), new ObjectValue()])],
    ]);
    Accelerator::configure(Mode::PurePhp);
    $pure = Json::encode($value);
    Accelerator::configure(Mode::Native);
    nativeCheck(Json::encode($value) === $pure, 'property encode parity ' . $case);
    nativeCheck(Json::encode(Json::decode($pure)) === $pure, 'property decode parity ' . $case);
}

$schemaInput = '{"revision":"r1","types":[]}';
$operationInput = '{"operations":[]}';
$schema = naatre_native_parse('schema', $schemaInput, ...$limits->arguments());
$operation = naatre_native_parse('operation', $operationInput, ...$limits->arguments());
$plan = naatre_native_compile($schema, $operation, ...$limits->arguments());
nativeCheck($schema->kind() === 'schema' && $schema->nodeCount() === 3, 'opaque schema representation');
Accelerator::configure(Mode::PurePhp);
nativeCheck($schema->canonical() === Json::canonicalize($schemaInput, $limits), 'schema parse canonical parity');
nativeCheck($operation->canonical() === Json::canonicalize($operationInput, $limits), 'operation parse canonical parity');
nativeCheck(naatre_native_validate_json($schemaInput, ...$limits->arguments()), 'native structural validation');
$pureInvalid = nativeError(static fn () => Json::decode('{'));
nativeCheck(nativeExtensionError(static fn () => naatre_native_validate_json('{', ...$limits->arguments())) === $pureInvalid, 'validation error parity');
nativeCheck(strlen($plan->key()) === 64 && $plan->schema() === $schema->canonical(), 'immutable operation plan');
nativeCheck(
    $plan->key() === hash('sha256', "naatre:document:c14n-1\n" . $schema->canonical() . "\n" . $operation->canonical()),
    'compiled plan key parity',
);
nativeCheck(
    nativeExtensionError(static fn () => naatre_native_compile($schema, $operation, 128, 4, 1024, 1024)) === 'CLIENT_JSON_NODE_LIMIT_EXCEEDED',
    'compiled plan obeys aggregate node limit',
);
nativeCheck(
    nativeExtensionError(static fn () => naatre_native_compile($schema, $operation, 128, 100, strlen($schema->canonical()) + strlen($operation->canonical()), 1024)) === 'CLIENT_RESPONSE_TOO_LARGE',
    'compiled plan accounts for key separator before allocation',
);

for ($request = 0; $request < 128; ++$request) {
    try {
        naatre_native_parse('schema', '{', ...$limits->arguments());
    } catch (Throwable) {
    }
    $isolated = naatre_native_parse('schema', '{"revision":"tenant-' . $request . '","types":[]}', ...$limits->arguments());
    if ($request > 0) {
        nativeCheck(!str_contains($isolated->canonical(), 'tenant-' . ($request - 1) . '"'), 'persistent request isolation ' . $request);
    }
    unset($isolated);
}
gc_collect_cycles();

fwrite(STDOUT, json_encode(['profile' => 'sdk.php.native-1', 'status' => 'native-passed', 'metadata' => $metadata], JSON_THROW_ON_ERROR) . "\n");
