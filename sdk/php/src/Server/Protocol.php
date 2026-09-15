<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

final class Protocol
{
    public const VERSION = 'naatre.remote-worker.v1';
    public const CODEC = 'naatre.json-1';
    public const UNARY = 'unary-1';
    public const CLIENT_STREAMING = 'client-streaming-1';
    public const SERVER_STREAMING = 'server-streaming-1';
    public const BIDIRECTIONAL_STREAMING = 'bidirectional-streaming-1';
    public const CANCELLATION_ACK = 'cancellation-ack-1';
    public const IDEMPOTENCY_REPLAY = 'idempotency-replay-1';
    public const TRANSACTION_PROVIDER = 'transaction-provider-1';
    public const SUBSCRIPTION_RESUME = 'subscription-resume-1';

    private function __construct()
    {
    }
}
