<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

final readonly class RuntimeProfile
{
    /** @param list<string> $capabilities */
    private function __construct(public string $name, public array $capabilities)
    {
    }

    public static function fpmUnary(): self
    {
        return new self('php.fpm-unary-1', [Protocol::UNARY]);
    }

    /**
     * The caller may add a streaming capability only when its transport adapter
     * implements that capability. The core reference worker adds cancellation.
     *
     * @param list<string> $implemented
     */
    public static function longLived(array $implemented = []): self
    {
        $capabilities = [Protocol::UNARY, Protocol::CANCELLATION_ACK];
        foreach ($implemented as $capability) {
            if (!in_array($capability, [Protocol::CLIENT_STREAMING, Protocol::SERVER_STREAMING, Protocol::BIDIRECTIONAL_STREAMING, Protocol::IDEMPOTENCY_REPLAY, Protocol::TRANSACTION_PROVIDER, Protocol::SUBSCRIPTION_RESUME], true)) {
                throw new ServerException('REMOTE_CAPABILITY_MISMATCH');
            }
            if (!in_array($capability, $capabilities, true)) {
                $capabilities[] = $capability;
            }
        }
        return new self('php.long-lived-worker-1', $capabilities);
    }

    /** @param list<string> $required */
    public function supports(array $required): bool
    {
        foreach ($required as $capability) {
            if (!in_array($capability, $this->capabilities, true)) {
                return false;
            }
        }
        return true;
    }
}
