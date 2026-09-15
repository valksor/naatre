<?php

declare(strict_types=1);

const PROTOCOL = 'naatre.remote-worker.v1';

$fixture = json_decode(file_get_contents(__DIR__ . '/../../../conformance/v1/remote-workers.json'), true, flags: JSON_THROW_ON_ERROR);

function handle(array $envelope): array
{
    if (($envelope['protocol'] ?? null) !== PROTOCOL) {
        throw new RuntimeException('invalid protocol');
    }
    $payload = $envelope['payload'];
    if ($envelope['kind'] === 'register') {
        return ['protocol' => PROTOCOL, 'kind' => 'registered', 'payload' => ['protocol' => PROTOCOL, 'workerId' => $payload['workerId'], 'sessionId' => 'example-session', 'schemaRevision' => $payload['schemaRevision'], 'acceptedCapabilities' => $payload['capabilities']]];
    }
    if ($envelope['kind'] === 'cancel') {
        return ['protocol' => PROTOCOL, 'kind' => 'cancelled', 'payload' => ['protocol' => PROTOCOL, 'invocationId' => $payload['invocationId'], 'disposition' => 'acknowledged']];
    }
    if ($envelope['kind'] !== 'invoke') {
        throw new RuntimeException('unsupported envelope');
    }
    $rejected = $payload['input']['name'] === 'reject';
    return ['protocol' => PROTOCOL, 'kind' => 'result', 'payload' => ['protocol' => PROTOCOL, 'invocationId' => $payload['invocationId'], 'attemptId' => $payload['attemptId'], 'schemaRevision' => $payload['schemaRevision'], 'data' => $rejected ? null : ['greeting' => 'Hello, ' . $payload['input']['name']], 'errors' => $rejected ? [['code' => 'NAME_REJECTED', 'message' => 'name was rejected', 'retryable' => false]] : []]];
}

function readExact($stream, int $length): string
{
    $result = '';
    while (strlen($result) < $length && !feof($stream)) {
        $chunk = fread($stream, $length - strlen($result));
        if ($chunk === false) {
            throw new RuntimeException('frame read failed');
        }
        $result .= $chunk;
    }
    return $result;
}

function serve(array $fixture): void
{
    while (($header = readExact(STDIN, 5)) !== '') {
        if (strlen($header) !== 5) {
            throw new RuntimeException('truncated frame header');
        }
        $decoded = unpack('Cflags/Nsize', $header);
        if ($decoded['flags'] !== 0 || $decoded['size'] < 1 || $decoded['size'] > $fixture['transport']['frame']['maximumBytes']) {
            throw new RuntimeException('invalid frame');
        }
        $payload = readExact(STDIN, $decoded['size']);
        if (strlen($payload) !== $decoded['size']) {
            throw new RuntimeException('truncated frame');
        }
        $encoded = json_encode(handle(json_decode($payload, true, flags: JSON_THROW_ON_ERROR)), JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES);
        fwrite(STDOUT, pack('CN', 0, strlen($encoded)) . $encoded);
        fflush(STDOUT);
    }
}

function selfTest(array $fixture): void
{
    if ($fixture['schema']['sharedSchemaProfile'] !== 'core.schema-1') {
        throw new RuntimeException('wrong shared schema');
    }
    foreach ($fixture['operations'] as $operation) {
        $response = handle(['protocol' => PROTOCOL, 'kind' => 'invoke', 'payload' => ['invocationId' => $operation['name'], 'attemptId' => 'attempt-1', 'schemaRevision' => $fixture['schema']['revision'], 'handlerId' => $operation['handlerId'], 'input' => $operation['input']]]);
        if (['data' => $response['payload']['data'], 'errors' => $response['payload']['errors']] !== $operation['expected']) {
            throw new RuntimeException('fixture mismatch');
        }
    }
}

in_array('--serve', $argv, true) ? serve($fixture) : selfTest($fixture);
