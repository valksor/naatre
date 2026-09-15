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

## Framework integration profiles

Issue #79 owns `sdk.php.adapters-1`, which consumes the core fixture instead of
defining another wire or schema contract. The optional
`Integration\\Symfony\\NaatreClientFactory` and
`Integration\\Laravel\\NaatreClientFactory` are long-lived service factories.
Call `forRequest()` for every FPM request or persistent-worker job and pass only
that request's authentication callback. The resulting `Psr18Client` is
request-scoped and must not be stored in a singleton or reused by another job.
The factories retain only the endpoint, response limit, and shared PSR
services, so credentials and operation state cannot cross request boundaries.

Both adapters accept application-supplied PSR-18 and PSR-17 services. For
Symfony, pass the official all-in-one Symfony bridge to `fromPsr18Bridge()`.
For Laravel, bind the application's selected PSR-18 client and PSR-17
request/stream factories in the container and call `fromContainer()` during
singleton registration, then call `forRequest()` from request or job scope.
The adapters do not call Symfony's native HTTP Client API or Laravel's native
`PendingRequest` API and do not depend on either framework package.

The supported package/runtime boundary is PHP 8.3, 8.4, and 8.5, Composer 2,
PSR HTTP Client 1.0.3, PSR HTTP Factory 1.1.0, and PSR HTTP Message 2.0. The
framework profiles support synchronous unary requests through those PSR
interfaces. Cancellation is deadline-only: `Naatre-Timeout-Ms` is propagated,
but pure PSR-18 cannot promise active abort. Responses are read incrementally,
bounded by `maximumResponseBytes`, and always closed.

Public failures expose only stable `CLIENT_*` codes. Provider exceptions are
not chained, authentication header values never appear in public messages, and
authentication cannot override framing, media-type, host, or deadline headers.
The complete code list is pinned in `conformance/v1/php-adapters.json`.

The adapter profile does not support active abort, Symfony's native HTTP Client
API, Laravel's native `PendingRequest` API, framework-managed retry or
authentication state, SSE, WebSocket, server handlers, or worker bindings. It
cannot enforce redirect or cross-origin credential policy through generic
PSR-18, so applications must disable redirects in the supplied client. It also
does not certify native FPM, RoadRunner, FrankenPHP, Swoole, Laravel
Octane, Symfony Runtime, or framework release matrices; its FPM and persistent
worker fixtures are deterministic CLI lifecycle models. Server handlers and
worker bindings remain owned by issue #58, and #69 owns the complete official
SDK/runtime matrix.

Run the adapter evidence without a listener or network connection:

```sh
php sdk/php/tests/adapters.php
node conformance/independent/verify-php-adapters.mjs
composer --working-dir=sdk/php test-adapters
```

The machine-readable profile pins the core fixture digest, the exact PSR
interface revisions modeled by the fixtures, all source evidence digests, and
positive, negative, boundary, cancellation, and resource-limit vectors.
