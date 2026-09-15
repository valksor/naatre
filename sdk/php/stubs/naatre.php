<?php

declare(strict_types=1);

namespace Naatre\Native {
    final class Representation
    {
        private function __construct() {}
        public function kind(): string {}
        public function canonical(): string {}
        public function nodeCount(): int {}
    }

    final class Plan
    {
        private function __construct() {}
        public function key(): string {}
        public function schema(): string {}
        public function operation(): string {}
    }
}

namespace {
    /** @return array<string, mixed> */
    function naatre_native_info(): array {}
    function naatre_native_decode_json(string $input, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): mixed {}
    function naatre_native_encode_json(mixed $value, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): string {}
    function naatre_native_canonicalize_json(string $input, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): string {}
    function naatre_native_validate_json(string $input, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): bool {}
    /** @param list<string> $inputs @return list<string> */
    function naatre_native_batch_canonicalize(array $inputs, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): array {}
    function naatre_native_semantic_hash(string $purpose, string $canonical, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): string {}
    function naatre_native_parse(string $kind, string $input, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): \Naatre\Native\Representation {}
    function naatre_native_compile(\Naatre\Native\Representation $schema, \Naatre\Native\Representation $operation, int $maximumDepth, int $maximumNodes, int $maximumBytes, int $maximumOutputBytes): \Naatre\Native\Plan {}
}
