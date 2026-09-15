<?php

declare(strict_types=1);

namespace Naatre\Sdk\Integration\Symfony;

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Integration\RequestScopedClientFactory;
use Naatre\Sdk\Integration\RequestScopedClientProvider;
use Naatre\Sdk\Transport\AuthContext;
use Naatre\Sdk\Transport\Psr18Client;
use Psr\Http\Client\ClientInterface;
use Psr\Http\Message\RequestFactoryInterface;
use Psr\Http\Message\StreamFactoryInterface;

final readonly class NaatreClientFactory implements RequestScopedClientProvider
{
    public const PROFILE = 'sdk.php.symfony-psr18-1';

    private RequestScopedClientFactory $clients;

    public function __construct(
        string $endpoint,
        ClientInterface $http,
        RequestFactoryInterface $requests,
        StreamFactoryInterface $streams,
        int $maximumResponseBytes = 16_777_216,
    ) {
        $this->clients = new RequestScopedClientFactory(
            $endpoint,
            $http,
            $requests,
            $streams,
            $maximumResponseBytes,
        );
    }

    public static function fromPsr18Bridge(
        string $endpoint,
        ClientInterface $bridge,
        int $maximumResponseBytes = 16_777_216,
    ): self {
        if (!$bridge instanceof RequestFactoryInterface || !$bridge instanceof StreamFactoryInterface) {
            throw new ClientException('CLIENT_CONFIGURATION_INVALID');
        }
        return new self($endpoint, $bridge, $bridge, $bridge, $maximumResponseBytes);
    }

    /** @param (callable(AuthContext): array<string, string>)|null $authenticate */
    public function forRequest(mixed $authenticate = null): Psr18Client
    {
        return $this->clients->forRequest($authenticate);
    }
}
