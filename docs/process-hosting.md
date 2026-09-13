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

The executable [`examples/processhost`](../examples/processhost) package shows
the smallest `net/http` host. It exposes liveness and readiness probes and maps
partition rejection to 429, aggregate rejection to 503, and both to a bounded
`Retry-After` hint. It deliberately does not decode the Naatre wire protocol,
authenticate headers, handle operating-system signals, or replace the concrete
supervisor and transport integrations.

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
