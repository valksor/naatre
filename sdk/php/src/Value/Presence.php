<?php

declare(strict_types=1);

namespace Naatre\Sdk\Value;

enum Presence: string
{
    case Missing = 'missing';
    case Null = 'null';
    case Pending = 'pending';
    case Present = 'present';
    case Failed = 'failed';
    case Skipped = 'skipped';
}
