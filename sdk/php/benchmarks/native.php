<?php

declare(strict_types=1);

use Naatre\Sdk\Native\Accelerator;
use Naatre\Sdk\Native\Limits;
use Naatre\Sdk\Native\Mode;
use Naatre\Sdk\Wire\Json;

require dirname(__DIR__) . '/tests/bootstrap.php';

$options = getopt('', ['samples:', 'warmup:', 'output:']);
$samples = max(30, (int) ($options['samples'] ?? 30));
$warmup = max(1, (int) ($options['warmup'] ?? 5));
$output = $options['output'] ?? null;
$fixture = (string) file_get_contents(dirname(__DIR__, 3) . '/conformance/v1/canonical.json');
$workloads = [
    'small' => '{"b":2,"a":1}',
    'medium' => $fixture,
    'large' => '[' . implode(',', array_fill(0, 32, $fixture)) . ']',
];
$limits = new Limits(maximumDepth: 128, maximumNodes: 2_000_000, maximumBytes: 64 * 1024 * 1024, maximumOutputBytes: 64 * 1024 * 1024);
$results = [];

/** @param callable(): string|bool $operation @return array<string, mixed> */
function benchmarkCase(string $mode, string $primitive, string $workload, int $inputBytes, callable $operation, int $samples, int $warmup): array
{
    for ($index = 0; $index < $warmup; ++$index) {
        $operation();
    }
    gc_collect_cycles();
    $memoryBefore = memory_get_usage(true);
    $raw = [];
    for ($index = 0; $index < $samples; ++$index) {
        $before = hrtime(true);
        $result = $operation();
        $elapsed = hrtime(true) - $before;
        $encoded = is_bool($result) ? ($result ? 'true' : 'false') : $result;
        $raw[] = [
            'elapsedNanoseconds' => $elapsed,
            'outputBytes' => strlen($encoded),
            'digest' => hash('sha256', $encoded),
        ];
    }
    return [
        'mode' => $mode,
        'primitive' => $primitive,
        'workload' => $workload,
        'inputBytes' => $inputBytes,
        'samples' => $raw,
        'memoryBeforeBytes' => $memoryBefore,
        'memoryAfterBytes' => memory_get_usage(true),
        'peakMemoryBytes' => memory_get_peak_usage(true),
    ];
}

foreach ([Mode::PurePhp, Mode::Native] as $mode) {
    Accelerator::configure($mode);
    foreach ($workloads as $size => $input) {
        $schemaInput = '{"schema":' . $input . '}';
        $operationInput = '{"operation":' . $input . '}';
        if ($mode === Mode::Native) {
            $schema = naatre_native_parse('schema', $schemaInput, ...$limits->arguments());
            $operation = naatre_native_parse('operation', $operationInput, ...$limits->arguments());
            $compile = static fn (): string => naatre_native_compile($schema, $operation, ...$limits->arguments())->key();
        } else {
            $schemaCanonical = Json::canonicalize($schemaInput, $limits);
            $operationCanonical = Json::canonicalize($operationInput, $limits);
            $compile = static fn (): string => hash('sha256', "naatre:document:c14n-1\n{$schemaCanonical}\n{$operationCanonical}");
        }

        $cases = [
            'parse-schema' => $mode === Mode::Native
                ? static fn (): string => naatre_native_parse('schema', $schemaInput, ...$limits->arguments())->canonical()
                : static fn (): string => Json::encode(Json::decode($schemaInput, limits: $limits), $limits),
            'validate-structure' => $mode === Mode::Native
                ? static fn (): bool => naatre_native_validate_json($input, ...$limits->arguments())
                : static function () use ($input, $limits): bool {
                    Json::decode($input, limits: $limits);
                    return true;
                },
            'canonicalize' => static fn (): string => Json::canonicalize($input, $limits),
            'semantic-hash' => static fn (): string => Json::semanticHash('document', $input, $limits),
            'compile-plan' => $compile,
            'repeated-execution-preparation' => $compile,
            'sdk-round-trip' => static fn (): string => Json::encode(Json::decode($input, limits: $limits), $limits),
            'persistent-worker-preparation' => $mode === Mode::Native
                ? static function () use ($schemaInput, $operationInput, $limits): string {
                    $schema = naatre_native_parse('schema', $schemaInput, ...$limits->arguments());
                    $operation = naatre_native_parse('operation', $operationInput, ...$limits->arguments());
                    return naatre_native_compile($schema, $operation, ...$limits->arguments())->key();
                }
                : static function () use ($schemaInput, $operationInput, $limits): string {
                    $schema = Json::canonicalize($schemaInput, $limits);
                    $operation = Json::canonicalize($operationInput, $limits);
                    return hash('sha256', "naatre:document:c14n-1\n{$schema}\n{$operation}");
                },
            'php-native-boundary' => static fn (): string => Json::encode('boundary', $limits),
        ];
        foreach ($cases as $primitive => $operation) {
            $results[] = benchmarkCase($mode->value, $primitive, $size, strlen($input), $operation, $samples, $warmup);
        }
    }
}

$report = [
    'format' => 'naatre.php-native-benchmark-1',
    'phpVersion' => PHP_VERSION,
    'phpVersionId' => PHP_VERSION_ID,
    'os' => PHP_OS_FAMILY,
    'architecture' => php_uname('m'),
    'zts' => PHP_ZTS,
    'debug' => PHP_DEBUG,
    'opcacheEnabled' => (bool) ini_get('opcache.enable_cli'),
    'jit' => (string) ini_get('opcache.jit'),
    'warmup' => $warmup,
    'sampleCount' => $samples,
    'allocationEvidence' => [
        'phpMemoryCounters' => ['memoryBeforeBytes', 'memoryAfterBytes', 'peakMemoryBytes'],
        'nativeAllocationCount' => 'requires-platform-profiler',
    ],
    'extension' => naatre_native_info(),
    'results' => $results,
];
$json = json_encode($report, JSON_THROW_ON_ERROR | JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES) . "\n";
if (is_string($output)) {
    file_put_contents($output, $json);
} else {
    fwrite(STDOUT, $json);
}
