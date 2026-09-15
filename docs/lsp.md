# LSP and editor integrations

`tooling.lsp-1` is the editor transport profile owned by issue #94. The
normative developer-tooling behavior remains owned by issue #55 in
[`spec/v1/tooling.md`](../spec/v1/tooling.md) and
[`tooling.EditorAdapter`](../tooling/editor.go). The LSP package translates
LSP 3.17 requests to that adapter; it does not redefine the Naatre language,
schema, validation, diagnostic, completion, hover, navigation, or rename
model.

## Packages and lifecycle ownership

- `tooling/lsp` owns bounded LSP 3.17 JSON-RPC framing over process stdio,
  UTF-16 position conversion, document synchronization, cancellation, public
  failure mapping, and LSP result translation.
- `cmd/naatre-lsp` owns command-line schema loading and process lifetime. It
  reads one bounded local schema, parses it with `schema.ParseDocument`, and
  constructs one server. Schema URI metadata is descriptive and is never
  fetched.
- `tooling.EditorAdapter` remains the only semantic implementation used by the
  CLI and editors. Schema and protocol packages remain the only contract
  authorities.
- `editors/vscode` owns the dependency-free VS Code process client and provider
  wiring. The editor owns activation, configuration, document events,
  diagnostic collections, child-process termination, and user presentation.

Start a server with one trusted local schema:

```sh
GOWORK=off go run ./cmd/naatre-lsp --schema schema.naatre.json
```

The server sends no network requests and opens no listener. Initialization
returns the exact schema revision. Each opened document retains that revision;
an optional `schemaRevision` echo on open and change is rejected when it differs.
Document versions must increase. A schema change requires closing documents and
restarting the server, so one incremental edit stream cannot combine revisions.
On shutdown, the client sends `shutdown`, then `exit`, closes stdin, disposes
diagnostics, and terminates the child if it remains active.

## Supported runtime profile

The server supports Go 1.27 or newer using only the standard library plus the
repository's portable schema and tooling packages. The transport is LSP 3.17
`Content-Length` framing over stdin/stdout with UTF-16 positions. It advertises
incremental open/change/close synchronization, publish and pull document
diagnostics, completion, plaintext hover, definition navigation, prepare
rename, workspace-edit rename, deprecated diagnostics, and request
cancellation.

The reference editor client is the desktop VS Code extension manifest in
`editors/vscode`, declared for VS Code 1.105 or newer. Its shared framed client
passed the protocol fixture under Node 26.8.2 on macOS 26.6.2 arm64; that
execution proves the stdio client/server profile, not the VS Code host UI or any
other operating system. The profile intentionally publishes no operating-system
certification.

Default finite limits are 4 MiB per framed message, 1 MiB per open document, 64
open documents, 256 changes per update, and 16 concurrent requests. Limit,
cancellation, stale-version, and schema-revision failures use stable public
codes. Diagnostics retain the core engine's stable code and RFC 6901-derived
source range, but expose only a bounded generic message. They never echo source
text, literal arguments, schema descriptions, credentials, filesystem paths,
Go errors, package names, or protected metadata. Schema bytes are parsed as
data; the server never evaluates them or activates plugins, callbacks, scripts,
URI references, or application handlers.

## Unsupported optional capabilities

The profile is closed: every LSP capability not advertised by `initialize` is
unsupported. For LSP 3.17 this includes alternate position encodings; workspace
diagnostics, diagnostic related documents and information, code descriptions,
data, and refresh; completion resolve, trigger characters, snippets,
insert/replace edits, item defaults, commit characters, commands, additional
edits, documentation, and label details; Markdown or trusted-command hovers;
definition links; declaration, type-definition, implementation, references,
document-highlight, document-symbol, and workspace-symbol navigation;
signature help; code actions and code lenses; document links and colors;
document, range, and on-type formatting; folding and selection ranges; call and
type hierarchies; semantic tokens; linked editing; monikers; inline values,
hints, and completions; document drop and paste edits; execute-command; will-save,
will-save-wait-until, and save notifications; dynamic registration; workspace
folders; watched-file registration and workspace file operations; notebook
synchronization; change annotations; and progress reporting.

The integration also does not support schema hot reload, remote schema fetch,
plugin execution, TCP, Unix-socket, WebSocket or browser transports,
multi-process indexes, native editor hosts, background schema discovery, or
operating-system certification. Markdown/trusted-command hover content,
credential transport, application execution, and framework-specific integration
are outside the profile. These exclusions are also machine-readable in
[`conformance/v1/lsp.json`](../conformance/v1/lsp.json).

## Reproducible conformance

The fixture pins issue #55 revision
`16aeb4606062207c0fccabb03a348d49e136e5c6` and the SHA-256 digest of every
dependency and client fixture. Run:

```sh
GOWORK=off go test ./tooling/lsp ./cmd/naatre-lsp -count=1
GOWORK=off go test ./internal/conformance ./internal/conformancerunner -run 'LSP|Lsp' -count=1
GOWORK=off go build -o /tmp/naatre-lsp-conformance ./cmd/naatre-lsp
node conformance/independent/verify-lsp.mjs /tmp/naatre-lsp-conformance
```

The final command uses the same `editors/vscode/client.mjs` transport as the
extension and prints one JSON result. The checked executed report is
[`conformance/reports/lsp-vscode-node-26.8.2-macos-arm64.json`](../conformance/reports/lsp-vscode-node-26.8.2-macos-arm64.json).
