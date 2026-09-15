# Quick starts

All quick starts consume checked source rather than copied Markdown snippets. Run the command recorded beside the source in [evidence.json](evidence.json); CI runs the same language toolchains. The Go, JavaScript/TypeScript, PHP, Python, and Rust clients consume the same language-neutral `GetAccount` schema and operation from [the generator model](../../conformance/v1/generator-model.json). The gateway/worker examples separately consume [the same remote-worker schema and vectors](../../conformance/v1/remote-workers.json).

## Go server and gateway

For an embedded Go server, explicitly register schema types and typed handlers, freeze the registry, configure `AuthorizationDenyByDefault`, adapt the immutable snapshot with `transport/http.RuntimeExecutor`, and place the router-free handler behind a bounded process host. The executable assembly is [the Go server quick start](../../examples/v1/server/server.go); registration and authorization are covered by [the registry tests](../../runtime/registry_test.go) and [deny-by-default tests](../../runtime/authorization_test.go). Process admission and drain are demonstrated by [examples/processhost](../../examples/processhost/host.go).

For a foreign-language worker, use the [bounded reference gateway quick start](../../examples/v1/gateway/gateway.go). It pins service identity, audience, schema revision/digest, handler allowlist, authorization callbacks, one in-flight invocation, and byte limits. A Go gateway plus PHP, Python, JavaScript/TypeScript, or Rust workers is a remote-worker deployment. It is never an independent native runtime in those languages.

## Clients

The Go client quick start is [the generated operation test](../../sdk/go/generated/operations_test.go): create `GetAccount`, add typed raw variables, serialize canonical request bytes, and decode selected states. JavaScript/TypeScript uses [the generated binding and executable generator test](../../sdk/typescript/sdkgen/generator.test.mjs). PHP uses [the core SDK test](../../sdk/php/tests/run.php). Python uses [the synchronous and asynchronous operation test](../../sdk/python/tests/test_operation.py). Rust uses [the generated request and result conformance test](../../sdk/rust/tests/conformance.rs).

Every client preserves absent, explicit null, pending, present, failed, and skipped states where its profile requires them. Never replace an extended integer or decimal string with a JavaScript number, a PHP float, or a binary floating-point intermediate. Do not turn a timestamp into a local-zone date: validate its explicit offset and preserve nanoseconds or use the profile's UTC normalization rule.

## Foreign server workers

The executable stdio examples are [JavaScript/TypeScript](../../conformance/independent/remote-worker.mjs), [PHP](../../examples/v1/workers/php.php), [Python](../../examples/v1/workers/python.py), and [Rust](../../sdk/rust/examples/remote_worker.rs). Each validates the shared fixture, answers registration, invocation, and cancellation envelopes, enforces the fixture frame bound, and returns the same success/error vectors. Stdio is a conformance transport; production uses authenticated, bounded HTTP/2 transport and supervision as described in [remote-worker deployment](../remote-workers.md).

These examples prove the framed worker contract, not native runtime status, production gateway readiness, hard cancellation, process isolation, framework integration, or exactly-once effects. Cancellation acknowledges a request to stop; it cannot prove an external side effect stopped or rolled back.
