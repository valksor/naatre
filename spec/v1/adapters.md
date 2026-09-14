# Interoperability adapters

This document defines `core.adapters-1`, the protocol-neutral fidelity contract
for importing, exporting, consuming, and exposing Naatre capabilities. It does
not define a complete OpenAPI, GraphQL, JSON-RPC, gRPC, or Connect integration.

## Profile and directions

- **ADAPT-001:** An adapter MUST publish a machine-readable fidelity report
  before deployment or invocation. The report identifies the adapter,
  direction, pinned upstream specification, independent Naatre schema identity,
  wire version, approved operations, policy claims, and every mapping as
  `lossless`, `explicitly-adapted`, `unsupported`, or `application-supplied`.
  Any unsupported or unresolved required mapping makes the report `rejected`
  and MUST prevent registration and invocation.
- **ADAPT-002:** `schema-import`, `schema-export`, `runtime-consume`, and
  `runtime-expose` are separate directions. Evidence for one direction MUST NOT
  be reused as evidence for another direction.
- **ADAPT-003:** OpenAPI is pinned to 3.2.0, GraphQL to September 2025, OpenRPC
  to 1.4.1, and the protobuf/gRPC/Connect family to protobuf Edition 2024 plus
  Connect protocol revision `fac060371d74da4205f28ef504d078d2d2ce286f`.
  Other revisions require a distinct report and compatibility evidence.

## Mapping fidelity

- **ADAPT-100:** The fidelity matrix MUST cover operation, type, and transport
  semantics for scalar ranges, nullable and optional input, one-of and union
  discrimination, projections and field masks, pagination, deadlines,
  metadata, authentication, error paths, partial failure, streaming,
  cancellation, batching, transactions, redirects, and network egress.
- **ADAPT-101:** An adapter MUST preserve exact scalar ranges and absence,
  explicit null, and present-value states. A host-language zero value, GraphQL
  omission, JSON `null`, protobuf default, and an unset presence-aware field
  MUST NOT be silently collapsed.
- **ADAPT-102:** GraphQL non-null propagation, JSON-RPC result/error envelopes,
  OpenAPI status and problem responses, and gRPC/Connect status details MUST be
  represented as explicit adaptations. Public errors retain safe Naatre
  response paths and MUST NOT expose upstream bodies, credentials, endpoints,
  stack traces, or internal status details.
- **ADAPT-103:** A round-trip claim applies only to matrix entries marked
  `lossless` and exercised in both directions by the same fixture. Renaming,
  default insertion, numeric narrowing, null propagation, status translation,
  or transport adaptation disqualifies a general round-trip claim.

## Policy and registration

- **ADAPT-200:** Effect, idempotency, retry safety, cacheability, cost,
  batching, transaction, and authorization metadata are application policy.
  An imported description that cannot establish a value MUST require explicit
  application configuration before registration.
- **ADAPT-201:** HTTP methods, GraphQL query or mutation labels, OpenRPC method
  names, protobuf method names, and operation-name conventions are not proof of
  effect, idempotency, retry, transaction, or shared-cache safety. Adapters
  MUST obey declared Naatre policy and MUST NOT infer these claims.
- **ADAPT-202:** Imported services expose only operations present in an
  application allowlist. Vendor extensions, annotations, directives, options,
  reflection, upstream discovery, and metadata MUST NOT add, approve, rename,
  or widen a registration.

## Selection and execution

- **ADAPT-300:** A supported nested field/call selection maps to a deterministic
  sorted, duplicate-free backend projection using underlying names rather than
  response aliases. Every visited node counts against a configured fan-out
  bound. Unsupported composition controls MUST fail before backend work.
- **ADAPT-301:** Pagination, batching, deadlines, cancellation, partial
  failures, and authentication forwarding remain explicit per-operation
  adaptations. Batching is only an opportunity when Naatre metadata declares
  it eligible; an adapter MUST NOT multiply retries or transaction boundaries.
- **ADAPT-302:** Exporting arbitrary Naatre composition as one REST route is
  unsupported. An export adapter MAY expose an explicitly declared operation
  or documented lossless subset, but MUST reject a composition it cannot
  preserve.

## Trust boundary

- **ADAPT-400:** Imported documents and remote responses are untrusted input.
  Parsers MUST use strict decoding, finite byte, depth, operation, diagnostic,
  projection, and response limits, and MUST reject unknown semantics instead
  of treating them as annotations.
- **ADAPT-401:** Remote endpoints and origins MUST be explicitly constrained.
  Redirects are denied unless a protocol-specific profile proves a bounded
  policy; credential and metadata forwarding use an exact allowlist, strip
  unsafe hop-by-hop and forwarding headers, and never follow a redirect with
  credentials. Cancellation and deadlines propagate to backend work.
- **ADAPT-402:** Naatre schema identities and wire versions are independent.
  An adapter MUST NOT introduce Deepr identities, a Deepr compatibility claim,
  or a legacy Deepr parser.

## Evidence and ownership

- **ADAPT-500:** `conformance/v1/adapters.json` is the language-neutral contract
  fixture. It includes per-direction mappings, scalar boundaries, absence and
  null, partial failures, streaming mismatch, cancellation, authentication
  forwarding, GraphQL non-null propagation, JSON-RPC envelopes, round-trip
  limits, and an approved-operation example.
- **ADAPT-501:** The Go reference path consumes the fixture's explicitly
  approved HTTP-JSON service through ordinary runtime registration and a
  bounded, origin-allowlisted client. Multi-SDK execution belongs to #69.
- **ADAPT-502:** GraphQL integration belongs to #92, OpenAPI integration to
  #93, protobuf/gRPC/Connect integration to #95, and OpenRPC/JSON-RPC
  integration to #98. Those optional profiles MUST consume this contract and
  publish their own direction-specific reports.
