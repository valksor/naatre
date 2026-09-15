# v1 remote-worker migration note

The `worker.remote-1` profile adds an out-of-process handler boundary without
changing the Go embedded runtime. Existing HTTP clients remain clients and do
not become server integrations.

Applications adopting the gateway model must replace native-object handler
arguments with shared schema-defined values or scoped opaque references,
assign stable handler IDs, declare effects and required capabilities, and
register against one exact schema revision. Principal and tenant fields must
move out of document input and into authenticated, audience-bound delegated
context. Worker output is untrusted until gateway schema validation and public
completion succeed.

Deployments must add explicit process, credential, connection-pool, upgrade,
queue, request-state, reference, stream, and cleanup ownership. Mutation replay
after a possible write is disabled unless the existing idempotency profile has
admitted evidence. Transaction handlers remain unavailable without an
advertised provider capability.
