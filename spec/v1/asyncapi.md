# AsyncAPI event-description profile

The `adapter.asyncapi-1` profile maps Naatre event metadata to the
[AsyncAPI 3.0.0 specification](https://www.asyncapi.com/docs/reference/specification/v3.0.0).
The normative source is that exact revision; later AsyncAPI 3.0.x revisions do
not enter this profile automatically. AsyncAPI describes discoverable metadata.
It does not replace Naatre execution, wire, authorization, ordering, replay,
retry, partial-result, or event-envelope rules.

## Version, directions, and identity

- **ASYNCAPI-001:** Export MUST emit AsyncAPI `3.0.0`, profile
  `adapter.asyncapi-1`, exporter `asyncapi-exporter-1`, and the exact normative
  source URL above. The exporter version, Naatre schema revision and digest,
  capability revision, canonicalization revision, and `core.events-1` envelope
  revision MUST be present in `x-naatre-revisions`.
- **ASYNCAPI-002:** Export and import are separate directions. Export proves
  that a caller-authorized Naatre model can be described by the lossless
  subset. Import validates inert metadata and returns a fidelity report. Import
  MUST NOT register operations, start a listener, resolve a reference, connect
  to a server, or invoke a business handler.
- **ASYNCAPI-003:** Every server, channel, operation, message, schema reference,
  correlation declaration, security scheme, and transport binding MUST carry
  `x-naatre-identity`. It contains a stable Naatre ID and declaration revision
  plus the schema, capability, canonicalization, and event-envelope revisions.
  Display names, map order, and regenerated component order are not identities.
- **ASYNCAPI-004:** Equivalent authorized schema documents and equivalent event
  capabilities MUST produce identical UTF-8 JSON bytes. Object keys use
  `c14n-1` ordering, declaration arrays are normalized by stable identity, and
  no time, process, path, random, or network value enters export.

## Fidelity matrix

Every document carries this matrix in `x-naatre-fidelity`. An imported document
missing a row, duplicating a row, marking a required row unsupported, or
omitting an explicit resolution is rejected before it can be returned for
registration.

| Feature | Classification | Resolution |
| --- | --- | --- |
| schema identity | lossless | Naatre revision and digest remain authoritative |
| capability revision | lossless | exact revision is retained |
| canonicalization | lossless | exact Naatre canonicalization revision is retained |
| correlation identifiers | lossless | each identity, lifetime, and trust boundary is retained separately |
| event envelope | explicitly adapted | CloudEvents structured JSON remains governed by `core.events-1` |
| ordering, replay, terminal, and errors | explicitly adapted | Naatre channel extensions preserve logical stream semantics |
| transport bindings | explicitly adapted | only implemented, evidence-backed Naatre bindings are declared |
| partial results | explicitly adapted | Naatre logical stream frames remain authoritative |
| authorization and security | application supplied | AsyncAPI security metadata never grants Naatre authorization |
| execution semantics | application supplied | descriptions cannot invoke handlers or redefine effects |
| retry and delivery | application supplied | Naatre webhook policy remains authoritative |

- **ASYNCAPI-100:** A classification is one of `lossless`,
  `explicitly-adapted`, `application-supplied`, or `unsupported`. Unsupported
  required semantics reject import. Explicitly adapted and application-supplied
  rows require a non-empty resolution. Metadata MUST NOT imply compatibility
  merely because an AsyncAPI keyword has the same name as a Naatre concept.
- **ASYNCAPI-101:** Payload schemas use
  `application/vnd.naatre.schema-reference+json;version=1`. The default export
  contains only an opaque authorized type identity, schema revision, and digest;
  it does not expose field names or protected schema metadata. A complete JSON
  Schema projection belongs to the extracted implementation issue #102.

## Channels, messages, and transports

- **ASYNCAPI-200:** Naatre subscriptions, SSE delivery, WebSocket delivery, and
  webhook HTTP delivery are distinct declarations. Each channel records its
  ordering scope, replay mechanism, terminal behavior, and error envelope.
  `asyncapiWireCompatibility` MUST be `false`; the description does not claim
  that a Naatre logical frame is an AsyncAPI-defined wire frame.
- **ASYNCAPI-201:** A binding is exported only when its concrete implementation
  and test evidence are named. `sse`, `websocket`, `webhook-http`, and future
  `worker` transports share this rule. A channel address without an
  evidence-backed binding is descriptive text, not runtime support, and is
  rejected by the reference exporter.
- **ASYNCAPI-202:** Message content type and event-envelope revision are
  independent. Webhook CloudEvents keep stable application event IDs across
  retries. Delivery attempts, stream sequences, and request identifiers MUST
  NOT become substitute application event IDs.

## Correlation and trust

- **ASYNCAPI-300:** Correlation locations are local AsyncAPI runtime expressions
  under `$message.header#/` or `$message.payload#/`. Each declaration records a
  distinct lifetime and trust classification. Naatre request ID has request
  lifetime, stream ID has logical-stream lifetime, application event ID has
  business-event lifetime, and webhook delivery attempt has delivery-attempt
  lifetime.
- **ASYNCAPI-301:** Correlation metadata is not authorization evidence. A
  transport-supplied request ID is untrusted, a server-issued stream ID is
  scoped to that stream, an application event ID is trusted only after event
  authentication, and a webhook delivery identity is trusted only after the
  configured signature and replay checks.

## Untrusted import and privacy

- **ASYNCAPI-400:** Import accepts bounded local JSON only. The default limits
  cover bytes, nesting depth, object count, channels, messages, references, and
  example bytes. Duplicate JSON names, trailing values, unknown profile fields,
  invalid local references, excessive references, and cycles are rejected.
- **ASYNCAPI-401:** Only local `#/...` references in the documented subset are
  accepted. HTTP, HTTPS, file, data, relative-file, and other external
  references are rejected. The core API has no resolver or network client.
- **ASYNCAPI-402:** Server URLs are inert metadata and require an exact
  caller-provided allowlist. The default allowlist is empty. URLs containing
  user information, queries, fragments, non-HTTPS/non-WSS schemes, or a value
  not exactly allowed are rejected. Accepting metadata never authorizes egress.
- **ASYNCAPI-403:** Export redacts credential values in examples with the shared
  tooling redactor. Protected messages and their payload type identities are
  omitted by default. Diagnostics contain stable codes and safe generic
  messages, never source fragments, URLs, examples, credentials, hidden names,
  or handler errors.

## Compatibility and tooling

- **ASYNCAPI-500:** Compatibility compares stable IDs. Removing a message,
  changing its payload/content type, moving a channel address, changing channel
  semantics, or changing a binding contract is breaking. Correlation and
  security changes are dangerous. Evidence and example-only changes are
  behavior-only. Additions are additive. The strongest classification uses the
  same ordering as Naatre schema compatibility.
- **ASYNCAPI-501:** CLI validation, canonical export, compatibility diff,
  documentation inspection, deterministic mocks, and playground inspection
  consume the same imported model. Those APIs accept no registry, executor,
  resolver, handler, credential provider, or transport. Mock output is fixture
  evidence and MUST NOT claim business or wire compatibility.

Valid: two equivalent schema/capability inputs produce identical bytes; a
playground view and CLI mock report the same model digest; a webhook message
retains separate event and delivery-attempt correlations.

Invalid: importing a remote `$ref`, accepting a server without an exact
allowlist, exporting an untested channel binding, treating a request ID as an
event ID, using an AsyncAPI security scheme as authorization, or printing a
protected payload type in a diagnostic.
