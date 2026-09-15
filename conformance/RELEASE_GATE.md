# Full release gate

`naatre-release-gate` is the fail-closed aggregation boundary for issue 69. It
does not infer a pass from source presence and it never executes a missing
toolchain on behalf of a release. The release orchestrator executes the matrix,
records `naatre.conformance.report-1` files, and then invokes the aggregator.

The expected matrix is derived from the advertised-but-unevidenced rows in
`v1/compatibility.json`; it is not duplicated in Go source. Print the exact
machine-readable inventory with:

```sh
go run ./cmd/naatre-release-gate --list > release-gate-inventory.json
```

The inventory currently contains 29 official implementation/profile cells,
four distinct Go-gateway-to-worker cells, ten SDK-to-Go paths, the independent
non-Go runner path, and the cross-profile security gate. A client or worker
path never counts as native-runtime evidence.

## Profile report requirements

Every input report must conform to `profile-report.schema.json` and pass:

```sh
node conformance/independent/profiles.mjs --report path/to/report.json
```

The report pins specification, schema, canonicalization, fixture, generator,
runtime, SDK, and transport revisions. A non-applicable revision is explicit
and reasoned; client reports must pin all four implementation revisions and
worker reports must pin runtime and transport revisions. Every report records
OS and architecture. Claims require exact clauses and ordered fixture digests.

Each official client report also records these named run artifacts, calculated
from the observed per-vector output after its verifier succeeds:

- `canonical-vectors`
- `schema-vectors`
- `validation-vectors`
- `generated-filter-sort-vectors`

The artifact bytes are the canonical JSON object mapping every shared vector ID
to its observed canonical bytes, semantic hash, validation outcome, or generated
filter/sort mapping. The aggregator requires one digest per artifact to agree
across all ten languages. Hashing fixture input or expected output instead of
observed output is not release evidence.

Optional capabilities use one of two explicit forms: include the capability in
the passing result and claim, or provide a separate report whose result is
`unsupported` and names that optional capability. Missing optional status,
unsupported required work, invalid skips, failed results, infrastructure
failures, partial clauses/fixtures, duplicate evidence, and any version skew
fail the aggregate.

## Orchestrator execution

Run from a clean checkout at the exact source revision. The following commands
are the complete repository-owned verifier layer. The orchestrator must retain
each command vector and its output in the applicable profile report. Networked
tests run only in the orchestrator environment; listener tests allocate
ephemeral ports.

```sh
go build ./...
go vet ./...
golangci-lint run --allow-parallel-runners ./...
go generate ./...
git diff --exit-code
go test -race ./...

node conformance/independent/profiles.mjs
node conformance/independent/scalars.mjs
node conformance/independent/canonical.mjs
node conformance/independent/validation.mjs
node conformance/independent/verify-collection-query-generation.mjs
node conformance/independent/verify-generator.mjs

node conformance/independent/verify-typescript-sdk.mjs
node conformance/independent/verify-typescript-adapters.mjs
node conformance/independent/verify-php-sdk.mjs
node conformance/independent/verify-php-adapters.mjs
node conformance/independent/verify-php-server.mjs
python3 conformance/independent/verify-python-sdk.py
python3 conformance/independent/verify-python-sdk-adapters.py
node conformance/independent/verify-rust-sdk.mjs
node conformance/independent/verify-rust-async-adapters.mjs
node conformance/independent/verify-jvm-sdk.mjs
node conformance/independent/verify-jvm-adapters.mjs
node conformance/independent/verify-dotnet-sdk.mjs
node conformance/independent/verify-dotnet-adapters.mjs
node conformance/independent/verify-swift-sdk.mjs
node conformance/independent/verify-swift-apple-adapters.mjs
node conformance/independent/verify-dart-sdk.mjs
node conformance/independent/verify-ruby-sdk.mjs
node conformance/independent/verify-ruby-adapters.mjs

node conformance/independent/verify-remote-worker-gateway.mjs
node conformance/independent/verify-php-worker-adapters.mjs
python3 conformance/independent/verify-python-worker.py
python3 conformance/independent/verify-python-worker-adapters.py
node conformance/independent/verify-typescript-worker.mjs
node conformance/independent/verify-rust-worker.mjs
node conformance/independent/verify-rust-tokio-axum.mjs
```

Record the independent, non-Go suite-consumption receipt and the Go security
gate receipt exactly as follows. `--require-pass` prevents a syntactically valid
failed runner response from passing the shell step.

```sh
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"release-independent","command":"run","path":{"source":{"kind":"codec","language":"javascript-typescript"},"destination":{"kind":"codec","language":"javascript-typescript"}},"profiles":["suite.contract-1"]}' | node conformance/independent/runner.mjs --require-pass > release-evidence/independent-runner.json

printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"release-security","command":"run","path":{"source":{"kind":"gateway","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["suite.security-gate-1"]}' | go run ./cmd/naatre-conformance --require-pass > release-evidence/security-gate.json
```

The full listener-backed runner and HTTP/streaming paths are exercised by
`go test -race ./...`. The independent runner binding itself uses
`127.0.0.1:0`, as asserted by `TestIndependentRunnerHTTPMediaType`; no release
test may claim or bind a fixed port.

After the orchestrator has emitted one complete profile report per inventory
cell, validate every report and aggregate it into the release directory:

```sh
find release-evidence/profile-reports -name '*.json' -type f -exec node conformance/independent/profiles.mjs --report '{}' \;

go run ./cmd/naatre-release-gate \
  --reports release-evidence/profile-reports \
  --independent-report release-evidence/independent-runner.json \
  --security-report release-evidence/security-gate.json \
  --source-revision SOURCE_REVISION \
  --output releases/RELEASE_ID/release-gate.json \
  --manifest-template releases/RELEASE_ID/manifest.template.json \
  --manifest-output releases/RELEASE_ID/manifest.json
```

Replace `SOURCE_REVISION` and `RELEASE_ID` with the exact immutable values; they
are placeholders, not shell variables. The command always writes a diagnostic
aggregate. It writes the linked release manifest only when the aggregate passes.
The linked claim is `release.stable-1` and contains the exact aggregate path and
SHA-256 digest. Existing planned or experimental support rows stay unevidenced
and are never promoted by the gate.
