<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

use Naatre\Sdk\Exception\ClientException;

final readonly class Selected
{
    /**
     * @param list<array<string, mixed>> $errors
     */
    private function __construct(
        public Presence $presence,
        public mixed $value = null,
        public array $errors = [],
        public ?string $reason = null,
    ) {
    }

    /** @return self */
    public static function missing(): self
    {
        return new self(Presence::Missing);
    }

    /** @return self */
    public static function null(): self
    {
        return new self(Presence::Null);
    }

    /** @return self */
    public static function pending(): self
    {
        return new self(Presence::Pending);
    }

    /** @return self */
    public static function present(mixed $value): self
    {
        return new self(Presence::Present, $value);
    }

    /**
     * @param list<array<string, mixed>> $errors
     * @return self
     */
    public static function failed(array $errors): self
    {
        if ($errors === []) {
            throw new ClientException('CLIENT_RESULT_INVALID');
        }
        return new self(Presence::Failed, errors: $errors);
    }

    /** @return self */
    public static function skipped(string $reason): self
    {
        return new self(Presence::Skipped, reason: $reason);
    }
}
