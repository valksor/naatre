<?php

declare(strict_types=1);

namespace Naatre\Sdk\Native;

enum Mode: string
{
    case Auto = 'auto';
    case Native = 'native';
    case PurePhp = 'pure-php';
}
