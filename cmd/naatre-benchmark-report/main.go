package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/valksor/naatre/internal/qualityharness"
)

func main() {
	var environment qualityharness.BenchmarkEnvironment
	fixture := flag.String("fixture", "conformance/v1/benchmarks.json", "authoritative benchmark fixture")
	flag.StringVar(&environment.CPU, "cpu", "", "CPU model")
	flag.Uint64Var(&environment.MemoryBytes, "memory-bytes", 0, "physical memory in bytes")
	flag.StringVar(&environment.ImplementationVersion, "implementation-version", "", "implementation version")
	flag.StringVar(&environment.FixtureVersion, "fixture-version", "", "fixture suite version")
	flag.StringVar(&environment.Revision, "revision", "", "exact source revision")
	flag.BoolVar(&environment.Dirty, "dirty", false, "whether the source tree is dirty")
	flag.Parse()
	environment.OS = runtime.GOOS
	environment.Architecture = runtime.GOARCH
	environment.LogicalCPUs = runtime.NumCPU()
	environment.GoVersion = runtime.Version()

	report, err := qualityharness.BuildBenchmarkReport(os.Stdin, *fixture, environment)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "{\"profile\":\"suite.benchmarks-1\",\"status\":\"failed\",\"code\":%q}\n", qualityharness.Code(err))
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(report) != nil {
		os.Exit(1)
	}
}
