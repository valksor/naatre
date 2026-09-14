<?php

declare(strict_types=1);

namespace Naatre\Sdk\Wire;

use JsonException;
use Naatre\Sdk\Exception\ClientException;

final class DuplicateKeyGuard
{
    private function __construct()
    {
    }

    public static function check(string $json): void
    {
        $offset = 0;
        self::space($json, $offset);
        self::value($json, $offset);
        self::space($json, $offset);
        if ($offset !== strlen($json)) {
            throw new ClientException('CLIENT_JSON_INVALID');
        }
    }

    private static function value(string $json, int &$offset): void
    {
        self::space($json, $offset);
        $character = $json[$offset] ?? '';
        if ($character === '{') {
            self::object($json, $offset);
            return;
        }
        if ($character === '[') {
            self::list($json, $offset);
            return;
        }
        if ($character === '"') {
            self::string($json, $offset);
            return;
        }
        while (isset($json[$offset]) && !in_array($json[$offset], [',', ']', '}', ' ', "\t", "\r", "\n"], true)) {
            ++$offset;
        }
    }

    private static function object(string $json, int &$offset): void
    {
        ++$offset;
        self::space($json, $offset);
        $seen = [];
        if (($json[$offset] ?? '') === '}') {
            ++$offset;
            return;
        }
        while (true) {
            $key = self::string($json, $offset);
            $identity = "\0" . $key;
            if (isset($seen[$identity])) {
                throw new ClientException('CLIENT_JSON_DUPLICATE_KEY');
            }
            $seen[$identity] = true;
            self::space($json, $offset);
            if (($json[$offset] ?? '') !== ':') {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
            ++$offset;
            self::value($json, $offset);
            self::space($json, $offset);
            $separator = $json[$offset] ?? '';
            ++$offset;
            if ($separator === '}') {
                return;
            }
            if ($separator !== ',') {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
            self::space($json, $offset);
        }
    }

    private static function list(string $json, int &$offset): void
    {
        ++$offset;
        self::space($json, $offset);
        if (($json[$offset] ?? '') === ']') {
            ++$offset;
            return;
        }
        while (true) {
            self::value($json, $offset);
            self::space($json, $offset);
            $separator = $json[$offset] ?? '';
            ++$offset;
            if ($separator === ']') {
                return;
            }
            if ($separator !== ',') {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
        }
    }

    private static function string(string $json, int &$offset): string
    {
        if (($json[$offset] ?? '') !== '"') {
            throw new ClientException('CLIENT_JSON_INVALID');
        }
        $start = $offset++;
        while (isset($json[$offset])) {
            if ($json[$offset] === '\\') {
                $offset += 2;
                continue;
            }
            if ($json[$offset++] === '"') {
                try {
                    $decoded = json_decode(substr($json, $start, $offset - $start), true, 2, JSON_THROW_ON_ERROR);
                } catch (JsonException $error) {
                    throw new ClientException('CLIENT_JSON_INVALID', $error);
                }
                if (!is_string($decoded)) {
                    throw new ClientException('CLIENT_JSON_INVALID');
                }
                return $decoded;
            }
        }
        throw new ClientException('CLIENT_JSON_INVALID');
    }

    private static function space(string $json, int &$offset): void
    {
        while (isset($json[$offset]) && in_array($json[$offset], [' ', "\t", "\r", "\n"], true)) {
            ++$offset;
        }
    }
}
