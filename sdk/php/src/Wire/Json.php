<?php

declare(strict_types=1);

namespace Naatre\Sdk\Wire;

use JsonException;
use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Scalar\Bytes;
use Naatre\Sdk\Scalar\Decimal;
use Naatre\Sdk\Scalar\Int64;
use Naatre\Sdk\Scalar\Timestamp;
use Naatre\Sdk\Scalar\UInt64;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use stdClass;

final class Json
{
    private function __construct()
    {
    }

    public static function decode(string $input, int $maximumBytes = 16_777_216): mixed
    {
        if (strlen($input) > $maximumBytes) {
            throw new ClientException('CLIENT_RESPONSE_TOO_LARGE');
        }
        DuplicateKeyGuard::check($input);
        try {
            return self::fromDecoded(json_decode($input, false, 512, JSON_THROW_ON_ERROR | JSON_BIGINT_AS_STRING));
        } catch (JsonException $error) {
            throw new ClientException('CLIENT_JSON_INVALID', $error);
        }
    }

    public static function encode(mixed $value): string
    {
        if ($value instanceof ObjectValue) {
            $entries = $value->entries();
            usort($entries, static fn (MapEntry $left, MapEntry $right): int => self::compareKeys($left->key, $right->key));
            $parts = [];
            foreach ($entries as $entry) {
                $parts[] = self::string($entry->key) . ':' . self::encode($entry->value);
            }
            return '{' . implode(',', $parts) . '}';
        }
        if ($value instanceof ListValue) {
            return '[' . implode(',', array_map(self::encode(...), $value->values())) . ']';
        }
        if ($value instanceof Int64 || $value instanceof UInt64 || $value instanceof Decimal || $value instanceof Timestamp) {
            return self::string((string) $value);
        }
        if ($value instanceof Bytes) {
            return self::string($value->wire());
        }
        if ($value === null) {
            return 'null';
        }
        if (is_bool($value)) {
            return $value ? 'true' : 'false';
        }
        if (is_int($value)) {
            $decimal = (string) $value;
            if (self::compareUnsigned(ltrim($decimal, '-'), '9007199254740991') > 0) {
                throw new ClientException('CLIENT_JSON_UNSAFE_INTEGER');
            }
            return $decimal;
        }
        if (is_float($value)) {
            if (!is_finite($value)) {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
            try {
                return json_encode($value, JSON_THROW_ON_ERROR | JSON_PRESERVE_ZERO_FRACTION);
            } catch (JsonException $error) {
                throw new ClientException('CLIENT_JSON_INVALID', $error);
            }
        }
        if (is_string($value)) {
            return self::string($value);
        }
        throw new ClientException('CLIENT_JSON_INVALID');
    }

    private static function fromDecoded(mixed $value): mixed
    {
        if ($value instanceof stdClass) {
            $entries = [];
            foreach (array_keys(get_object_vars($value)) as $key) {
                $name = self::objectKey($key);
                $entries[] = new MapEntry($name, self::fromDecoded($value->{$name}));
            }
            return new ObjectValue($entries);
        }
        if (is_array($value)) {
            return new ListValue(array_map(self::fromDecoded(...), array_values($value)));
        }
        return $value;
    }

    private static function string(string $value): string
    {
        try {
            return json_encode($value, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
        } catch (JsonException $error) {
            throw new ClientException('CLIENT_JSON_INVALID', $error);
        }
    }

    private static function compareKeys(string $left, string $right): int
    {
        $leftUTF16 = iconv('UTF-8', 'UTF-16BE', $left);
        $rightUTF16 = iconv('UTF-8', 'UTF-16BE', $right);
        if ($leftUTF16 === false || $rightUTF16 === false) {
            throw new ClientException('CLIENT_JSON_INVALID');
        }
        return strcmp($leftUTF16, $rightUTF16);
    }

    private static function compareUnsigned(string $left, string $right): int
    {
        return strlen($left) === strlen($right) ? strcmp($left, $right) <=> 0 : strlen($left) <=> strlen($right);
    }

    private static function objectKey(string|int $key): string
    {
        return is_int($key) ? (string) $key : $key;
    }
}
