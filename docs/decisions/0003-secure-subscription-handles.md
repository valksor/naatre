# Decision 0003: secure subscription handles

Status: accepted for v1.

Browser GET delivery is represented by an opaque subscription identifier whose
server-side record remains bound to principal, tenant, canonical operation and
variable identities, schema and authorization revisions, finite limits, and an
expiry. The identifier is deliberately not a bearer capability. Every action
and protected frame is reauthorized against the retained binding.

Authenticated POST establishment returns the initial snapshot and one protected
reconciliation cursor. Fetch sends bearer credentials and resume state in
headers. Native EventSource is limited to a same-origin cookie path and uses the
server-held reconciliation cursor for its first attachment, so neither path
puts documents, variables, credentials, or cursors into URLs.

The broker boundary preserves Naatre logical frames and publishes explicit
fidelity. The in-process adapter proves the snapshot/history/live handoff.
External Mercure integration is acceptable only with private narrow topics and
an adapter layer retaining Naatre authorization, terminal, replay, loss, and
schema semantics. Issue 105 supplies the separately profiled PostgreSQL, Redis
Streams, and NATS JetStream delivery-source adapters plus reconnect-safe
canary/rollback control without changing this decision's protocol authority.
