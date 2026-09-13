# Decision 0001: dependency-free observability core

Status: accepted for v1.

The core runtime publishes typed lifecycle facts through small Go function
hooks and keeps backend exporters outside the core module. This preserves a
language-neutral event contract, avoids forcing an OpenTelemetry SDK on every
consumer, and lets applications select trace, metric, and logging backends.

Trace and log events carry safe correlation references. Metrics receive a
separate bounded shape that cannot contain operation names, request IDs,
principal references, handler names, or other untrusted label values. Hook
errors and panics are reported out of band and never affect execution. The
metric shape carries numeric lifecycle measurements separately from its fixed
label vocabulary.

Mutation audit remains a distinct append-only, error-returning hook. Its errors
and panics are isolated and safely reported; they do not turn best-effort audit
delivery into a transaction veto. Durable pre-write audit is an admission or
transactional-outbox policy, not a property that a best-effort telemetry
callback can claim. Backend-specific OpenTelemetry and durable-audit
integrations remain independently shippable adapters.
