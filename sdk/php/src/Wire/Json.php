<?php

declare(strict_types=1);

namespace Naatre\Sdk\Wire;

use Naatre\Sdk\Native\Accelerator;
use Naatre\Sdk\Native\Limits;
use Naatre\Sdk\Native\NativeAdapter;

final class Json
{
    private function __construct()
    {
    }

    public static function decode(string $input, int $maximumBytes = 16_777_216, ?Limits $limits = null): mixed
    {
        $limits ??= new Limits(maximumBytes: $maximumBytes);
        if (Accelerator::usesNative('json-codec')) {
            return NativeAdapter::decode($input, $limits);
        }
        return PureJson::decodeChecked($input, $limits);
    }

    public static function decodePure(string $input, int $maximumBytes = 16_777_216, ?Limits $limits = null): mixed
    {
        $limits ??= new Limits(maximumBytes: $maximumBytes);
        return PureJson::decode($input, $limits);
    }

    public static function encode(mixed $value, ?Limits $limits = null): string
    {
        $limits ??= new Limits();
        if (Accelerator::usesNative('json-codec')) {
            return NativeAdapter::encode($value, $limits);
        }
        return PureJson::encodeChecked($value, $limits);
    }

    public static function encodePure(mixed $value): string
    {
        return PureJson::encode($value);
    }

    public static function canonicalize(string $input, ?Limits $limits = null): string
    {
        $limits ??= new Limits();
        if (Accelerator::usesNative('canonical-json')) {
            return NativeAdapter::canonicalize($input, $limits);
        }
        return self::encode(self::decodePure($input, $limits->maximumBytes, $limits), $limits);
    }

    public static function semanticHash(string $purpose, string $input, ?Limits $limits = null): string
    {
        $limits ??= new Limits();
        $canonical = self::canonicalize($input, $limits);
        if (Accelerator::usesNative('semantic-hash')) {
            return NativeAdapter::semanticHash($purpose, $canonical, $limits);
        }
        return PureJson::semanticHash($purpose, $canonical);
    }
}
