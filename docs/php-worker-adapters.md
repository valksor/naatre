# PHP worker framework and persistent-runtime adapters

Issue #97 owns the dependency-free PHP host integration layer around
`sdk.php.server-1`. Issue #58 remains the authority for generated handler
contracts, `worker.remote-1` envelopes, capability negotiation, dispatch,
transactions, completion, and public failures. These adapters do not define a
second protocol, schema, retry policy, or public error vocabulary. Issue #69
owns the combined implementation and certification matrix.

## Package and runtime boundary

`Naatre\Sdk\Server\Integration\IntegrationProfile` publishes the host boundary
and wraps the unchanged #58 `RuntimeProfile`. `LifecycleAdapter` applies a
finite accepted-request budget, drain state, and an application-supplied
after-request reset around the core `Dispatcher`. The host owns its framework
container, runtime loop, process pool, signals, credentials, and supervision.
The package never discovers handlers or services automatically.

The source package supports PHP 8.3, 8.4, and 8.5. Its dependency-free fixture
runs with PHP CLI on Linux, macOS, and Windows. A platform or runtime named
below is an integration API boundary, not native binary certification: the
fixture does not boot a Symfony or Laravel kernel, RoadRunner, Swoole,
OpenSwoole, Octane, or FPM. Applications must bind the adapter to the selected
host API, make the request budget finite, and call `drain()` before host
shutdown or worker replacement.

```php
$profile = IntegrationProfile::roadRunner();
$adapter = new LifecycleAdapter(
    $profile,
    $dispatcher,
    afterRequest: $resetContainer,
    maximumRequests: 1000,
);

$result = $adapter->handle($invocation);
$adapter->drain();
```

Failures at this boundary use only existing stable #58 codes. Invalid lifecycle
or budget state is `REMOTE_REGISTRATION_INVALID`; an unknown application,
transaction, or cleanup throwable becomes `REMOTE_WORKER_MALFORMED`. Causes,
credentials, delegated context, protected metadata, runtime class names, and
stack traces are not attached to those exceptions.

## Profile evidence

| Profile | Lifecycle and pool | Cancellation | Transactions | Streaming | Request isolation | Supported host boundary |
| --- | --- | --- | --- | --- | --- | --- |
| `php.symfony-worker-bridge-1` | Container service, finite runtime-owned pool, reset after every dispatch, explicit drain | Delegated to the selected runtime; acknowledgement is cooperative and never hard termination | Application `TransactionProvider` only when the selected runtime advertises it | Only capabilities implemented by a separate transport adapter | New core context plus container reset | Symfony request or worker host on PHP 8.3-8.5 |
| `php.laravel-worker-bridge-1` | Container binding, finite runtime-owned pool, reset after every dispatch, explicit drain | Delegated to the selected runtime; acknowledgement is cooperative and never hard termination | Application `TransactionProvider` only when the selected runtime advertises it | Only capabilities implemented by a separate transport adapter | New core context plus container reset | Laravel request or worker host on PHP 8.3-8.5 |
| `php.fpm-host-1` | One invocation per host-owned request; the FPM manager owns its process pool | Unsupported; disconnect is not acknowledgement or rollback | Unsupported by `php.fpm-unary-1` | Unsupported and rejected before source invocation | New core context per request | FPM 8.3, 8.4, and 8.5 host API |
| `php.roadrunner-worker-1` | Persistent loop, RoadRunner-owned finite pool, reset after every job, explicit drain | `cancellation-ack-1` control boundary only; no hard termination claim | Per-invocation application provider; no distributed atomicity | Not advertised by this adapter | New core context plus reset after every job | Application-provided RoadRunner host API on PHP 8.3-8.5 |
| `php.swoole-worker-1` | Persistent loop, Swoole-owned finite pool, reset after every job, explicit drain | `cancellation-ack-1` control boundary only; no hard termination claim | Per-invocation application provider; no distributed atomicity | Not advertised by this adapter | New core context plus reset after every job | Application-provided Swoole or OpenSwoole host API on PHP 8.3-8.5 |

Cancellation acknowledgement means only that the #58 control boundary was
reached. It is not evidence that synchronous PHP code stopped, a process was
killed, or an external effect was rolled back. Transaction completion remains
local to the application provider; process loss after a write or unknown
commit remains indeterminate under `worker.remote-1`.

## Unsupported optional capabilities

The Symfony profile does not provide automatic service discovery, framework
kernel boot, hard process termination, exactly-once effects, distributed
transactions, or streaming without a separate transport adapter.

The Laravel profile does not provide automatic container registration,
framework kernel boot, Octane driver certification, hard process termination,
exactly-once effects, distributed transactions, or streaming without a
separate transport adapter.

The FPM profile does not provide cancellation acknowledgement, client/server/
bidirectional streaming, an SDK-owned process pool, forced rollback after
disconnect, hard process termination, exactly-once effects, or distributed
transactions.

The RoadRunner profile does not provide native RoadRunner binary certification,
SDK-owned process supervision, hard process termination, exactly-once effects,
distributed transactions, or client/server/bidirectional streaming.

The Swoole profile does not provide native Swoole extension certification,
coroutine-context propagation, SDK-owned process supervision, hard process
termination, exactly-once effects, distributed transactions, or client/server/
bidirectional streaming.

Across all five profiles, production TLS/HTTP/2 transport certification,
framework/runtime release-matrix certification, source-stream credit and
half-close support, opaque-reference providers, idempotency replay,
subscription resume, process isolation, native-runtime status, and complete
official implementation certification are unsupported. Those capabilities
require separately named implementations and executed evidence.

## Reproducible conformance

`conformance/v1/php-worker-adapters.json` pins the exact #58 source revision,
core fixture digest, dependency versions, evidence file digests, and a distinct
lifecycle, cancellation, pool, transaction, streaming, isolation, failure, and
resource-limit vector for every advertised profile.

Run the listener-free evidence from the repository root:

```sh
php sdk/php/tests/worker-adapters.php
node conformance/independent/verify-php-worker-adapters.mjs
composer --working-dir=sdk/php test-worker-adapters
composer --working-dir=sdk/php phpstan
composer --working-dir=sdk/php psalm
```

The PHP fixture is a CLI lifecycle model. It proves the adapter and core
boundaries without claiming that an unexecuted native runtime or framework
version is certified.
