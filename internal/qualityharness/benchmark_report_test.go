package qualityharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func TestBuildBenchmarkReportPublishesContractFields(t *testing.T) {
	input := benchmarkEvents(t, 30, true)
	report, err := BuildBenchmarkReport(input, benchmarkFixturePath(), benchmarkEnvironment())
	if err != nil {
		t.Fatalf("BuildBenchmarkReport: %v", err)
	}
	if report.Status != "passed" || report.Profile != "suite.benchmarks-1" || len(report.Benchmarks) != 10 {
		t.Fatalf("report identity = %#v", report)
	}
	for _, benchmark := range report.Benchmarks {
		if len(benchmark.RawSamples) != 30 || benchmark.Metrics.P50 == 0 || benchmark.Metrics.P95 == 0 || benchmark.Metrics.P99 == 0 {
			t.Fatalf("benchmark result = %#v", benchmark)
		}
	}
}

func TestBuildBenchmarkReportRejectsLimitsAndMetrics(t *testing.T) {
	_, err := BuildBenchmarkReport(benchmarkEvents(t, 29, true), benchmarkFixturePath(), benchmarkEnvironment())
	if Code(err) != CodeBudgetExceeded {
		t.Fatalf("short sample code = %q", Code(err))
	}
	_, err = BuildBenchmarkReport(benchmarkEvents(t, 30, false), benchmarkFixturePath(), benchmarkEnvironment())
	if Code(err) != CodeInvalidConfiguration {
		t.Fatalf("missing metric code = %q", Code(err))
	}
	if _, _, ok := parseBenchmarkLine("BenchmarkQualityWorkloads/parse-small-valid-12 100 20 ns/op 2 widgets/op 1 other/op"); ok {
		t.Fatal("benchmark line without allocation metrics was accepted")
	}
}

func benchmarkEvents(t *testing.T, count int, upstream bool) *bytes.Reader {
	t.Helper()
	names := []string{
		"parse-small-valid", "parse-adversarial-depth", "plan-cold-fragments", "plan-warm-fragments",
		"execute-ordered-read", "expand-collection-256", "batch-duplicates-256", "canonicalize-schema",
		"stream-frames-1024", "stream-replay-live-handoff",
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for sample := 1; sample <= count; sample++ {
		for _, name := range names {
			metric := benchmarkUpstreamMetric(name, upstream)
			line := fmt.Sprintf("BenchmarkQualityWorkloads/%s-12 100 %d ns/op 32 B/op 2 allocs/op%s\n", name, sample, metric)
			for _, part := range benchmarkEventParts(line, sample == 1 && name == "plan-cold-fragments") {
				if err := encoder.Encode(map[string]string{"Action": "output", "Output": part}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return bytes.NewReader(output.Bytes())
}

func benchmarkUpstreamMetric(name string, enabled bool) string {
	if !enabled {
		return ""
	}
	expected, ok := map[string]int{"batch-duplicates-256": 1, "stream-replay-live-handoff": 2}[name]
	if !ok {
		return ""
	}
	return fmt.Sprintf(" %d upstreamCalls", expected)
}

func benchmarkEventParts(line string, split bool) []string {
	if !split {
		return []string{line}
	}
	middle := len(line) / 2
	return []string{line[:middle], line[middle:]}
}

func benchmarkFixturePath() string {
	return filepath.Join("..", "..", "conformance", "v1", "benchmarks.json")
}

func benchmarkEnvironment() BenchmarkEnvironment {
	return BenchmarkEnvironment{
		OS: "linux", Architecture: "amd64", CPU: "test cpu", LogicalCPUs: 4, MemoryBytes: 1024,
		GoVersion: "go1.27.1", ImplementationVersion: "test", FixtureVersion: "1.0.0", Revision: "abc123",
	}
}
