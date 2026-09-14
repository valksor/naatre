# OpenTelemetry and durable audit integrations

Issue #107 owns the optional Go integrations around the dependency-free
`operations.observability-1` runtime contract owned by issue #27. The
normative event and audit shapes remain in `runtime`, `spec/v1/observability.md`,
and `conformance/v1/observability.json`; these adapters neither add a wire
protocol nor define a storage schema.

## Packages and lifecycle ownership

`observability/otel` translates `runtime.TelemetryHooks` to OpenTelemetry
traces, metrics, and logs. An application creates and configures the tracer,
meter, and logger providers, passes them to `otel.New`, installs
`adapter.Hooks()` in `runtime.TelemetryOptions`, and remains responsible for
exporter credentials, resources, sampling, propagation, force-flush, and
provider shutdown. The adapter never starts background workers and never owns
provider lifecycle.

```go
adapter, err := otel.New(otel.Config{
    TracerProvider: tracerProvider,
    MeterProvider: meterProvider,
    LoggerProvider: loggerProvider,
})
if err != nil {
    return err
}

outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{
    Telemetry: runtime.TelemetryOptions{Hooks: adapter.Hooks()},
})
```

The adapter uses fixed span, event, and instrument names. It hashes request,
operation, handler, schema, persisted-operation, principal, stream, signal,
parent, and link references with a domain-separated SHA-256 digest before
export. The hashes preserve joins without exporting the source values.
OpenTelemetry metric points carry exactly the five portable dimensions:
event kind, event stage, operation kind, bounded outcome, and safe error code.
All other values are measurements. Unknown dimensions normalize through the
runtime-owned vocabulary and become a bounded failed event with `INTERNAL`.

Synchronous start and terminal events share one OpenTelemetry span.
Transaction `attempted` opens a span; `denied` is an event on it; committed,
rolled-back, compensated, and indeterminate facts close it. Stream and replay
signals are individual spans. A bounded cache retains completed span contexts
so later asynchronous signals can use real OpenTelemetry parents and links;
the hashed portable parent and link references are also recorded so causality
remains reconstructable when sampling omits either endpoint.

Default limits are 1,024 active spans, 4,096 retained causal references, and
32 links per input event. Applications can lower or raise each limit in
`otel.Config`. Exceeding a limit returns only `RESOURCE_EXHAUSTED`. Adapter
errors otherwise use `OTEL_INVALID_CONFIG`, `OTEL_INVALID_EVENT`,
`OTEL_INITIALIZATION_FAILED`, or `OTEL_EXPORT_FAILED`; they never include a
provider error, panic payload, attribute value, credential, or
implementation-only name. The core
runtime contains every hook error and panic, so telemetry cannot alter data,
error classification, transaction truth, or process liveness.

`observability/audit` is application-required durable admission. The
application implements `audit.Writer` using the transaction handle in the
provided context, creates a recorder, and calls `Admit` from a
transaction-participating mutation handler:

```go
recorder, err := audit.New(audit.WriterFunc(func(ctx context.Context, event runtime.MutationAuditEvent) error {
    tx := transactionFromContext(ctx)
    return appendAuditOutbox(ctx, tx, event)
}), audit.Options{})
if err != nil {
    return err
}

if err := recorder.Admit(ctx, runtime.MutationAuditEvent{
    Stage: runtime.MutationAttempted,
    Operation: "CreateOrder",
    RequestID: requestReference,
}); err != nil {
    return "", err
}
```

`Admit` and `AdmitBatch` use `runtime.RegisterOutbox`. Persistence therefore
runs after handler completion but before commit, receives the active
transaction context, and fails the operation with the runtime-owned
`OUTBOX_PERSIST_FAILED` code if the writer fails. The runtime rolls back both
the business write and outbox row. Cancellation is not detached for outbox
persistence; a cancelled write cannot commit merely to preserve its audit
row. Commit and rollback cleanup remain owned by the transaction provider and
the core runtime.

Transactional admission accepts only `attempted`: predicting a committed,
rolled-back, compensated, or indeterminate fact before commit would be false.
`Recorder.Hook()` separately adapts all core mutation-audit stages to the same
writer as best-effort lifecycle delivery, but its errors never veto or rewrite
a transaction. It must not be described as transactionally durable.

The default admission limits are 16 facts per call and 256 bytes per public
field. Errors use `AUDIT_INVALID_CONFIG`, `AUDIT_INVALID_EVENT`,
`AUDIT_TRANSACTION_REQUIRED`, `AUDIT_TRANSACTION_CLOSED`,
`AUDIT_PERSIST_FAILED`, or `RESOURCE_EXHAUSTED`. Backend errors are never
retained in the public error.

## Runtime and platform boundary

The published implementation profile is
`operations.observability-integrations-go-1` for Go 1.27. The packages contain
no platform-specific code and use the public Go and OpenTelemetry APIs. The
profile intentionally certifies no operating system or architecture; passing
on one host must not be presented as broader native-runtime certification.
Applications must verify their selected OpenTelemetry SDK, exporters, storage
driver, and transaction provider on every deployed target.

## Unsupported optional capabilities

This slice does not own or claim support for:

- OpenTelemetry provider ownership, exporter configuration, sampling,
  resources, propagators, baggage, force-flush, or shutdown;
- a durable-audit storage driver, schema migrations, relay, delivery retry,
  retention policy, encryption, signatures, or WORM certification;
- native-runtime, transport, or framework adapters; or
- operating-system or architecture certification.

Those boundaries are enumerated verbatim in
`conformance/v1/observability-integrations.json`. An application may compose
these capabilities around the adapters, but that composition is outside this
profile until it has its own machine-readable evidence.

## Reproducible conformance

From the repository root, with dependency revisions pinned by `go.mod` and
`go.sum`:

```sh
GOWORK=off go test ./observability/... -count=1
GOWORK=off go test ./runtime -run 'TestCommittedMutationSurvivesTelemetryAndAuditHookFailures|TestPublicTelemetryNormalizationBoundsEveryDimension' -count=1
GOWORK=off go test ./internal/conformance -run TestObservabilityIntegrationProfile -count=1
GOWORK=off go test -race ./observability/... ./runtime ./internal/conformance -count=1
```

The machine-readable profile pins the exact OpenTelemetry API and SDK module
versions and names the positive, negative, boundary, cancellation, and
resource-limit tests. Its digest is pinned by `conformance/v1/suite.json`.
