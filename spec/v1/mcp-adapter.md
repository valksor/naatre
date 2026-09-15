# Model Context Protocol adapter profile

This document defines `adapter.mcp-1`, the protocol-independent registration,
fidelity, trust, and lifecycle contract for Model Context Protocol adapters.
It pins MCP revision `2025-11-25`. It does not claim that Naatre speaks the MCP
wire protocol or that MCP speaks the Naatre protocol. The concrete server and
client transports are owned by issue 103.

## Profile and directions

- **MCP-001:** An implementation MUST treat `naatre-to-mcp` and
  `mcp-to-naatre` as separate directions. Exposure includes only application
  approved Naatre operations and resources. Consumption creates ordinary
  Naatre handlers only from exact trusted registrations.
- **MCP-002:** Initialization MUST negotiate the exact MCP revision
  `2025-11-25` and every capability required by a registration before an
  invoker can run. A missing, unknown, or unsupported required semantic MUST
  reject registration; later discovery cannot silently widen it.
- **MCP-003:** Every manifest and fidelity report MUST identify
  `adapter.mcp-1`, the MCP specification URL, `core.schema-1`,
  `core.protocol-1`, and conformance revision `1.0.0`. Reports MUST state that
  they are not Naatre/MCP wire-compatibility claims.

## Schema and result fidelity

- **MCP-100:** The lossless schema subset MUST use JSON Schema Draft 2020-12
  closed objects, required membership for presence, explicit `null`, exact
  JSON booleans and strings, bounded JSON numeric forms, trusted string codecs
  for extended integers, decimal, timestamp, UUID, and bytes, and closed
  `oneOf` variants with unique required discriminators. Missing and explicit
  null MUST remain distinct.
- **MCP-101:** References, defaults, open objects or variants, ambiguous
  unions, unsupported keywords, lossy numeric narrowing, unbounded streams,
  implicit files, and custom constraints outside the declared subset MUST fail
  schema registration. Unsupported required capabilities MUST fail negotiation
  before a remote tool or resource is invoked.
- **MCP-102:** A lossless tool result MUST carry the closed Naatre
  `complete`/`data`/`errors` envelope in MCP `structuredContent`. Partial data,
  safe error codes and paths remain in that envelope. MCP JSON-RPC faults and
  `isError` tool failures are explicit adaptations and MUST NOT be presented as
  lossless Naatre errors.
- **MCP-103:** The fidelity matrix MUST cover initialization, capabilities,
  tools, resources, resource templates, prompts, progress, cancellation,
  logging, pagination, structured content, errors, transport lifecycle,
  missing/null, numeric and timestamp forms, unions, extended scalars, open
  variants, partial results, streams, files, and custom constraints.

## Trust, policy, and admission

- **MCP-200:** Query or mutation kind, effect, idempotency, retry safety,
  cacheability, cost, batching, transaction participation, and authorization
  MUST come from a trusted Naatre registration. Tool names, descriptions,
  annotations, prompts, resource text, and result prose are never evidence for
  those properties.
- **MCP-201:** Remote descriptions, prompts, resources, annotations, URLs,
  logging data, and tool results MUST remain bounded untrusted data. They MUST
  NOT create or approve an operation, alter a principal or tenant, choose an
  endpoint, supply credentials, become executable configuration, or decide
  authorization.
- **MCP-202:** Remote tool and resource identities MUST match an exact trusted
  registration. Streamable HTTP endpoints and allowed origins MUST be trusted
  application configuration; resource URIs cannot select transport endpoints.
- **MCP-203:** Tool fan-out, retries, progress events, pagination, resource
  reads, pending work, response bytes, and returned content MUST remain under
  normal Naatre admission, authorization, egress, retry, and cost controls.
  The adapter MUST NOT add retries, transactions, or cache scope.

## Identity, isolation, and lifecycle

- **MCP-300:** MCP request IDs, progress tokens, resource URIs, MCP session
  IDs, Naatre request IDs, Naatre operation IDs, and idempotency IDs MUST use
  separate typed namespaces and finite lengths. Progress tokens bind to one
  request in one session.
- **MCP-301:** Concurrent sessions MUST NOT share principal, tenant, progress
  token bindings, request-local cache, loader state, request state, or
  cancellation. Identical token text in two sessions remains two independent
  bindings.
- **MCP-302:** Cancellation, disconnect, deadline expiry, response overflow,
  progress overflow, and owned-process death MUST produce stable Naatre
  outcomes. Cancellation propagates to the application invoker; untrusted
  progress and logging text is never interpreted as policy.
- **MCP-303:** A transport adapter MUST apply finite pending-work, response,
  content, retry, progress-event, and reconnect limits, and MUST release
  request-local state on every terminal outcome.

## Transport profiles

- **MCP-400:** The stdio profile MUST declare process ownership, isolate each
  logical session, reserve stdout for protocol frames, send logs elsewhere,
  propagate cancellation, bound pending frames and responses, and treat EOF or
  process death as a deterministic terminal outcome. It does not reconnect.
- **MCP-401:** The Streamable HTTP profile MUST use one trusted HTTPS endpoint,
  exact Origin validation, an explicit MCP session header, per-session state,
  bounded reconnect, cancellation, pending work, and response bytes. It MUST
  reject userinfo, fragments, query-selected endpoints, and untrusted origins.
- **MCP-402:** Stdio and Streamable HTTP claims MUST be tested and reported
  independently. Evidence for origin validation or reconnect on HTTP cannot be
  reused as stdio evidence, and process ownership claims cannot be inferred
  across transports.

## Evidence and ownership

- **MCP-500:** `conformance/v1/mcp-adapter.json` is the machine-readable
  fidelity and fixture authority. The Go `mcpadapter` package and independent
  JavaScript verifier exercise the contract without listeners or network
  access. Issue 103 owns MCP wire clients and servers; issue 69 owns the final
  combined language and transport matrix.
