# Namespaced protocol and runtime extensions

The `core.extensions-1` profile permits additive experimentation without
allowing an extension to redefine the core language, plan, authorization, or
completion model. Its portable vectors are
[extensions.json](../../conformance/v1/extensions.json).

## Identity and discovery

- **EXT-001:** An extension descriptor contains a globally unique lowercase
  reverse-DNS `id`, Semantic Version 2.0.0 `version`, exact version-specific
  `capability`, and version-specific `implementation` identity. The tuple is
  immutable after registry freeze. Reusing an ID with another version,
  capability, or implementation requires a new schema approval; an
  implementation MUST NOT select a compatible version range implicitly.
- **EXT-002:** `core.*`, `naatre.*`, and every `$` control key are reserved for
  this specification. Extension IDs, capabilities, implementation identities,
  payload namespaces, ordering references, and conflicts are case-sensitive.
  A vendor prefix grants no authority over another registered namespace.
- **EXT-003:** Portable schema discovery lists each descriptor's ID, version,
  capability, implementation identity, extension points, owned directives,
  determinism, side effects, cost behavior, compatibility class, security
  implications, optional-metadata status, ordering, and conflicts. Callback
  code and mutable state are never schema data.

## Negotiation and envelope metadata

- **EXT-100:** Required extension semantics are requested through the exact
  descriptor capability in the request `capabilities` array and in the
  document `requires` set. The decoder rejects an unknown capability or a
  different version's capability before planning or execution. A namespaced
  request payload for a semantic extension is accepted only when that exact
  capability was negotiated.
- **EXT-101:** A response reports only capabilities selected by the server. A
  response extension or namespaced error detail is accepted only for its exact
  negotiated capability. Plans and outcomes may report the negotiated
  ID/version/capability tuples, but MUST NOT retain or expose extension request
  payloads.
- **EXT-102:** Unknown optional metadata is ignored only under an exact
  namespace policy that declares it inert. Ignoring means discarding the value;
  it is not retained for later application interpretation. Inert metadata
  cannot affect execution, document or cache identity, validation,
  authorization, ordering, cost, or interpretation of a result. A portable
  descriptor marked `optionalMetadata` is therefore limited to the `schema`
  point, `none` effects and cost, no directives, and no ordering or conflict
  relationships. Every other unknown or unnegotiated namespace fails closed.
- **EXT-103:** The legacy raw namespace allowlist remains a transport decoder
  facility. It preserves detached JSON for an exact configured namespace but
  conveys no version, capability, runtime activation, or schema support.
  A namespace cannot simultaneously be registered, ignorable, and legacy.

## Extension points and isolation

- **EXT-104:** A descriptor declares one or more of `ast-metadata`,
  `validation`, `planning`, `execution`, `schema`, and `response`. Planning is
  deterministic. Side effects are exactly `none`, `read`, or `write`; non-`none`
  effects require the execution point. Cost behavior is `none`, `declared`, or
  `bounded`. A bounded extension declares a non-zero finite maximum additional
  planning cost no greater than 1,048,576; `none` and `declared` cannot add
  dynamic cost.
- **EXT-105:** The Go reference runtime implements behavioral extension points
  only through the closed directive subsystem in LANG-245 through LANG-249. An
  owned directive must exist, have the extension's exact capability, use only
  declared points and effects, and belong to one extension. Planner-added cost
  is accumulated across every invocation and rejected when it exceeds the
  extension bound. Extensions receive immutable selection, descriptor, typed
  argument, path, and response-annotation views; they receive no registry,
  mutable AST, mutable plan, authorization decision, resource-limit, handler,
  or mutable response handle.
- **EXT-106:** Core decode, structural validation, effect analysis, resource
  admission, planning authorization, dynamic authorization, completion,
  redaction, cancellation, and transaction rules always run at their specified
  boundaries. An extension cannot add an unregistered field, hide a mutation,
  create an alias collision, weaken a serial barrier, disable a security
  control, or turn invalid work into valid work. Plans are structurally checked
  before extension planners and authorized after all bounded decisions.

In-process directives are trusted application code. These API invariants
contain unsupported use and panics; arbitrary malicious Go code requires a
process, worker, or other external isolation boundary.

## Ordering, conflicts, and compatibility

- **EXT-200:** Registry freeze resolves installed extensions by a stable
  topological order. `before` creates an edge from the declaring extension;
  `after` creates an edge to it. References to extensions not installed in the
  snapshot are inert. Lexical ID order breaks every available tie. A cycle
  fails freeze. If either installed descriptor names the other in `conflicts`,
  freeze fails independently of registration order.
- **EXT-201:** Extension ordering governs registry composition and discovery.
  Directive invocation order remains the authored directive-array order from
  LANG-243; extension metadata cannot reorder source syntax. Registry order,
  source order, object member order, and parallel completion order MUST NOT be
  conflated.
- **EXT-202:** Compatibility is declared as `additive`, `behavior-only`,
  `dangerous`, or `breaking`. Version, capability, or implementation changes
  are breaking. Point, directive, effect, cost, security, optionality,
  ordering, and conflict changes require explicit compatibility review and are
  at least dangerous unless a later profile specifies a stricter rule.
- **EXT-203:** Promotion into a later core specification assigns a new reserved
  core capability and normative clauses. Existing vendor IDs remain vendor
  semantics; they are not aliases for the promoted feature. A migration must
  update documents, persisted approvals, schema discovery, conformance claims,
  and deployment release manifests explicitly.

## Persisted operations and conformance

- **EXT-300:** Required extension semantics participate in document identity
  through the canonical `requires` set. Documents that differ only by the
  version-specific extension capability have different `document` digests.
  Persisted records bind the same exact required-capability set and reject a
  mismatch before storage or execution.
- **EXT-301:** Conformance covers exact negotiation, unknown required
  capabilities, explicitly ignorable and rejected optional metadata,
  incompatible versions, deterministic ordering, conflicts, cycles,
  unregistered directive ownership, immutable discovery, aggregate cost, and
  distinct persisted identities.
