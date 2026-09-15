<?php

declare(strict_types=1);

$input = stream_get_contents(STDIN);
if ($input === false) {
    exit(2);
}

$profiles = [
    [64, 100_000, 1_048_576, 1_048_576],
    [2, 2, 8, 8],
];

foreach ($profiles as $limits) {
    foreach ([
        static fn () => naatre_native_decode_json($input, ...$limits),
        static fn () => naatre_native_encode_json($input, ...$limits),
        static fn () => naatre_native_canonicalize_json($input, ...$limits),
        static fn () => naatre_native_validate_json($input, ...$limits),
        static fn () => naatre_native_batch_canonicalize([$input, $input], ...$limits),
        static fn () => naatre_native_semantic_hash('document', $input, ...$limits),
        static fn () => naatre_native_parse('schema', $input, ...$limits),
        static fn () => naatre_native_parse('operation', $input, ...$limits),
    ] as $entryPoint) {
        try {
            $entryPoint();
        } catch (Throwable) {
            // Declared input rejection is a successful fuzz iteration.
        }
    }

    try {
        $schema = naatre_native_parse('schema', $input, ...$limits);
        $operation = naatre_native_parse('operation', $input, ...$limits);
        naatre_native_compile($schema, $operation, ...$limits);
    } catch (Throwable) {
        // Invalid documents and exhausted limits are expected corpus outcomes.
    }
}
