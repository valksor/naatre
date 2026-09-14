<?php

declare(strict_types=1);

require __DIR__ . '/PsrStubs.php';

spl_autoload_register(static function (string $class): void {
    $prefix = 'Naatre\\Sdk\\';
    if (!str_starts_with($class, $prefix)) {
        return;
    }
    $relative = str_replace('\\', '/', substr($class, strlen($prefix)));
    $path = dirname(__DIR__) . '/src/' . $relative . '.php';
    if (is_file($path)) {
        require $path;
    }
});

require dirname(__DIR__) . '/generated/Operations.php';
