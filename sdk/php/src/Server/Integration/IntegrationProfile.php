<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server\Integration;

use Naatre\Sdk\Server\Protocol;
use Naatre\Sdk\Server\RuntimeProfile;

/**
 * Describes a host integration boundary without redefining worker.remote-1.
 */
final readonly class IntegrationProfile
{
    /** @param list<string> $supportedRuntimes */
    private function __construct(
        public string $id,
        public RuntimeProfile $runtime,
        public array $supportedRuntimes,
    ) {
    }

    public static function symfony(RuntimeProfile $runtime): self
    {
        return new self(
            'php.symfony-worker-bridge-1',
            $runtime,
            ['symfony-request-or-worker-host'],
        );
    }

    public static function laravel(RuntimeProfile $runtime): self
    {
        return new self(
            'php.laravel-worker-bridge-1',
            $runtime,
            ['laravel-request-or-worker-host'],
        );
    }

    public static function fpm(): self
    {
        return new self(
            'php.fpm-host-1',
            RuntimeProfile::fpmUnary(),
            ['php-fpm-8.3', 'php-fpm-8.4', 'php-fpm-8.5'],
        );
    }

    public static function roadRunner(): self
    {
        $runtime = RuntimeProfile::longLived([Protocol::TRANSACTION_PROVIDER]);

        return new self(
            'php.roadrunner-worker-1',
            $runtime,
            ['roadrunner-host-api'],
        );
    }

    public static function swoole(): self
    {
        $runtime = RuntimeProfile::longLived([Protocol::TRANSACTION_PROVIDER]);

        return new self(
            'php.swoole-worker-1',
            $runtime,
            ['swoole-host-api', 'openswoole-host-api'],
        );
    }

    public function layer(): string
    {
        return match ($this->id) {
            'php.symfony-worker-bridge-1', 'php.laravel-worker-bridge-1' => 'framework',
            default => 'runtime',
        };
    }

    public function lifecycle(): string
    {
        return match ($this->id) {
            'php.symfony-worker-bridge-1' => 'container-service-with-after-request-reset',
            'php.laravel-worker-bridge-1' => 'container-binding-with-after-request-reset',
            'php.fpm-host-1' => 'one-invocation-per-host-owned-request',
            default => 'persistent-worker-with-after-request-reset-and-drain',
        };
    }

    public function cancellation(): string
    {
        if ($this->id === 'php.fpm-host-1') {
            return 'unsupported-disconnect-is-not-a-cancellation-acknowledgement';
        }
        return $this->runtime->supports([Protocol::CANCELLATION_ACK])
            ? 'cooperative-control-acknowledgement-no-hard-termination'
            : 'unsupported';
    }

    public function pool(): string
    {
        return match ($this->id) {
            'php.fpm-host-1' => 'fpm-process-manager-owned',
            'php.roadrunner-worker-1' => 'roadrunner-owned-bounded-worker-pool',
            'php.swoole-worker-1' => 'swoole-server-owned-bounded-worker-pool',
            default => 'runtime-owned-bounded-pool',
        };
    }

    public function transaction(): string
    {
        if ($this->id === 'php.fpm-host-1') {
            return 'unsupported-by-php.fpm-unary-1';
        }
        return $this->runtime->supports([Protocol::TRANSACTION_PROVIDER])
            ? 'application-provider-per-invocation-no-distributed-atomicity'
            : 'unsupported';
    }

    public function streaming(): string
    {
        return match ($this->id) {
            'php.fpm-host-1' => 'unsupported-rejected-before-dispatch',
            'php.symfony-worker-bridge-1', 'php.laravel-worker-bridge-1' => 'runtime-transport-capabilities-only',
            default => 'not-advertised-without-a-separate-transport-adapter',
        };
    }

    public function requestIsolation(): string
    {
        return match ($this->id) {
            'php.fpm-host-1' => 'new-core-context-per-request',
            'php.roadrunner-worker-1', 'php.swoole-worker-1' => 'new-core-context-and-reset-after-every-job',
            default => 'new-core-context-per-dispatch-plus-container-reset',
        };
    }

    /** @return list<string> */
    public function supportedPlatforms(): array
    {
        return ['linux', 'macos', 'windows-where-the-selected-host-is-supported'];
    }

    /** @return list<string> */
    public function unsupportedCapabilities(): array
    {
        return match ($this->id) {
            'php.symfony-worker-bridge-1' => ['automatic-service-discovery', 'framework-kernel-boot', 'hard-process-termination', 'exactly-once-effects', 'distributed-transactions', 'streaming-without-a-separate-transport-adapter'],
            'php.laravel-worker-bridge-1' => ['automatic-container-registration', 'framework-kernel-boot', 'octane-driver-certification', 'hard-process-termination', 'exactly-once-effects', 'distributed-transactions', 'streaming-without-a-separate-transport-adapter'],
            'php.fpm-host-1' => ['cancellation-acknowledgement', 'client-streaming', 'server-streaming', 'bidirectional-streaming', 'sdk-owned-process-pool', 'forced-rollback-after-disconnect', 'hard-process-termination', 'exactly-once-effects', 'distributed-transactions'],
            'php.roadrunner-worker-1' => ['native-roadrunner-binary-certification', 'sdk-owned-process-supervision', 'hard-process-termination', 'exactly-once-effects', 'distributed-transactions', 'client-streaming', 'server-streaming', 'bidirectional-streaming'],
            'php.swoole-worker-1' => ['native-swoole-extension-certification', 'coroutine-context-propagation', 'sdk-owned-process-supervision', 'hard-process-termination', 'exactly-once-effects', 'distributed-transactions', 'client-streaming', 'server-streaming', 'bidirectional-streaming'],
            default => [],
        };
    }
}
