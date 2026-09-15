<?php

declare(strict_types=1);

namespace Naatre\Sdk\Native;

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Scalar\Bytes;
use Naatre\Sdk\Scalar\Decimal;
use Naatre\Sdk\Scalar\Int64;
use Naatre\Sdk\Scalar\Timestamp;
use Naatre\Sdk\Scalar\UInt64;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Wire\PureJson;
use stdClass;
use Throwable;

final class NativeAdapter
{
    private function __construct()
    {
    }

    public static function decode(string $input, Limits $limits): mixed
    {
        /** @var mixed $decoded */
        $decoded = self::invoke(static fn (): mixed => naatre_native_decode_json($input, ...$limits->arguments()));
        return PureJson::fromDecoded($decoded);
    }

    public static function encode(mixed $value, Limits $limits): string
    {
        $encoded = self::invoke(static fn (): string => naatre_native_encode_json(self::toNative($value), ...$limits->arguments()));
        return is_string($encoded) ? $encoded : throw new ClientException('CLIENT_NATIVE_FAILURE');
    }

    public static function canonicalize(string $input, Limits $limits): string
    {
        $canonical = self::invoke(static fn (): string => naatre_native_canonicalize_json($input, ...$limits->arguments()));
        return is_string($canonical) ? $canonical : throw new ClientException('CLIENT_NATIVE_FAILURE');
    }

    public static function semanticHash(string $purpose, string $canonical, Limits $limits): string
    {
        $digest = self::invoke(static fn (): string => naatre_native_semantic_hash($purpose, $canonical, ...$limits->arguments()));
        return is_string($digest) ? $digest : throw new ClientException('CLIENT_NATIVE_FAILURE');
    }

    /** @param callable(): mixed $operation */
    private static function invoke(callable $operation): mixed
    {
        try {
            return $operation();
        } catch (Throwable $error) {
            $code = $error->getMessage();
            if (!preg_match('/^CLIENT_[A-Z0-9_]+$/', $code)) {
                $code = 'CLIENT_NATIVE_FAILURE';
            }
            throw new ClientException($code);
        }
    }

    private static function toNative(mixed $value): mixed
    {
        if ($value instanceof ObjectValue) {
            $object = new stdClass();
            foreach ($value->entries() as $entry) {
                $object->{$entry->key} = self::toNative($entry->value);
            }
            return $object;
        }
        if ($value instanceof ListValue) {
            return array_map(self::toNative(...), $value->values());
        }
        if ($value instanceof Int64 || $value instanceof UInt64 || $value instanceof Decimal || $value instanceof Timestamp) {
            return (string) $value;
        }
        if ($value instanceof Bytes) {
            return $value->wire();
        }
        if ($value === null || is_bool($value) || is_int($value) || is_float($value) || is_string($value)) {
            return $value;
        }
        throw new ClientException('CLIENT_JSON_INVALID');
    }
}
