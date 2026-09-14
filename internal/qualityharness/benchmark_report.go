package qualityharness

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// BenchmarkEnvironment identifies the exact environment that produced a
// benchmark report.
type BenchmarkEnvironment struct {
	OS                    string `json:"os"`
	Architecture          string `json:"architecture"`
	CPU                   string `json:"cpu"`
	LogicalCPUs           int    `json:"logicalCpus"`
	MemoryBytes           uint64 `json:"memoryBytes"`
	GoVersion             string `json:"goVersion"`
	ImplementationVersion string `json:"implementationVersion"`
	FixtureVersion        string `json:"fixtureVersion"`
	Revision              string `json:"revision"`
	Dirty                 bool   `json:"dirty"`
}

// BenchmarkReport is the machine-readable publication required by the
// language-neutral benchmark fixture.
type BenchmarkReport struct {
	Profile     string               `json:"profile"`
	Status      string               `json:"status"`
	Environment BenchmarkEnvironment `json:"environment"`
	Fixture     BenchmarkFixture     `json:"fixture"`
	Benchmarks  []BenchmarkResult    `json:"benchmarks"`
}

type BenchmarkFixture struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type BenchmarkResult struct {
	Name       string            `json:"name"`
	Metrics    BenchmarkMetrics  `json:"metrics"`
	RawSamples []BenchmarkSample `json:"rawSamples"`
}

type BenchmarkMetrics struct {
	NanosecondsPerOp float64  `json:"ns/op"`
	BytesPerOp       float64  `json:"B/op"`
	AllocsPerOp      float64  `json:"allocs/op"`
	P50              float64  `json:"p50"`
	P95              float64  `json:"p95"`
	P99              float64  `json:"p99"`
	UpstreamCalls    *float64 `json:"upstreamCalls,omitempty"`
}

type BenchmarkSample struct {
	Iterations       uint64   `json:"iterations"`
	NanosecondsPerOp float64  `json:"ns/op"`
	BytesPerOp       float64  `json:"B/op"`
	AllocsPerOp      float64  `json:"allocs/op"`
	UpstreamCalls    *float64 `json:"upstreamCalls,omitempty"`
}

type benchmarkContract struct {
	Profile string `json:"profile"`
	Rules   struct {
		MinimumSamples int `json:"minimumSamples"`
	} `json:"rules"`
	Workloads []struct {
		Name            string             `json:"name"`
		RequiredMetrics map[string]float64 `json:"requiredMetrics"`
	} `json:"workloads"`
}

// BuildBenchmarkReport parses Go benchmark JSON events and validates them
// against the authoritative benchmark fixture before publishing a report.
func BuildBenchmarkReport(input io.Reader, fixturePath string, environment BenchmarkEnvironment) (BenchmarkReport, error) {
	contract, fixture, err := loadBenchmarkContract(fixturePath)
	if err != nil || !validBenchmarkEnvironment(environment) {
		return BenchmarkReport{}, &Failure{Code: CodeInvalidConfiguration}
	}
	samples, err := parseBenchmarkSamples(input, contract)
	if err != nil {
		return BenchmarkReport{}, err
	}
	results, err := summarizeBenchmarks(contract, samples)
	if err != nil {
		return BenchmarkReport{}, err
	}
	return BenchmarkReport{
		Profile: contract.Profile, Status: "passed", Environment: environment,
		Fixture: fixture, Benchmarks: results,
	}, nil
}

func loadBenchmarkContract(path string) (benchmarkContract, BenchmarkFixture, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return benchmarkContract{}, BenchmarkFixture{}, err
	}
	var contract benchmarkContract
	if json.Unmarshal(content, &contract) != nil || contract.Profile != "suite.benchmarks-1" ||
		contract.Rules.MinimumSamples < 1 || len(contract.Workloads) == 0 {
		return benchmarkContract{}, BenchmarkFixture{}, &Failure{Code: CodeInvalidConfiguration}
	}
	digest := sha256.Sum256(content)
	return contract, BenchmarkFixture{Path: path, SHA256: hex.EncodeToString(digest[:])}, nil
}

func validBenchmarkEnvironment(environment BenchmarkEnvironment) bool {
	return environment.OS != "" && environment.Architecture != "" && environment.CPU != "" &&
		environment.LogicalCPUs > 0 && environment.MemoryBytes > 0 && environment.GoVersion != "" &&
		environment.ImplementationVersion != "" && environment.FixtureVersion != "" && environment.Revision != ""
}

func parseBenchmarkSamples(input io.Reader, contract benchmarkContract) (map[string][]BenchmarkSample, error) {
	wanted := make(map[string]struct{}, len(contract.Workloads))
	for _, workload := range contract.Workloads {
		wanted[workload.Name] = struct{}{}
	}
	samples := make(map[string][]BenchmarkSample, len(wanted))
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	pending := ""
	for scanner.Scan() {
		var event struct {
			Output string `json:"Output"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return nil, &Failure{Code: CodeInvalidConfiguration}
		}
		pending += event.Output
		for {
			end := strings.IndexByte(pending, '\n')
			if end < 0 {
				break
			}
			recordBenchmarkSample(pending[:end], wanted, samples)
			pending = pending[end+1:]
		}
	}
	if scanner.Err() != nil {
		return nil, &Failure{Code: CodeInvalidConfiguration}
	}
	recordBenchmarkSample(pending, wanted, samples)
	return samples, nil
}

func recordBenchmarkSample(line string, wanted map[string]struct{}, samples map[string][]BenchmarkSample) {
	name, sample, ok := parseBenchmarkLine(line)
	if _, required := wanted[name]; ok && required {
		samples[name] = append(samples[name], sample)
	}
}

func parseBenchmarkLine(output string) (string, BenchmarkSample, bool) {
	fields := strings.Fields(output)
	if len(fields) < 8 || !strings.HasPrefix(fields[0], "BenchmarkQualityWorkloads/") {
		return "", BenchmarkSample{}, false
	}
	name := strings.TrimPrefix(fields[0], "BenchmarkQualityWorkloads/")
	separator := strings.LastIndexByte(name, '-')
	if separator < 1 {
		return "", BenchmarkSample{}, false
	}
	name = name[:separator]
	iterations, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return "", BenchmarkSample{}, false
	}
	metrics := make(map[string]float64, (len(fields)-2)/2)
	for index := 2; index+1 < len(fields); index += 2 {
		value, parseErr := strconv.ParseFloat(fields[index], 64)
		if parseErr != nil {
			return "", BenchmarkSample{}, false
		}
		metrics[fields[index+1]] = value
	}
	nanoseconds, hasNanoseconds := metrics["ns/op"]
	bytesPerOp, hasBytes := metrics["B/op"]
	allocsPerOp, hasAllocs := metrics["allocs/op"]
	if !hasNanoseconds || !hasBytes || !hasAllocs || nanoseconds <= 0 {
		return "", BenchmarkSample{}, false
	}
	sample := BenchmarkSample{
		Iterations: iterations, NanosecondsPerOp: nanoseconds,
		BytesPerOp: bytesPerOp, AllocsPerOp: allocsPerOp,
	}
	if upstream, ok := metrics["upstreamCalls"]; ok {
		sample.UpstreamCalls = &upstream
	}
	return name, sample, true
}

func summarizeBenchmarks(contract benchmarkContract, samples map[string][]BenchmarkSample) ([]BenchmarkResult, error) {
	results := make([]BenchmarkResult, 0, len(contract.Workloads))
	for _, workload := range contract.Workloads {
		workloadSamples := samples[workload.Name]
		if len(workloadSamples) < contract.Rules.MinimumSamples {
			return nil, &Failure{Code: CodeBudgetExceeded}
		}
		if !requiredMetricsMatch(workloadSamples, workload.RequiredMetrics) {
			return nil, &Failure{Code: CodeInvalidConfiguration}
		}
		results = append(results, summarizeBenchmark(workload.Name, workloadSamples))
	}
	return results, nil
}

func requiredMetricsMatch(samples []BenchmarkSample, required map[string]float64) bool {
	expected, requiredUpstream := required["upstreamCalls"]
	for _, sample := range samples {
		if sample.BytesPerOp < 0 || sample.AllocsPerOp < 0 {
			return false
		}
		if requiredUpstream && (sample.UpstreamCalls == nil || *sample.UpstreamCalls != expected) {
			return false
		}
	}
	return true
}

func summarizeBenchmark(name string, samples []BenchmarkSample) BenchmarkResult {
	nanoseconds := make([]float64, len(samples))
	bytesPerOp := make([]float64, len(samples))
	allocsPerOp := make([]float64, len(samples))
	for index, sample := range samples {
		nanoseconds[index] = sample.NanosecondsPerOp
		bytesPerOp[index] = sample.BytesPerOp
		allocsPerOp[index] = sample.AllocsPerOp
	}
	metrics := BenchmarkMetrics{
		NanosecondsPerOp: quantile(nanoseconds, 0.50), BytesPerOp: quantile(bytesPerOp, 0.50),
		AllocsPerOp: quantile(allocsPerOp, 0.50), P50: quantile(nanoseconds, 0.50),
		P95: quantile(nanoseconds, 0.95), P99: quantile(nanoseconds, 0.99),
	}
	if samples[0].UpstreamCalls != nil {
		metrics.UpstreamCalls = samples[0].UpstreamCalls
	}
	return BenchmarkResult{Name: name, Metrics: metrics, RawSamples: samples}
}

func quantile(values []float64, percentile float64) float64 {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	index := int(float64(len(ordered)-1)*percentile + 0.5)
	return ordered[index]
}
