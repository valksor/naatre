# Authentication, authorization, and interceptor security

## Security layers and principal

- **SEC-001:** Authentication establishes a principal. Schema visibility
  determines which declarations that principal may discover. Per-object
  authorization decides whether a selected resource may be observed or
  changed. Provider-side filtering determines collection membership. These are
  separate controls; success at one layer MUST NOT imply success at another.
- **SEC-002:** A request principal contains a subject, optional tenant, claims,
  and an authorization revision. The runtime MUST carry it through the request
  context and MUST prevent callers or policy callbacks from mutating admitted
  claims. A handle, cursor, cache entry, or prior decision is not a principal.
- **SEC-003:** Authentication failure and authorization denial are distinct
  internally. A deployment MAY intentionally map both to the same public shape
  when disclosing the distinction would reveal a protected resource.

## Authorization decisions

- **SEC-100 Production default:** Production deployments MUST use
  deny-by-default authorization. The compatibility allow-by-default mode is
  intended only for explicitly trusted or migrating deployments and MUST NOT be
  presented as the production recommendation.
- **SEC-101 Protected-work boundary:** Authorization MUST complete before
  evaluating protected arguments, reading a protected field, invoking its
  resolver, acquiring a reusable protected result, or exposing collection and
  cursor metadata. Denial MUST prevent that protected work from starting.
- **SEC-102 Safe denial:** A runtime denial uses code `UNAUTHORIZED`, message
  `access denied`, and the requested response path after alias expansion. It
  MUST NOT expose the hidden declaration name, policy error, argument values,
  current object, item count, cursor position, cache key, or remote target.
- **SEC-103 Policy input:** A dynamic policy receives the immutable principal,
  operation kind, portable descriptor metadata, safe response path, and an
  isolated copy of the already-authorized current object. It MUST NOT receive
  raw operation arguments through the core callback contract. Policy
  diagnostics and audit sinks MUST apply the same redaction boundary.
- **SEC-104 Static and dynamic checks:** A planning policy MAY reject a
  descriptor using only request-independent plan facts. Every execution still
  requires a current dynamic decision at the protected-work boundary. A static
  allow MUST NOT authorize later execution, cache reuse, replay, or delegation.
- **SEC-105 Operation and composition identity:** Both static and dynamic
  checks receive the selected operation kind. Aliases change only the safe
  response path. Fragment expansion, nested calls, and parallel grouping MUST
  produce the same checks as the equivalent expanded sequential selection.
- **SEC-106 Decision lifecycle:** An allow decision is current only before its
  expiry and while its non-empty authorization revision exactly matches the
  principal revision. An expired, revoked, malformed, unknown-scope, errored,
  or panicking decision MUST fail closed. Expiry equality is expired.
- **SEC-107 Cache scope:** An authorization decision declares `no-store`,
  `principal`, or `tenant` scope. Empty scope means `no-store`. Principal scope
  requires and binds the subject. Tenant scope requires and binds the tenant.
  A protected cache key MUST also bind schema and authorization revisions and
  MUST expire no later than the decision. Unknown scopes are denied.
- **SEC-108 Cross-profile equivalence:** Planned, cached, batched, streamed, and
  remote execution MUST apply an equivalent current decision for the same
  principal, resource, schema revision, authorization revision, and operation
  kind. Optimization or transport placement MUST NOT widen authority.
- **SEC-109 Policy budget:** Hosts MUST bound policy evaluation with request
  cancellation, deadlines, and resource budgets. The core invokes at most one
  dynamic policy check per selected handler at each object occurrence. Absolute
  timing noninterference is not promised; implementations MUST hide protected
  identifiers, counts, ordering, cursor positions, and worker/cache topology.
- **SEC-120 Planning denial:** A static planning denial produces the generic
  `POLICY_DENIED` diagnostic before business execution. The diagnostic source
  is the client-supplied selection location and does not disclose a hidden
  declaration or policy reason.

Valid: an aliased field is denied at path `account/private` with the generic
public denial while its resolver start count remains zero.

Invalid: a policy error string, argument token, underlying field name, or
forbidden collection length appears in the response.

## Collections, caches, and delegation

- **SEC-200 Collection membership:** Provider-side filtering MUST establish
  authorized membership before returning a collection to core execution.
  Per-object checks still apply to selected members. Filter, sort, count,
  pagination, cursor, history-loss, and total metadata MUST be computed only
  over the authorized view. A whole-collection denial exposes neither values
  nor length.
- **SEC-201 Reuse boundary:** A cached or batched value may be reused only after
  a current authorization check and only within the decision's cache scope.
  Batch coalescing MUST NOT merge principals or tenants merely because their
  provider key matches.
- **SEC-202 Remote delegation:** A remote request carries a least-authority,
  audience-bound authorization context. The delegating and receiving sides
  both enforce current policy; a worker identity, queue receipt, or signed job
  proves transport provenance but not end-user authority.
- **SEC-203 Async references:** Blob references, job handles, result receipts,
  and terminal retrieval each require current authorization. Their public
  not-found or denied shapes MUST NOT reveal whether protected work exists.

Valid: two principals request the same provider row and receive independently
authorized results without sharing a principal-scoped cache entry.

Invalid: a cached object, batch slot, blob identifier, or remote receipt is
treated as sufficient authority.

## Interceptors and trusted callbacks

- **SEC-300 Ordering:** Interceptors wrap every authorized handler in this
  deterministic outer-to-inner order: global, operation kind, owner type,
  field, then exact handler. Registration order is preserved within one level;
  completion unwinds in reverse order.
- **SEC-301 One-shot continuation:** An interceptor receives a no-argument
  continuation fixed to the authorized descriptor, source, input, context, and
  execution budget. The continuation may be called at most once. It exposes no
  API for selecting another handler or replacing the context, source, or input.
- **SEC-302 Core invariants:** Interceptors MUST NOT bypass authorization,
  cancellation, concurrency or output limits, completion, or public error
  redaction. Cancellation observed before or during an interceptor wins over a
  value it returns.
- **SEC-303 Trust boundary:** In-process policies and interceptors are trusted
  application code, not a security sandbox. The runtime enforces its callback
  shape, one-shot continuation, panic containment, and fail-closed decisions;
  hostile untrusted code requires process isolation outside the core profile.

Valid: an interceptor returns an application error containing a secret; the
public response retains only the generic handler-failure shape.

Invalid: an interceptor calls its continuation twice, swaps in a different
handler, or returns protected data after cancellation.

## Replay and cursor authorization

- **SEC-400 Current authorization:** Authorization is applied independently to
  handle creation, stream attachment, every replay candidate, every live event,
  and terminal or result retrieval. Revocation, expiry, or policy change stops
  future replay and live delivery even when attachment previously succeeded.
- **SEC-401 Binding:** Handles and cursors are opaque and integrity protected.
  Before protected work or metadata lookup, they MUST be bound to the current
  subject, tenant, schema revision, authorization revision, resource, and
  operation. A mismatch uses the same safe rejection as an unknown reference.
- **SEC-402 Mixed history:** Replay filters each candidate under current policy.
  Forbidden candidates do not advance client-visible counts, ordering, cursor
  positions, watermarks, or timing metadata.
- **SEC-403 Safe history loss:** History-loss and resume failures expose only a
  generic unavailable outcome. They MUST NOT reveal protected topic names,
  event identifiers, skipped counts, earliest positions, or whether the cursor
  was structurally valid before authorization.

Valid: a matching cursor replays only currently authorized candidates and
returns an opaque next cursor bound to the current revisions.

Invalid: a cursor from another principal, tenant, schema revision, or
authorization revision reaches broker lookup or reveals a loss count.
