# Developer tooling

The `tooling.workflow-1` profile defines portable authoring behavior shared by
command-line, editor, CI, playground, and mock integrations. Its fixtures are
[tooling.json](../../conformance/v1/tooling.json). The Go reference engine is
the `tooling` package; optional protocol and user-interface integrations are
separate packages and do not redefine these results.

## Commands and diagnostics

- **TOOL-001:** A conforming CLI exposes `validate`, `format`, `canonicalize`,
  `hash`, `schema export`, `schema diff`, `generate`, `manifest`,
  `compatibility`, `explain`, `mock`, and `conformance`. Exit status 0 means
  success, 1 means a document or compatibility diagnostic, 2 means invalid
  command usage, 3 means bounded input/output failure, and 4 means an internal
  tooling failure. These meanings MUST remain stable within profile version 1.
- **TOOL-002:** Machine-readable diagnostics use
  `naatre.tooling.diagnostics-1` and contain `phase`, stable `code`, RFC 6901
  `path`, safe `message`, and optional line and column. CLI and editor adapters
  MUST call the same validation engine and return identical phase, code, and
  path for the same input, schema bundle, operation, and capability set.
- **TOOL-003:** Inputs are bounded local files or standard input selected by an
  explicit adapter. Parsing a schema reference MUST NOT fetch its URI. A
  diagnostic does not disclose file contents, credentials, environment values,
  plugin arguments, or hidden schema names.

## Formatting, identity, and explain

- **TOOL-100:** Formatting validates the language AST before writing output.
  It MAY reorder object members and whitespace, but MUST preserve operation,
  fragment, selection, stage, branch, argument-position, and directive arrays;
  literal bytes after canonical language decoding; extension semantics; and
  operation identity. Formatting followed by canonicalization and document
  hashing MUST reproduce the original canonical payload and digest.
- **TOOL-101:** Explain is side-effect-free. It resolves the selected
  operation, execution and barrier order, estimated static cost, batch
  opportunities, capabilities, and rejection diagnostics from immutable
  metadata. It MUST NOT invoke a business handler, dynamic authorization
  policy, directive callback, remote service, schema fetch, or plugin.
- **TOOL-102:** Explain output uses `naatre.explain-1`. Literal arguments are
  redacted by default, authorization policy identities are omitted, and an
  adapter MUST replace declarations outside the caller's visibility set rather
  than reveal their names in nodes, rejection reasons, or suggestions.

## Editors and playgrounds

- **TOOL-200:** An editor adapter provides diagnostics, deterministic
  completions, hover documentation, source-based definition locations,
  structural rename edits for fragments/types, and deprecation metadata. LSP
  3.17 is the preferred transport; an adapter MUST preserve the core diagnostic
  envelope when translating to an editor protocol. Issue #94 owns the reference
  LSP transport and packaged editor integrations.
- **TOOL-201:** A local playground is opt-in and disabled until explicitly
  started. It provides schema browsing, variable editing, request/error paths,
  streaming frame inspection, and copyable SDK examples. Endpoint schemes and
  origins MUST be validated before requests. Issue #91 owns the browser UI and
  mock-server integration.
- **TOOL-202:** Playground history, displayed URLs, logs, and exported snippets
  MUST apply the shared credential redactor by default. Authorization headers,
  cookies, URL user information, passwords, API keys, access/refresh tokens,
  and equivalent configured credential names are removed before persistence or
  export. A user may copy unredacted data only through an explicit, unsaved
  reveal action.

## Mocks, bundles, and compatibility

- **TOOL-300:** Schema-driven mocks use an explicit seed and scenario. The same
  schema digest, document digest, operation, seed, and scenario MUST produce
  identical results. Scenarios include success, null, missing, partial failure,
  and unknown open variants; pagination metadata and stream open/next/complete
  frames are explicit fixture data.
- **TOOL-301:** Mock generation, completion, introspection, and explain consume
  only portable descriptors and planning metadata and MUST invoke zero business
  handlers. Mock success is fixture evidence only and MUST NOT be presented as
  evidence that authentication, authorization, provider behavior, or business
  logic works.
- **TOOL-302:** Offline schema bundles pin document version, canonicalization
  version, revision, and digest. Configuration precedence is command flags,
  environment, project file, then built-in defaults. Plugin discovery is never
  implicit: executable plugins require an absolute configured path, explicit
  trust, stable ID, algorithm version, and lowercase SHA-256 pin. A core tooling
  command MUST NOT execute an untrusted or unpinned plugin.
- **TOOL-303:** An operation manifest stores each registered operation's name,
  kind, canonical document, and document digest. CI compatibility checks MUST
  recompute every digest, select the bound operation, and plan every registered
  document against the proposed schema. A global schema diff alone is not
  operation compatibility evidence.
- **TOOL-304:** A reproducible workflow validates a document, generates a
  client from pinned generator artifacts, emits an operation manifest, and
  checks that manifest against a proposed offline schema. Published fixture and
  generator profile versions MUST appear in the commands or their pinned input
  artifacts.

Valid: two editors and the CLI report `UNKNOWN_FIELD`, phase `validate`, and the
same document pointer; a seeded partial-failure mock returns useful fixture data
and one path-specific error while all business-handler counters remain zero.

Invalid: a formatter sorts selections, an explain command prints an argument
token or policy name, a playground stores a bearer token, a mock calls an
application resolver, or CI approves a breaking schema because an unrelated
global diff was empty.
