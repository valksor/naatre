<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

final class HandlerRegistry
{
    /** @var array<string, RegisteredHandler> */
    private array $handlers = [];

    public function register(RegisteredHandler $handler): void
    {
        if (isset($this->handlers[$handler->id])) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
        $this->handlers[$handler->id] = $handler;
    }

    public function get(string $id): RegisteredHandler
    {
        return $this->handlers[$id] ?? throw new ServerException('REMOTE_HANDLER_UNKNOWN');
    }

    /** @return list<RegisteredHandler> */
    public function all(): array
    {
        return array_values($this->handlers);
    }
}
