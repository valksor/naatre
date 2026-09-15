<?php

declare(strict_types=1);

namespace Naatre\Sdk\Integration;

use Naatre\Sdk\Transport\AuthContext;
use Naatre\Sdk\Transport\Psr18Client;
use Psr\Http\Client\ClientInterface;
use Psr\Http\Message\RequestFactoryInterface;
use Psr\Http\Message\StreamFactoryInterface;

final readonly class RequestScopedClientFactory implements RequestScopedClientProvider
{
    public function __construct(
        private string $endpoint,
        private ClientInterface $http,
        private RequestFactoryInterface $requests,
        private StreamFactoryInterface $streams,
        private int $maximumResponseBytes = 16_777_216,
    ) {
    }

    /** @param (callable(AuthContext): array<string, string>)|null $authenticate */
    public function forRequest(mixed $authenticate = null): Psr18Client
    {
        return new Psr18Client(
            $this->endpoint,
            $this->http,
            $this->requests,
            $this->streams,
            $authenticate,
            $this->maximumResponseBytes,
        );
    }
}
