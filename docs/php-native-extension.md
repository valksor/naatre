# PHP native accelerator

The optional `ext-naatre` profile accelerates bounded deterministic primitives
while `naatre/sdk` remains installable and fully functional without it. Public
SDK/server APIs, stable `CLIENT_*` codes, redaction, generated bindings, and
wire ownership remain in PHP. The companion `naatre/sdk-native` metapackage is
the opt-in installation assertion that requires `ext-naatre`; the base package
does not suggest or require it.

Select a mode explicitly:

```php
<?php

use Naatre\Sdk\Native\Accelerator;
use Naatre\Sdk\Native\Mode;
use Naatre\Sdk\Wire\Json;

Accelerator::configure(Mode::PurePhp);
$portable = Json::canonicalize('{"b":2,"a":1}');

Accelerator::configure(Mode::Native);
$native = Json::canonicalize('{"b":2,"a":1}');
assert($native === $portable);

var_export(Accelerator::diagnostics());
```

This example is identical on PHP 8.5 and PHP 8.6. Forced native mode fails with
`CLIENT_NATIVE_UNAVAILABLE` when the module is absent and
`CLIENT_NATIVE_INCOMPATIBLE` when metadata or a requested capability does not
match. Auto mode consults the extension's per-primitive `defaultEnabled` list.
Version 0.1 ships that list empty: native code is never silently enabled before
the exact build profile has passing parity, safety, and benchmark evidence.

The declared source distribution matrix is Linux, macOS, and Windows on
64-bit x86-64 and ARM64 where the PHP project publishes that runtime profile.
PHP 8.5 and 8.6 are separate artifact lines. NTS/ZTS, debug/release, and
opcache/JIT-off/on are separate test dimensions. Source packages are the
portable release artifact. Any optional binary must be named with OS,
architecture, PHP minor and API, ZTS/NTS, and debug/release, and be accompanied
by SHA-256 checksums and provenance.

The reproducible benchmark runner emits raw JSON with runtime, extension, OS,
architecture, warmup, sample count, elapsed nanoseconds, PHP memory counters,
and correctness digests for parse, structural validation, canonicalization,
semantic hashing, plan compilation, repeated preparation, SDK round trips,
persistent-worker preparation, and PHP/native boundary overhead in both forced
modes. Native allocation counts require the platform profiler used by the
publishing job and must accompany the raw runner output. The output is evidence,
not a checked-in universal performance claim. A primitive may enter
`defaultEnabled` only when its small, medium, and large representative workloads
improve application-level results without materially increasing peak memory.
See
[`ext/naatre/README.md`](../ext/naatre/README.md) for exact build, sanitizer,
fuzz, parity, and benchmark commands.
