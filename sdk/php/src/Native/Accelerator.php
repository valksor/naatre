<?php

declare(strict_types=1);

namespace Naatre\Sdk\Native;

use Naatre\Sdk\Exception\ClientException;

final class Accelerator
{
    private static Mode $mode = Mode::Auto;

    private function __construct()
    {
    }

    public static function configure(Mode|string $mode): void
    {
        self::$mode = is_string($mode) ? Mode::from($mode) : $mode;
        if (self::$mode === Mode::Native) {
            self::requireCapability('json-codec');
        }
    }

    public static function mode(): Mode
    {
        return self::$mode;
    }

    public static function reset(): void
    {
        self::$mode = Mode::Auto;
    }

    /** @return array<string, mixed> */
    public static function diagnostics(): array
    {
        $metadata = self::metadata();
        return [
            'configuredMode' => self::$mode->value,
            'selectedMode' => self::usesNative('json-codec') ? Mode::Native->value : Mode::PurePhp->value,
            'compatible' => self::compatible($metadata),
            'extension' => $metadata,
        ];
    }

    public static function usesNative(string $capability): bool
    {
        if (self::$mode === Mode::PurePhp) {
            return false;
        }
        $metadata = self::metadata();
        if (!self::compatible($metadata) || !in_array($capability, self::stringList($metadata, 'capabilities'), true)) {
            if (self::$mode === Mode::Native) {
                self::unavailable($metadata === null ? 'CLIENT_NATIVE_UNAVAILABLE' : 'CLIENT_NATIVE_INCOMPATIBLE');
            }
            return false;
        }
        if (self::$mode === Mode::Native) {
            return true;
        }
        return in_array($capability, self::stringList($metadata, 'defaultEnabled'), true);
    }

    /** @return array<string, mixed>|null */
    private static function metadata(): ?array
    {
        if (!extension_loaded('naatre') || !function_exists('naatre_native_info')) {
            return null;
        }
        return naatre_native_info();
    }

    /** @param array<string, mixed>|null $metadata */
    private static function compatible(?array $metadata): bool
    {
        return $metadata !== null
            && ($metadata['abiVersion'] ?? null) === 1
            && ($metadata['phpVersionId'] ?? null) === PHP_VERSION_ID
            && ($metadata['pointerBits'] ?? null) === PHP_INT_SIZE * 8;
    }

    private static function requireCapability(string $capability): void
    {
        self::usesNative($capability);
    }

    /**
     * @param array<string, mixed>|null $metadata
     * @return list<string>
     */
    private static function stringList(?array $metadata, string $key): array
    {
        $value = $metadata[$key] ?? null;
        if (!is_array($value)) {
            return [];
        }
        $result = [];
        foreach (array_keys($value) as $index) {
            if (is_string($value[$index])) {
                $result[] = $value[$index];
            }
        }
        return $result;
    }

    private static function unavailable(string $code): never
    {
        throw new ClientException($code);
    }
}
