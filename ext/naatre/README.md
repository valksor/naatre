# ext-naatre

`ext-naatre` 0.1 is an optional accelerator for `naatre/sdk`. The portable PHP
implementation remains the public wire and error oracle. Native selection is
request-local, explicit, introspectable, and disabled in `auto` mode until a
published benchmark profile enables an individual capability.

The extension supports PHP 8.5 and 8.6. A binary is valid only for its exact
PHP API, Zend module API, ZTS/NTS mode, debug/release mode, operating system,
and architecture. PHP rejects an API mismatch at load time, and the adapter
also compares `PHP_VERSION_ID`, ABI version, and pointer width before selecting
the extension. Never copy one minor's module into another minor's extension
directory.

## Install (PIE)

The extension is distributed with [PIE](https://github.com/php/pie), the
PHP Foundation's Composer-based extension installer (the modern successor to
PECL). Its metadata lives in [`composer.json`](composer.json) with
`"type": "php-ext"`.

```sh
pie install naatre/naatre-ext
```

PIE resolves the package from Packagist, builds it against the active PHP
(passing `--enable-naatre`), and installs and registers `naatre.so`. The
userland library `naatre/sdk` `suggest`s this package and transparently falls
back to its pure-PHP implementation when the extension is absent.

## Unix source build

```sh
cd ext/naatre
phpize
./configure --enable-naatre
make -j2
make test TESTS=tests
php -n -d extension=json -d extension=modules/naatre.so --ri naatre
```

On platforms where JSON is compiled into PHP, omit `-d extension=json`. Run SDK
parity from the repository root:

```sh
php -d extension=ext/naatre/modules/naatre.so sdk/php/tests/native.php
NAATRE_EXTENSION=ext/naatre/modules/naatre.so node conformance/independent/verify-php-native.mjs
php -d extension=ext/naatre/modules/naatre.so sdk/php/benchmarks/native.php --samples=30 --warmup=5 --output=/tmp/naatre-native-benchmark.json
```

The Linux source-build matrix also enables FPM and runs three FastCGI requests
through one static worker on an ephemeral port:

```sh
python3 ext/naatre/fpm/isolation.py /path/to/php-src/sapi/fpm/php-fpm ext/naatre/fpm/isolation.php
```

Run the checked-in corpus under a sanitizer-built PHP and extension with:

```sh
USE_ZEND_ALLOC=0 ASAN_OPTIONS=detect_leaks=1:halt_on_error=1 UBSAN_OPTIONS=halt_on_error=1 php -d extension=ext/naatre/modules/naatre.so sdk/php/tests/native.php
for corpus in ext/naatre/fuzz/corpus/*.json; do USE_ZEND_ALLOC=0 ASAN_OPTIONS=detect_leaks=1:halt_on_error=1 UBSAN_OPTIONS=halt_on_error=1 php -d extension=ext/naatre/modules/naatre.so ext/naatre/fuzz/php-facing.php < "$corpus"; done
```

Distribution is handled by PIE from the tagged Git source on Packagist (see
"Install (PIE)" above); there is no separate PECL source tarball to build.

## Windows source build

Use the matching PHP 8.5 or 8.6 SDK and architecture from a Developer Command
Prompt. ZTS/NTS and debug/release must match the target PHP executable.

```powershell
phpize
configure --enable-naatre --with-php-build=PATH_TO_DEVEL_PACK
nmake
php -n -d extension=json -d extension=x64\Release\php_naatre.dll --ri naatre
php -d extension=x64\Release\php_naatre.dll sdk\php\tests\native.php
```

## Ownership and safety

MINIT registers only final, non-serializable opaque classes. MSHUTDOWN owns no
cache. RINIT/RSHUTDOWN own no request globals. Representations and compiled
plans retain only extension-owned immutable canonical bytes and decoded value
trees; their destructors release all of that state in the creating request.
There is deliberately no persistent cache, callback, zval, principal, tenant,
loader, executor, or transaction state. Forked and persistent workers therefore
cannot inherit a request object through extension storage.

Every data entry point requires positive depth, node, input-byte, and
output-byte limits. Parsing rejects duplicate keys after escape decoding,
malformed JSON, invalid UTF-8, unsafe integers, depth exhaustion, node
exhaustion, and output exhaustion. Preemptive cancellation is not implemented
or advertised.
