# Generator model and common SDK behavior

## Versioned generator input and output

- **GEN-001:** `naatre.generator-model-1` is the versioned, language-neutral
  generator input. It contains the canonical public schema, canonical operation
  documents, variable declarations, request examples, and operation-selected
  result descriptions. It MUST NOT contain handlers, request principals,
  credentials, framework objects, or Go runtime values. A third-party generator
  MUST be able to consume the JSON model without importing a Naatre runtime.
- **GEN-002:** A model identifies its protocol and canonicalization versions.
  Generated output MUST identify the model version, generator algorithm
  version, protocol version, canonicalization version, schema semantic digest,
  and each operation's document semantic digest. Unknown major model or
  protocol versions MUST fail generation; they MUST NOT be interpreted as the
  newest known version.
- **GEN-003:** Generation is reproducible. Given byte-equivalent canonical
  schema and document semantics, the same generator algorithm version and
  configuration MUST produce byte-identical output independent of map order,
  working directory, locale, ambient clock, randomness, or process identity.
  Persisted references use CANON-200 through CANON-221 rather than hashing
  generated source or host object serialization.
- **GEN-004:** The core generator model maps required, optional, nullable,
  lists, maps, input objects, closed and open enums/unions, custom scalars,
  deprecations, and documentation. Every custom scalar used by an operation
  MUST have an explicit configured mapping; an absent mapping fails with
  `GENERATOR_UNMAPPED_SCALAR`. A language generator MUST reject any model state
  it cannot represent exactly rather than substitute a lossy host type.
- **GEN-005:** Operation result generation uses the selected response shape,
  including aliases, fragments, type conditions, skipped fields, nullable
  fields, and streamed pending fields. It MUST NOT substitute the full schema
  object. Successful domain values and operation-result envelopes are distinct:
  an operation result preserves data, structured errors, and explicit
  completion state simultaneously.
- **GEN-006:** Stable symbols derive from portable identifiers under a published
  escaping table. Reserved words receive a deterministic suffix and collisions
  after normalization fail with `GENERATOR_SYMBOL_COLLISION`. Descriptions,
  defaults, identifiers, and enum values are untrusted data and MUST be escaped
  for the target grammar; they are never template code, paths, or executable
  plugin instructions.

## Plugin and filesystem boundary

- **GEN-100:** A generator plugin declares a stable ID, algorithm version,
  accepted model versions, target language, output kinds, configuration schema,
  and deterministic diagnostics. Discovery searches only explicit configured
  locations. An installed plugin is trusted executable code, but schema and
  operation content remain untrusted input. The plugin-host implementation and
  isolation harness are owned by the extracted issue #76.
- **GEN-101:** Diagnostics contain a stable code, JSON Pointer into the model or
  configuration, and a safe message. They MUST NOT echo source values,
  credentials, environment values, or generated source fragments. Unsupported
  features, version skew, name collisions, invalid defaults, unmapped scalars,
  and output drift are generation failures.
- **GEN-102:** Every artifact path is relative to a configured output root.
  Absolute paths, traversal, duplicate normalized paths, and symlink-parent
  traversal fail with `GENERATOR_PATH_ESCAPE`. Writes MUST use a same-directory
  temporary file and rename, so a failed write cannot expose a partial artifact.
  A plugin MUST NOT read or write outside its declared inputs and output root.
- **GEN-103:** Wire-model output and optional framework integration are separate
  artifacts and capability claims. A generated domain model does not imply an
  HTTP, SSE, WebSocket, cache, framework, worker, browser, or operating-system
  adapter. Each official generator and SDK issue owns its executable language
  and packaging evidence; SDK-MATRIX-001 owns the complete matrix.

## Common SDK behavior

- **GEN-200:** Variables default only when absent. Serialization preserves
  explicit null, empty objects, empty lists, and exact scalar spellings, and
  rejects undefined or sparse values where the wire requires a value. Missing,
  null, failed, skipped, pending, and present are distinct result states. Client
  validation is advisory; server validation remains authoritative under
  VALID-004.
- **GEN-201:** Open enum and union variants retain the unknown raw value or
  discriminator in an explicit unknown case. Closed variants and unknown
  required protocol control fields fail with a typed protocol error. Domain
  forward compatibility MUST NOT weaken strict envelope, stream-control,
  persisted-reference, or extension negotiation fields.
- **GEN-202:** The default common limits are 8 MiB compressed response bytes,
  16 MiB decompressed response bytes, and 1 MiB per stream frame; an SDK MAY
  lower them. Oversized, malformed, truncated, invalidly compressed, duplicate-
  key, invalid-Unicode, or trailing responses fail with a typed error and MUST
  NOT return a successful typed result. Decoders use own-property containers so
  `__proto__`, `constructor`, and `prototype` never traverse or mutate a host
  prototype.
- **GEN-203:** Authentication is supplied through an explicit hook. Redirects
  are bounded to five by default and credentials are stripped on every
  cross-origin redirect unless the application explicitly authorizes the exact
  destination. Refresh is single-flight per credential scope and MUST NOT
  automatically replay a non-idempotent operation.
- **GEN-204:** Retry eligibility uses the common reliability and persisted
  operation contract. Network failure alone does not make a write replayable.
  Attempts honor the caller deadline and cancellation, consume bounded response
  bodies, and surface typed transport, remote, protocol, cancellation, and
  unsupported-capability errors without erasing partial data received in a
  valid response.
- **GEN-205:** Unary HTTP and Fetch streaming provide active abort by closing
  the request body and response resource. Authenticated POST SSE uses Fetch
  streaming with request headers and body; a generator MUST NOT claim native
  EventSource support for that shape. An SDK claiming SSE rejects a missing or
  malformed terminal frame as `CLIENT_STREAM_TRUNCATED`.
- **GEN-206:** WebSocket support is optional and requires an advertised adapter.
  Cancellation actively closes the socket and pending consumers, frame bounds
  apply before decode, and normal close before the required terminal frame is a
  truncated stream. An SDK whose backend provides only cooperative cancellation
  or deadline-only termination MUST advertise that weaker guarantee instead of
  the active-close profile.
- **GEN-207:** `sdk.generation-1` binds the model, Go reference generator,
  independent JavaScript generator, byte-identical reference output, semantic
  hashes, transport behavior, and hostile-input cases. Passing it proves the
  core generator/SDK contract only; it does not certify any official SDK or the
  complete language/runtime matrix.

The language-neutral `conformance/v1/generation.json` evidence and its pinned
model/output files are normative for `sdk.generation-1`.
