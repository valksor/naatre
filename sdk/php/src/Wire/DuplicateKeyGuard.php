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

    public static function check(string $json, int $maximumDepth = 512, int $maximumNodes = 1_000_000): void
    {
        $state = new JsonScanState($json, $maximumDepth, $maximumNodes);
        self::space($state);
        self::value($state, 1);
        self::space($state);
        if ($state->offset !== strlen($json)) {
            throw new ClientException('CLIENT_JSON_INVALID');
        }
    }

    private static function value(JsonScanState $state, int $depth): void
    {
        if ($depth > $state->maximumDepth) {
            throw new ClientException('CLIENT_JSON_DEPTH_EXCEEDED');
        }
        if (++$state->nodes > $state->maximumNodes) {
            throw new ClientException('CLIENT_JSON_NODE_LIMIT_EXCEEDED');
        }
        self::space($state);
        $character = $state->json[$state->offset] ?? '';
        if ($character === '{') {
            self::object($state, $depth);
            return;
        }
        if ($character === '[') {
            self::list($state, $depth);
            return;
        }
        if ($character === '"') {
            self::string($state);
            return;
        }
        while (isset($state->json[$state->offset]) && !in_array($state->json[$state->offset], [',', ']', '}', ' ', "\t", "\r", "\n"], true)) {
            ++$state->offset;
        }
    }

    private static function object(JsonScanState $state, int $depth): void
    {
        ++$state->offset;
        self::space($state);
        $seen = [];
        if (($state->json[$state->offset] ?? '') === '}') {
            ++$state->offset;
            return;
        }
        while (true) {
            $key = self::string($state);
            $identity = "\0" . $key;
            if (isset($seen[$identity])) {
                throw new ClientException('CLIENT_JSON_DUPLICATE_KEY');
            }
            $seen[$identity] = true;
            self::space($state);
            if (($state->json[$state->offset] ?? '') !== ':') {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
            ++$state->offset;
            self::value($state, $depth + 1);
            self::space($state);
            $separator = $state->json[$state->offset] ?? '';
            ++$state->offset;
            if ($separator === '}') {
                return;
            }
            if ($separator !== ',') {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
            self::space($state);
        }
    }

    private static function list(JsonScanState $state, int $depth): void
    {
        ++$state->offset;
        self::space($state);
        if (($state->json[$state->offset] ?? '') === ']') {
            ++$state->offset;
            return;
        }
        while (true) {
            self::value($state, $depth + 1);
            self::space($state);
            $separator = $state->json[$state->offset] ?? '';
            ++$state->offset;
            if ($separator === ']') {
                return;
            }
            if ($separator !== ',') {
                throw new ClientException('CLIENT_JSON_INVALID');
            }
        }
    }

    private static function string(JsonScanState $state): string
    {
        if (($state->json[$state->offset] ?? '') !== '"') {
            throw new ClientException('CLIENT_JSON_INVALID');
        }
        $start = $state->offset++;
        while (isset($state->json[$state->offset])) {
            if ($state->json[$state->offset] === '\\') {
                $state->offset += 2;
                continue;
            }
            if ($state->json[$state->offset++] === '"') {
                try {
                    $decoded = json_decode(substr($state->json, $start, $state->offset - $start), true, 2, JSON_THROW_ON_ERROR);
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

    private static function space(JsonScanState $state): void
    {
        while (isset($state->json[$state->offset]) && in_array($state->json[$state->offset], [' ', "\t", "\r", "\n"], true)) {
            ++$state->offset;
        }
    }
}

/** @internal */
final class JsonScanState
{
    public int $offset = 0;
    public int $nodes = 0;

    public function __construct(
        public readonly string $json,
        public readonly int $maximumDepth,
        public readonly int $maximumNodes,
    ) {
    }
}
