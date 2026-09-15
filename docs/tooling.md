# Developer tooling workflow

The unified reference command is `go run ./cmd/naatre`. It operates on local,
versioned artifacts and does not discover schemas or plugins from the network.
The portable contract is [Developer tooling](../spec/v1/tooling.md), and the
machine fixtures are [tooling.workflow-1](../conformance/v1/tooling.json).

## Reproducible CI workflow

```sh
go run ./cmd/naatre validate --document operations.naatre.json --schema schema.naatre.json --operation GetAccount
go run ./cmd/naatre generate --language go --model conformance/v1/generator-model.json --reference conformance/v1/generator-output.json --out generated
go run ./cmd/naatre manifest --document operations.naatre.json > operations.manifest.json
go run ./cmd/naatre compatibility --schema schema.naatre.json --manifest operations.manifest.json
```

This sequence validates an operation, reproduces the pinned Go client, creates
an operation manifest, and evaluates every registered operation against the
proposed schema. `generator-model.json` pins `naatre.generator-model-1`,
protocol `1`, and `c14n-1`; `generator-output.json` pins the reference generator
version and every operation digest. The compatibility report binds the accepted
schema digest and exits with status 1 when any manifest operation no longer
plans.

Other commands are:

```text
naatre format --document FILE
naatre canonicalize --document FILE
naatre hash --document FILE
naatre schema export --schema FILE
naatre schema diff --before FILE --after FILE
naatre schema jtd export --schema FILE --root TYPE --report FILE
naatre schema jtd validate --jtd FILE --approve-embedded-identities
naatre schema jtd import --jtd FILE --approve-embedded-identities
naatre schema jtd diff --before FILE --after FILE --approve-embedded-identities
naatre generate --language reference --model FILE --out FILE
naatre explain --schema FILE --document FILE --operation NAME
naatre mock --schema FILE --document FILE --operation NAME --seed N --scenario success|null|missing|failure|unknown-variant
naatre conformance --fixture conformance/v1/tooling.json
```

All diagnostic commands emit `naatre.tooling.diagnostics-1`. Exit statuses are
0 success, 1 diagnostic or incompatibility, 2 usage, 3 input/output, and 4
internal failure.

The JTD commands use the same RFC 8927 mapper for generation, validation,
import, and diffing. See the [JTD projection workflow](jtd.md) for explicit
identity assignment when importing third-party JTD and for the strict fidelity
boundary.

## Offline configuration and trust

Configuration precedence is flags, environment, project configuration, then
built-in defaults. Empty values do not erase a lower-precedence pin. Offline
schema bundles are validated and canonicalized before use; URI references stay
descriptive and are never fetched.

Generator plugins are trusted executable code. The core does not search the
current directory or `PATH`. An adapter that enables a plugin requires an
absolute path, explicit trust, stable plugin ID, algorithm version, exact
lowercase SHA-256 digest, and declared input/output boundaries.

## Editor, explain, mocks, and playground safety

`tooling.EditorAdapter` is the shared protocol-neutral implementation for
diagnostics, completion, hover, definition, rename assistance, and deprecation
metadata. The LSP transport and packaged integrations belong to issue #94.

Explain and schema-driven mock generation use metadata-only planning snapshots.
They cannot call application handlers. Explain redacts literals and policy
identities. Mocks are deterministic fixture generators with explicit null,
missing, failure, pagination, streaming, and unknown-variant states; successful
mock output never proves application authorization or business behavior.

The opt-in playground and local mock server belong to issue #91 and are
implemented by the [`playground` package](playground.md). Every history, URL,
log, and snippet persistence/export boundary calls `tooling.RedactAndBound`;
unredacted reveal is not implemented and is never saved.
