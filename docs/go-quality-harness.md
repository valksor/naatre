# Go quality harness

Issue #78 owns the Go execution surface for the language-neutral adversarial
and benchmark contracts in `conformance/v1/adversarial.json` and
`conformance/v1/benchmarks.json`. Those fixtures remain authoritative. The
`quality.go.harness-1` profile and `internal/qualityharness` package only bind
Go test mechanisms to them; they do not define protocol or schema behavior.

## Ownership and lifecycle

The internal package provides deterministic fault schedules and request-local
accounting for goroutines, response bodies, and streams. A test acquires every
owned resource, releases it exactly once, cancels cooperative goroutines, waits
within its case budget, and calls `Tracker.Check` before completing. Leaks and
injected failures use the stable `QUALITY_*` codes listed in
`conformance/v1/go-quality.json`. Their public error text contains no fault
point, cause, stack, credential, protected metadata, or implementation detail.

Go's fuzz engine retains a failing input under the target's
`testdata/fuzz/<target>` seed corpus. CI prints every retained seed as base64 on
failure so the run log remains a reproducible corpus source. Adversarial tests
use finite fuzz durations and existing decode, planning, execution, and stream
limits. Repeated race runs use fresh package processes. Benchmarks publish raw
Go JSON events and allocation metrics for 30 samples; do not infer N+1 behavior
from elapsed time.

## Reproducible commands

Run from the repository root at the revision under test:

```sh
go test ./protocol -run=^$ -fuzz=FuzzStrictDecoder -fuzztime=30s
go test ./runtime -run=^$ -fuzz=FuzzCheckedResourceAddition -fuzztime=30s
go test ./internal/qualityharness -run=^$ -fuzz=FuzzFaultSchedule -fuzztime=30s
go test ./internal/qualityharness -run=^$ -fuzz=FuzzResourceLifecycle -fuzztime=30s
go test -race -count=3 ./...
go test ./internal/qualityharness -run=^TestLeak -count=20
go test ./internal/qualityharness -run=^TestFault -count=20
bash conformance/run-go-quality-ci.sh benchmark
```

The benchmark driver collects 31 attempts and rejects the report unless every
normative workload has at least 30 valid raw samples. It pipes Go JSON events
through `cmd/naatre-benchmark-report`, which publishes one JSON object with the
OS, architecture, CPU, logical CPU count, memory bytes, Go version,
implementation version, fixture version, exact git revision, dirty state,
`ns/op`, `B/op`, `allocs/op`, p50, p95, p99, raw samples, and required
upstream-call metrics. `conformance/v1/benchmarks.json` is the source of those
required fields and workloads. The checked-in driver discovers hardware on
Linux and macOS; Windows benchmark publication requires invoking the reporter
with explicitly collected `-cpu` and `-memory-bytes` values.

Produce machine-readable conformance evidence, including SHA-256 identities
for `go.mod`, `go.sum`, the authoritative fixtures, and every harness source:

```sh
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"go-quality","command":"run","path":{"source":{"kind":"sdk","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["quality.go.harness-1"]}' | go run ./cmd/naatre-conformance --require-pass
```

## Runtime and profile boundary

The minimum and release toolchain is Go 1.27. The supported execution matrix is
Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. The profile covers only
Go fuzzing, race execution, owned-resource leak detection, deterministic fault
injection, and Go benchmark publication.

Unsupported optional capabilities are: non-Go harness execution; native
runtime, framework, browser, HTTP, WebSocket, or worker certification;
distributed federation transport benchmarking; performance pass/fail
thresholds; automatic Windows benchmark-environment discovery; and platforms
outside the listed matrix. These remain explicitly unsupported rather than
being omitted or counted as passes.
