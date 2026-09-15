<?php

declare(strict_types=1);

namespace Naatre\Sdk\Transport;

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Protocol\Operation;
use Naatre\Sdk\Protocol\OperationResult;
use Psr\Http\Client\ClientInterface;
use Psr\Http\Message\RequestFactoryInterface;
use Psr\Http\Message\StreamFactoryInterface;
use Throwable;

final readonly class Psr18Client
{
    public const REQUEST_MEDIA_TYPE = 'application/vnd.naatre.request+json;version=1';
    public const RESPONSE_MEDIA_TYPE = 'application/vnd.naatre.response+json;version=1';

    /** @var (callable(AuthContext): array<string, string>)|null */
    private mixed $authenticate;

    /** @param (callable(AuthContext): array<string, string>)|null $authenticate */
    public function __construct(
        private string $endpoint,
        private ClientInterface $http,
        private RequestFactoryInterface $requests,
        private StreamFactoryInterface $streams,
        mixed $authenticate = null,
        private int $maximumResponseBytes = 16_777_216,
    ) {
        $parts = parse_url($endpoint);
        if (
            !is_array($parts)
            || ($parts['scheme'] ?? null) !== 'https'
            || !isset($parts['host'])
            || isset($parts['user'])
            || isset($parts['pass'])
            || isset($parts['fragment'])
            || $maximumResponseBytes < 1
            || ($authenticate !== null && !is_callable($authenticate))
        ) {
            throw new ClientException('CLIENT_CONFIGURATION_INVALID');
        }
        $this->authenticate = $authenticate;
    }

    /** @return OperationResult */
    public function execute(Operation $operation, ?int $timeoutMilliseconds = null, int $maximumAttempts = 1): OperationResult
    {
        if ($timeoutMilliseconds !== null && ($timeoutMilliseconds < 1 || $timeoutMilliseconds > 86_400_000)) {
            throw new ClientException('CLIENT_DEADLINE_INVALID');
        }
        if ($maximumAttempts < 1 || $maximumAttempts > 8 || ($maximumAttempts > 1 && $operation->kind !== 'query')) {
            throw new ClientException('CLIENT_RETRY_INVALID');
        }
        $attempt = 0;
        while (true) {
            ++$attempt;
            try {
                $body = $this->send($operation, $timeoutMilliseconds);
                return OperationResult::decode($body, $operation);
            } catch (ClientException $error) {
                if ($error->errorCode !== 'CLIENT_TRANSPORT_ERROR' || $attempt >= $maximumAttempts) {
                    throw $error;
                }
            }
        }
    }

    private function send(Operation $operation, ?int $timeoutMilliseconds): string
    {
        try {
            $request = $this->requests->createRequest('POST', $this->endpoint)
                ->withHeader('Content-Type', self::REQUEST_MEDIA_TYPE)
                ->withHeader('Accept', self::RESPONSE_MEDIA_TYPE)
                ->withBody($this->streams->createStream($operation->canonicalRequest()));
            if ($timeoutMilliseconds !== null) {
                $request = $request->withHeader('Naatre-Timeout-Ms', (string) $timeoutMilliseconds);
            }
            foreach (AuthenticationHeaders::resolve($this->authenticate, $this->endpoint, $operation) as $name => $value) {
                $request = $request->withHeader($name, $value);
            }
            $response = $this->http->sendRequest($request);
            return BoundedResponseBody::read($response, $this->maximumResponseBytes);
        } catch (Throwable $error) {
            if ($error instanceof ClientException) {
                throw $error;
            }
            throw new ClientException('CLIENT_TRANSPORT_ERROR');
        }
    }
}
