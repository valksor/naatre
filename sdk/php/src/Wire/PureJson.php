<?php

declare(strict_types=1);

namespace Naatre\Sdk\Wire;

use JsonException;
use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Native\Limits;
use Naatre\Sdk\Scalar\Bytes;
use Naatre\Sdk\Scalar\Decimal;
use Naatre\Sdk\Scalar\Int64;
use Naatre\Sdk\Scalar\Timestamp;
use Naatre\Sdk\Scalar\UInt64;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use stdClass;

/** @internal */
final class PureJson
{
    private function __construct()
    {
    }

    public static function decodeChecked(string $input, Limits $limits): mixed
    {
        /** @var mixed $decoded */
        $decoded = self::decode($input, $limits);
        $nodes = 0;
        self::checkValueLimits($decoded, $limits, 1, $nodes);
        if (strlen(self::encode($decoded)) > $limits->maximumOutputBytes) {
            throw new ClientException('CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED');
        }
        return $decoded;
    }

    public static function decode(string $input, Limits $limits): mixed
    {
        if (strlen($input) > $limits->maximumBytes) {
            throw new ClientException('CLIENT_RESPONSE_TOO_LARGE');
        }
        DuplicateKeyGuard::check($input, $limits->maximumDepth, $limits->maximumNodes);
        try {
            return self::fromDecoded(json_decode($input, false, 512, JSON_THROW_ON_ERROR | JSON_BIGINT_AS_STRING));
        } catch (JsonException $error) {
            throw new ClientException('CLIENT_JSON_INVALID', $error);
        }
    }

    public static function encodeChecked(mixed $value, Limits $limits): string
    {
        $nodes = 0;
        self::checkValueLimits($value, $limits, 1, $nodes);
        $encoded = self::encode($value);
        if (strlen($encoded) > $limits->maximumOutputBytes) {
            throw new ClientException('CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED');
        }
        return $encoded;
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
            return self::number($value);
        }
        if (is_string($value)) {
            return self::string($value);
        }
        throw new ClientException('CLIENT_JSON_INVALID');
    }

    public static function semanticHash(string $purpose, string $canonical): string
    {
        if (!in_array($purpose, ['document', 'schema', 'approval', 'result-cache', 'idempotency', 'signed-message', 'federation'], true)) {
            throw new ClientException('CLIENT_NATIVE_PURPOSE_INVALID');
        }
        return hash('sha256', "naatre:{$purpose}:c14n-1\n{$canonical}");
    }

    public static function fromDecoded(mixed $value): mixed
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

    private static function number(float $value): string
    {
        if ($value == 0.0) {
            return '0';
        }
        try {
            $encoded = strtolower(json_encode($value, JSON_THROW_ON_ERROR));
        } catch (JsonException $error) {
            throw new ClientException('CLIENT_JSON_INVALID', $error);
        }
        return preg_replace('/\.0(?=e)/', '', $encoded) ?? throw new ClientException('CLIENT_JSON_INVALID');
    }

    private static function checkValueLimits(mixed $value, Limits $limits, int $depth, int &$nodes): void
    {
        if ($depth > $limits->maximumDepth) {
            throw new ClientException('CLIENT_JSON_DEPTH_EXCEEDED');
        }
        if (++$nodes > $limits->maximumNodes) {
            throw new ClientException('CLIENT_JSON_NODE_LIMIT_EXCEEDED');
        }
        if ($value instanceof ObjectValue) {
            foreach ($value->entries() as $entry) {
                self::checkValueLimits($entry->value, $limits, $depth + 1, $nodes);
            }
        } elseif ($value instanceof ListValue) {
            $values = $value->values();
            foreach (array_keys($values) as $index) {
                self::checkValueLimits($values[$index], $limits, $depth + 1, $nodes);
            }
        }
    }

    private static function objectKey(string|int $key): string
    {
        return is_int($key) ? (string) $key : $key;
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
}
