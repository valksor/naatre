# Hosting a bounded Naatre process

`runtime.ProcessController` places a finite process boundary around request-
level runtime limits. Construct it from validated configuration and an initial
immutable `ProcessRevision`, configure lifecycle hooks, call `Start`, and route
all business work through `Run` or an explicit `Admit`/`Release` lease.
Lifecycle observers run through a finite queue of
`runtime.ProcessHookQueueCapacity` events. They must return promptly; once the
queue fills, observation is best-effort so a broken exporter cannot block
admission or shutdown. Use the optional durable observability integration when
lossless external delivery is required. The queue closes after graceful stop,
or after the final still-owned lease is released following a forced drain.

The [`examples/processhost`](../examples/processhost) package is the reference
`net/http` integration. It exposes liveness and readiness probes, maps
partition rejection to 429 and aggregate rejection to 503 with a bounded
`Retry-After` hint, and binds an `http.Server` to process drain through
`Supervisor`. A caller passes a context derived from its service manager or
signal policy. Cancellation closes admission and the listener concurrently,
waits for the controller's bounded drain, and force-closes HTTP connections if
drain or independently bounded server cleanup cannot finish. Public
`SupervisorResult` values contain only a drain outcome, forced flag, and stable
code; the separately returned error is private operator data and must never be
serialized to a client.

## Deployment contract

Before admission opens:

1. Build and freeze the registry and configuration snapshot.
2. Validate finite global, tenant, principal, queue, byte, stream, remote, drain,
   cleanup, dependency, and connection-rotation limits.
3. Construct `ProcessController` with the complete revision.
4. Verify required delivery and storage dependencies, then call `Start`.

Every ingress path must derive `TenantReference` and `PrincipalReference` from
trusted authentication state, never directly from an untrusted header. Estimate
retained, queued, and result bytes conservatively. Batch, nested, stream, and
remote adapters must reserve their aggregate footprint before starting child
work. References are bounded to
`runtime.ProcessPartitionReferenceMaxBytes`; principal quotas are scoped by
tenant and leases retain only fixed-size domain hashes.

Expose `Health().Live` and `Health().Ready` through separate probes. Alert on
readiness loss without immediately restarting the process. Restart only after
liveness fails following sustained essential-dependency unavailability or a
declared forced outcome; otherwise a transient dependency outage can amplify
into a reconnect storm.

## Shutdown order

1. Invoke `Drain` when the supervisor asks the process to stop.
2. Stop accepting new connections and let the controller reject queued work.
3. Propagate lease cancellation and terminal/resume outcomes according to work
   kind while preserving committing mutations and durable ownership.
4. Use separate `CleanupContext` values for rollback, lease release, and source
   closure.
5. Release a lease only after its underlying handler, stream, remote connection,
   or durable handoff has actually exited.
6. Exit cleanly on `completed`; on `forced`, rely on durable fencing/replay and
   do not report still-owned work as released.

Set the supervisor's termination grace longer than `MaxDrainDuration` plus the
cleanup allowance required by the deployment. The same contract works under a
plain service manager, a container scheduler, or an embedded host; no specific
orchestration or metrics backend is required.

## Ownership and support boundary

Issue #57 and `spec/v1/operations.md` own the normative
`operations.lifecycle-1` state machine. The `runtime` package is its Go
reference implementation. Issue #96 owns only the `examples/processhost`
HTTP/supervisor integration and the Go conformance-runner evidence; neither is
a second protocol, schema, or lifecycle authority.

The supported runtime is Go 1.27 with the standard-library `context`,
`net/http`, and `net.Listener` contracts. The controller and context-driven
supervisor contain no operating-system-specific calls. The independently
executed forced-kill fixture additionally supports Node 24 on POSIX platforms
with `SIGTERM` and `SIGKILL`. Tests use in-memory listeners, so concurrent
workers do not claim fixed ports.

The profile does not implement native Windows service control, systemd launch
configuration, container-orchestrator manifests, automatic restart/backoff,
cross-process listener handoff, TLS or HTTP/2/HTTP/3 configuration, Naatre
protocol decoding, authentication, dependency-probe scheduling, a metrics
exporter, durable lifecycle-event delivery, in-process operating-system forced
kill, upgraded or hijacked connection termination, or a connection pool.
`ConnectionDeadline` supplies only a finite deterministic rotation deadline.
Every optional host capability not explicitly listed as supported in
`conformance/v1/operations.json` is outside `operations.lifecycle-1` and is
unsupported by this integration.

## Reload and rotation

Build the next registry/configuration generation completely, then call
`InstallRevision`. Existing leases retain their old revision; subsequent leases
receive the new one. `RollbackRevision` follows the same atomic boundary. Never
mutate a revision already held by active work.

Use `ConnectionDeadline` to spread routine reconnects deterministically. Supply
only a stable opaque reference; do not emit it as telemetry. Identity expiry may
shorten the returned deadline. During drain, stop establishment before closing
accepted connections and retain replay or durable handoff ownership until it is
transferred.

Run the process benchmarks with:

```sh
go test ./runtime -run '^$' -bench 'Process|ConnectionDeadline' -benchmem
```

The benchmark groups cover immediate admission/release, overload rejection,
completed drain, forced drain, and deterministic connection rotation. The unit
and race suites contain the overload, queue cancellation, noisy-tenant,
uncooperative-handler, dependency-failure, revision rollback, graceful drain,
forced drain, and hook-failure scenarios.

Run the integration profile and its independent forced-kill fixture with:

```sh
go test ./runtime ./examples/processhost ./internal/conformancerunner -count=1
go test -race ./runtime ./examples/processhost ./internal/conformancerunner -count=1
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"operations","command":"run","path":{"source":{"kind":"http-server","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["operations.lifecycle-1"]}' | go run ./cmd/naatre-conformance --require-pass
node conformance/independent/supervise.mjs
```

The operations fixture pins the exact #57 dependency revision and SHA-256
digests for every runtime, host, and runner source used by the machine-readable
profile result.
