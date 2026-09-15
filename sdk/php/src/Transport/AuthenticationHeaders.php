<?php

declare(strict_types=1);

namespace Naatre\Sdk\Transport;

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Protocol\Operation;
use Throwable;

/** @internal */
final class AuthenticationHeaders
{
    /** @var list<string> */
    private const PROTECTED = [
        'accept',
        'content-length',
        'content-type',
        'host',
        'naatre-timeout-ms',
        'transfer-encoding',
    ];

    /**
     * @param (callable(AuthContext): array<string, string>)|null $authenticate
     * @return array<string, string>
     */
    public static function resolve(mixed $authenticate, string $endpoint, Operation $operation): array
    {
        if ($authenticate === null) {
            return [];
        }
        try {
            $headers = $authenticate(new AuthContext($endpoint, $operation));
            if (!is_array($headers)) {
                throw new ClientException('CLIENT_AUTHENTICATION_ERROR');
            }
            foreach ($headers as $name => $value) {
                if (!self::valid($name, $value)) {
                    throw new ClientException('CLIENT_AUTH_HEADER_INVALID');
                }
            }
            return $headers;
        } catch (Throwable $error) {
            if ($error instanceof ClientException) {
                throw $error;
            }
            throw new ClientException('CLIENT_AUTHENTICATION_ERROR');
        }
    }

    private static function valid(mixed $name, mixed $value): bool
    {
        return is_string($name)
            && is_string($value)
            && preg_match("/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/D", $name) === 1
            && !str_contains($value, "\r")
            && !str_contains($value, "\n")
            && !in_array(strtolower($name), self::PROTECTED, true);
    }
}
