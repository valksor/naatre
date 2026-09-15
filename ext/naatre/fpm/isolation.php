<?php

declare(strict_types=1);

$tenant = $_GET['tenant'] ?? '';
$previous = $_GET['previous'] ?? '';
$exerciseFailure = ($_GET['fail'] ?? '') === '1';

if (!is_string($tenant) || $tenant === '') {
    http_response_code(400);
    exit;
}

$failureCode = null;
if ($exerciseFailure) {
    try {
        naatre_native_parse('schema', '{', 16, 100, 1024, 1024);
    } catch (Throwable $error) {
        $failureCode = $error->getMessage();
    }
}

$representation = naatre_native_parse(
    'schema',
    json_encode(['revision' => $tenant, 'types' => []], JSON_THROW_ON_ERROR),
    16,
    100,
    1024,
    1024,
);
$canonical = $representation->canonical();

header('Content-Type: application/json');
echo json_encode([
    'pid' => getmypid(),
    'tenant' => $tenant,
    'containsCurrent' => str_contains($canonical, '"' . $tenant . '"'),
    'containsPrevious' => $previous !== '' && str_contains($canonical, '"' . $previous . '"'),
    'failureCode' => $failureCode,
], JSON_THROW_ON_ERROR);
