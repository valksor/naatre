#!/usr/bin/env bash

set -Eeuo pipefail

retain_fuzz_seeds() {
  find . -type f -path '*/testdata/fuzz/*' -print -exec base64 {} \;
}

run_benchmarks() {
  local quality_cpu quality_hardware quality_memory_kib quality_memory_bytes quality_revision quality_dirty quality_version
  case "$(uname -s)" in
    Linux)
      quality_cpu="$(awk -F ': ' '/model name/{print $2; exit}' /proc/cpuinfo)"
      quality_memory_kib="$(awk '/MemTotal/{print $2; exit}' /proc/meminfo)"
      quality_memory_bytes="$((quality_memory_kib * 1024))"
      ;;
    Darwin)
      quality_hardware="$(system_profiler SPHardwareDataType -detailLevel mini)"
      quality_cpu="$(awk -F ': ' '/Chip:|Processor Name:/{print $2; exit}' <<<"$quality_hardware")"
      quality_memory_bytes="$(awk -F ': ' '/Memory:/{split($2, value, " "); if (value[2] == "GB") printf "%.0f\n", value[1] * 1073741824; else if (value[2] == "MB") printf "%.0f\n", value[1] * 1048576; exit}' <<<"$quality_hardware")"
      ;;
    *)
      printf '%s\n' '{"profile":"suite.benchmarks-1","status":"failed","code":"QUALITY_INVALID_CONFIGURATION"}'
      return 1
      ;;
  esac
  quality_revision="$(git rev-parse HEAD)"
  quality_dirty=false
  if [[ -n "$(git status --porcelain)" ]]; then
    quality_dirty=true
  fi
  quality_version="$(go list -m -f '{{if .Version}}{{.Version}}{{else}}devel{{end}}')"

  go test -json -run=^$ -bench=^BenchmarkQualityWorkloads$ -benchmem -benchtime=100ms -count=31 ./internal/qualityharness |
    go run ./cmd/naatre-benchmark-report \
      -cpu "$quality_cpu" \
      -memory-bytes "$quality_memory_bytes" \
      -implementation-version "$quality_version" \
      -fixture-version 1.0.0 \
      -revision "$quality_revision" \
      -dirty="$quality_dirty"
}

trap retain_fuzz_seeds ERR

if [[ "${1:-all}" == benchmark ]]; then
  run_benchmarks
  exit
fi

go test ./protocol -run=^$ -fuzz=FuzzStrictDecoder -fuzztime=5s
go test ./runtime -run=^$ -fuzz=FuzzCheckedResourceAddition -fuzztime=5s
go test ./internal/qualityharness -run=^$ -fuzz=FuzzFaultSchedule -fuzztime=5s
go test ./internal/qualityharness -run=^$ -fuzz=FuzzResourceLifecycle -fuzztime=5s
go test ./internal/qualityharness -run='^Test(Leak|Fault)' -count=20
go test -race ./internal/conformance -run=^TestGoRunnerParallelCancellationProfile$ -count=1
go test -race -count=3 ./...
run_benchmarks
