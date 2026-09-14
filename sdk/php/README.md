# PHP SDK core

`naatre/sdk` is the framework-neutral `sdk.php.core-1` client and generated
binding package. It supports PHP 8.3, 8.4, and 8.5 with Composer 2, PHPStan at
maximum level, and Psalm at error level 1. Generated files use PHP 8.3 syntax.

The package models `Int64`, `UInt64`, decimal values, bytes, and timestamps as
lossless value objects. It never routes their wire values through a PHP integer
or float. `ObjectValue` stores string-keyed entries instead of associative
arrays, so an empty object cannot become `[]` and a key such as `"0"` cannot
become integer `0`. `Selected` keeps missing, null, pending, present, failed,
and skipped states distinct. Open enum and union wrappers retain unknown raw
variants. Serializer-neutral PHP attributes describe wire fields, scalars, and
variants without depending on a serializer package.

The generated operation source and manifest consume the shared language-neutral
model and reference output directly. Regenerate and verify them with:

```sh
composer --working-dir=sdk/php install --no-interaction --prefer-dist
composer --working-dir=sdk/php generate
composer --working-dir=sdk/php test
composer --working-dir=sdk/php phpstan
composer --working-dir=sdk/php psalm
git diff --exit-code -- sdk/php/generated
```

## Transport boundary

`Psr18Client` is a synchronous unary client built only on PSR-18 and PSR-7/17
interfaces. It supplies canonical persisted requests, authentication hooks,
bounded responses, query-only bounded retries, and `Naatre-Timeout-Ms` request
deadlines. As PSR-18 has no standard cancellation API, this profile advertises
deadline-only cancellation and never claims active abort. Valid Naatre response
bodies are decoded regardless of HTTP status; 4xx and 5xx are not automatically
treated as transport exceptions.

`StreamingAdapter` is the optional backend contract. Its cancellation strength
must be reported as active abort, cooperative, or deadline-only.
`CloseableStream` closes its backend exactly once after cancellation, timeout,
error, normal completion, or consumer abandonment. The core profile does not
advertise an SSE or WebSocket implementation.

Symfony and Laravel adapters, persistent-worker integration, and active-abort
transport evidence are owned by issue #79. Server-handler and worker bindings
are owned by issue #58. Those optional integrations must consume
`conformance/v1/php-sdk.json`; they do not establish a second wire contract.
The complete official SDK matrix remains owned by issue #69.
