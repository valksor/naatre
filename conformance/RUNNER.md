# Conformance runner protocol

`naatre.conformance.runner-1` is a language-neutral request/report protocol. A
runner consumes the checked-in suite directly; it does not import the Go
implementation or generate expected values from the implementation under test.
The JSON contract is [`runner-protocol.schema.json`](runner-protocol.schema.json).

## Bindings

The standard-input binding is UTF-8 NDJSON. Each line is one complete request
and produces exactly one response line on standard output, in input order.
Diagnostics and logs go to standard error. A malformed line still produces one
protocol response when its `id` can be recovered safely. Inputs are bounded to
1 MiB per line.

The HTTP binding uses `GET /v1/conformance/profiles` for discovery and
`POST /v1/conformance/run` for a request object. Request and response media type
is `application/json; charset=utf-8`; missing, malformed, or different request
media types receive HTTP 415, and request bodies are bounded to 1 MiB. HTTP
400 represents a malformed runner request, while fixture failures remain HTTP
200 with a `failed` or `error` result so a report is never confused with
transport failure.

`path.source` and `path.destination` distinguish SDK-to-native-runtime,
SDK-to-Go gateway, and SDK-to-worker/gateway evidence. A runner reports
`unsupported` for every requested profile it cannot execute. An unsupported
profile is never omitted and never counted as passed.

Every result carries the complete portable result shape: phase, public code,
source location, response path, data, negotiated capabilities, diagnostics,
and fixture digests. Empty values are explicit. Reports bind the independently
versioned fixture suite, runner version, language, and platform.

Runner discovery and execution responses are named-run evidence, not profile
certificates. Publishable certification reports use
`naatre.conformance.report-1`, the closed
[`profile-report.schema.json`](profile-report.schema.json) shape, and the exact
coverage rules in [PROFILES.md](PROFILES.md). Runner internal errors use
`infrastructure-failure`; `invalid-skip` is reserved for invalid required-
fixture skips, while `unsupported` discovery remains non-certifying.

## Reference independent runner

The dependency-free JavaScript runner supports NDJSON and HTTP:

```sh
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"profiles","command":"discover"}' | node conformance/independent/runner.mjs
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"ci","command":"run","path":{"source":{"kind":"codec","language":"javascript-typescript"},"destination":{"kind":"codec","language":"javascript-typescript"}},"profiles":["suite.contract-1"]}' | node conformance/independent/runner.mjs --require-pass
node conformance/independent/runner.mjs --http=127.0.0.1:8080
```

It executes the suite-integrity profile, the independent scalar and
canonicalization profiles, the JavaScript/TypeScript core and adapter SDK
profiles, the Python core and adapter profiles, and the runtime-neutral Rust
SDK core profile through its checked Cargo conformance target. The standalone
reliability script checks the portable reliability fixture contract but does
not advertise execution of runtime reliability semantics. All other profiles
are discovered and reported explicitly as unsupported. The final advertised-
language and cross-profile execution matrix belongs to issues #69 and #70.

The Go reference binding is available as `go run ./cmd/naatre-conformance` and
supports the same NDJSON and `--http=address` forms. Go implementations register
context-aware profile handlers with `internal/conformancerunner`; the race suite
executes a real 32-branch runtime plan through that runner and proves bounded
parallelism, cancellation propagation, and complete branch joining.
The Go runner also executes `runtime.go.plan-cache-1` directly and publishes
the exact fixture, core-planning, module, source, test, and documentation
digests used as evidence.

Both NDJSON runners accept `--require-pass`, emit the complete report, and then
exit non-zero when any requested profile is failed, errored, or unsupported.
CI uses this mode so a syntactically valid failing report cannot pass a pipeline.
