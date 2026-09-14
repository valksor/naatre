<?php

declare(strict_types=1);

namespace Naatre\Sdk\Protocol;

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;

final readonly class OperationResult
{
    /**
     * @param list<ObjectValue> $errors
     */
    public function __construct(
        public Presence $dataPresence,
        public mixed $data,
        public array $errors,
        public bool $complete,
    ) {
    }

    /** @return self */
    public static function decode(string $input, Operation $operation): self
    {
        $root = \Naatre\Sdk\Wire\Json::decode($input);
        if (!$root instanceof ObjectValue) {
            throw new ClientException('CLIENT_PROTOCOL_INVALID');
        }
        foreach ($root->entries() as $entry) {
            if (!in_array($entry->key, ['complete', 'data', 'errors'], true)) {
                throw new ClientException('CLIENT_PROTOCOL_INVALID');
            }
        }
        $complete = $root->selected('complete');
        if ($complete->presence !== Presence::Present || !is_bool($complete->value)) {
            throw new ClientException('CLIENT_PROTOCOL_INVALID');
        }
        $errorsValue = $root->selected('errors');
        $errors = [];
        if ($errorsValue->presence === Presence::Present) {
            if (!$errorsValue->value instanceof ListValue) {
                throw new ClientException('CLIENT_PROTOCOL_INVALID');
            }
            foreach ($errorsValue->value->values() as $error) {
                if (!$error instanceof ObjectValue) {
                    throw new ClientException('CLIENT_PROTOCOL_INVALID');
                }
                $errors[] = $error;
            }
        } elseif ($errorsValue->presence !== Presence::Missing) {
            throw new ClientException('CLIENT_PROTOCOL_INVALID');
        }
        $selected = $root->selected('data');
        if ($selected->presence === Presence::Present) {
            if (!$selected->value instanceof ObjectValue) {
                throw new ClientException('CLIENT_RESULT_INVALID');
            }
            return new self(Presence::Present, $operation->decode($selected->value), $errors, $complete->value);
        }
        if ($selected->presence !== Presence::Missing && $selected->presence !== Presence::Null) {
            throw new ClientException('CLIENT_PROTOCOL_INVALID');
        }
        return new self($selected->presence, null, $errors, $complete->value);
    }
}
