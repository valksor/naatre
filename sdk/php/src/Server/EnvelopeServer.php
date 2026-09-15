<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;

final class EnvelopeServer
{
    private bool $registered = false;

    public function __construct(
        private readonly HandlerRegistry $handlers,
        private readonly Dispatcher $dispatcher,
        private readonly RuntimeProfile $profile,
        private readonly string $workerId,
        private readonly string $serviceIdentity,
        private readonly string $audience,
        private readonly string $endpoint,
        private readonly string $schemaRevision,
        private readonly string $schemaDigest,
        private readonly string $sessionId,
    ) {
    }

    public function handle(ObjectValue $envelope): ObjectValue
    {
        if (self::string($envelope, 'protocol') !== Protocol::VERSION) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        $kind = self::string($envelope, 'kind');
        $payload = self::object($envelope, 'payload');
        return match ($kind) {
            'register' => $this->register($payload),
            'invoke' => $this->invoke($payload),
            'cancel' => $this->cancel($payload),
            default => throw new ServerException('REMOTE_WORKER_MALFORMED'),
        };
    }

    private function register(ObjectValue $registration): ObjectValue
    {
        if (self::string($registration, 'protocol') !== Protocol::VERSION
            || self::string($registration, 'workerId') !== $this->workerId
            || self::string($registration, 'serviceIdentity') !== $this->serviceIdentity
            || self::string($registration, 'audience') !== $this->audience
            || self::string($registration, 'endpoint') !== $this->endpoint
            || self::string($registration, 'schemaRevision') !== $this->schemaRevision
            || self::string($registration, 'schemaDigest') !== $this->schemaDigest) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
        $requested = self::strings($registration, 'capabilities');
        $accepted = array_values(array_intersect($requested, $this->profile->capabilities));
        if (!in_array(Protocol::UNARY, $accepted, true)) {
            throw new ServerException('REMOTE_CAPABILITY_MISMATCH');
        }
        $descriptors = self::objects($registration, 'handlers');
        $registeredHandlers = $this->handlers->all();
        if (count($descriptors) !== count($registeredHandlers)) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
        foreach ($registeredHandlers as $index => $handler) {
            $descriptor = $descriptors[$index];
            if (self::string($descriptor, 'id') !== $handler->id
                || self::string($descriptor, 'inputSchema') !== $handler->inputSchema
                || self::string($descriptor, 'outputSchema') !== $handler->outputSchema
                || self::string($descriptor, 'codec') !== Protocol::CODEC
                || self::string($descriptor, 'effect') !== $handler->effect->value
                || self::strings($descriptor, 'requiredCapabilities') !== $handler->requiredCapabilities
                || !$this->profile->supports($handler->requiredCapabilities)) {
                throw new ServerException('REMOTE_CAPABILITY_MISMATCH');
            }
        }
        $this->registered = true;
        return self::envelope('registered', ObjectValue::fromPairs([
            ['protocol', Protocol::VERSION],
            ['workerId', $this->workerId],
            ['sessionId', $this->sessionId],
            ['schemaRevision', $this->schemaRevision],
            ['acceptedCapabilities', new ListValue($accepted)],
        ]));
    }

    private function invoke(ObjectValue $payload): ObjectValue
    {
        if (!$this->registered || self::string($payload, 'protocol') !== Protocol::VERSION) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
        $invocation = new Invocation(
            self::string($payload, 'requestId'),
            self::string($payload, 'invocationId'),
            self::string($payload, 'attemptId'),
            self::string($payload, 'handlerId'),
            self::string($payload, 'schemaRevision'),
            self::integer($payload, 'deadlineUnixMilli'),
            self::string($payload, 'delegatedContext'),
            self::value($payload, 'input'),
            self::optionalString($payload, 'idempotencyKey'),
            self::optionalObject($payload, 'parent'),
            self::optionalString($payload, 'resumeCursor'),
        );
        if ($invocation->schemaRevision !== $this->schemaRevision) {
            throw new ServerException('REMOTE_SCHEMA_MISMATCH');
        }
        return self::envelope('result', $this->dispatcher->dispatch($invocation)->toWire());
    }

    private function cancel(ObjectValue $payload): ObjectValue
    {
        if (!$this->registered || !in_array(Protocol::CANCELLATION_ACK, $this->profile->capabilities, true)) {
            throw new ServerException('REMOTE_CANCELLATION_INVALID');
        }
        $invocationId = self::string($payload, 'invocationId');
        return self::envelope('cancelled', ObjectValue::fromPairs([
            ['protocol', Protocol::VERSION],
            ['invocationId', $invocationId],
            ['disposition', 'acknowledged'],
        ]));
    }

    private static function envelope(string $kind, ObjectValue $payload): ObjectValue
    {
        return ObjectValue::fromPairs([['protocol', Protocol::VERSION], ['kind', $kind], ['payload', $payload]]);
    }

    private static function value(ObjectValue $object, string $key): mixed
    {
        $selected = $object->selected($key);
        if ($selected->presence !== Presence::Present) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        return $selected->value;
    }

    private static function string(ObjectValue $object, string $key): string
    {
        $value = self::value($object, $key);
        if (!is_string($value) || $value === '') {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        return $value;
    }

    private static function optionalString(ObjectValue $object, string $key): ?string
    {
        $selected = $object->selected($key);
        if ($selected->presence === Presence::Missing) {
            return null;
        }
        if ($selected->presence !== Presence::Present || !is_string($selected->value) || $selected->value === '') {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        return $selected->value;
    }

    private static function integer(ObjectValue $object, string $key): int
    {
        $value = self::value($object, $key);
        if (!is_int($value)) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        return $value;
    }

    private static function object(ObjectValue $object, string $key): ObjectValue
    {
        $value = self::value($object, $key);
        if (!$value instanceof ObjectValue) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        return $value;
    }

    private static function optionalObject(ObjectValue $object, string $key): ?ObjectValue
    {
        $selected = $object->selected($key);
        if ($selected->presence === Presence::Missing) {
            return null;
        }
        if ($selected->presence !== Presence::Present || !$selected->value instanceof ObjectValue) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        return $selected->value;
    }

    /** @return list<string> */
    private static function strings(ObjectValue $object, string $key): array
    {
        $value = self::value($object, $key);
        if (!$value instanceof ListValue) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        $strings = [];
        foreach ($value->values() as $item) {
            if (!is_string($item) || in_array($item, $strings, true)) {
                throw new ServerException('REMOTE_WORKER_MALFORMED');
            }
            $strings[] = $item;
        }
        return $strings;
    }

    /** @return list<ObjectValue> */
    private static function objects(ObjectValue $object, string $key): array
    {
        $value = self::value($object, $key);
        if (!$value instanceof ListValue) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        $objects = [];
        foreach ($value->values() as $item) {
            if (!$item instanceof ObjectValue) {
                throw new ServerException('REMOTE_WORKER_MALFORMED');
            }
            $objects[] = $item;
        }
        return $objects;
    }
}
