# v1 observability migration note

The `operations.observability-1` profile adds dependency-free lifecycle hooks
and expands MUT-008 with the `denied` audit fact plus safe request, operation,
and principal-reference correlation.

Mutation audit hooks now return an error so a best-effort sink failure can be
reported safely without changing transaction truth. Existing hooks must add a
`return nil` after successful delivery. Consumers that switch on audit stages
must add `denied`; it is emitted when runtime authorization rejects a mutation
before its handler runs. Audit storage should accept the new optional
correlation fields without treating their absence as evidence that an older
record is invalid.

Metric adapters must use only the bounded `MetricEvent` dimensions. Existing
adapters that label by operation name, handler, request, tenant, or principal
must remove those labels before claiming this profile. Durations, costs, batch
sizes, retry and connection attempts, active-stream counts, replay work, and
drain progress are exported as measurements instead of labels.
