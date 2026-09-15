package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valksor/naatre/internal/releasegate"
)

func main() {
	root := flag.String("root", ".", "repository root")
	fixture := flag.String("fixture", "conformance/v1/release-gate.json", "release-gate fixture")
	reportsDirectory := flag.String("reports", "", "directory containing profile-report-1 JSON files")
	independent := flag.String("independent-report", "", "independent non-Go runner response")
	security := flag.String("security-report", "", "Go security-gate runner response")
	sourceRevision := flag.String("source-revision", "", "exact source revision under test")
	output := flag.String("output", "", "aggregate release-gate report path")
	manifestTemplate := flag.String("manifest-template", "", "release manifest template to link after a pass")
	manifestOutput := flag.String("manifest-output", "", "linked release manifest output path")
	list := flag.Bool("list", false, "print the complete expected matrix and exit")
	flag.Parse()
	if *list {
		inventory, err := releasegate.Enumerate(*root, *fixture)
		if err != nil {
			fail(err)
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(inventory); err != nil {
			fail(err)
		}
		return
	}

	if *reportsDirectory == "" || *output == "" || *manifestTemplate == "" || *manifestOutput == "" {
		fail(fmt.Errorf("--reports, --output, --manifest-template, and --manifest-output are required"))
	}
	reports, err := filepath.Glob(filepath.Join(*reportsDirectory, "*.json"))
	if err != nil {
		fail(err)
	}
	aggregate := releasegate.Evaluate(releasegate.Input{
		Root: *root, FixturePath: *fixture, ProfileReports: reports,
		IndependentReport: *independent, SecurityReport: *security, SourceRevision: *sourceRevision,
	})
	aggregateBytes, err := json.MarshalIndent(aggregate, "", "  ")
	if err != nil {
		fail(err)
	}
	aggregateBytes = append(aggregateBytes, '\n')
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		fail(err)
	}
	relativeOutput, err := relativeInsideRoot(absoluteRoot, *output)
	if err != nil {
		fail(err)
	}
	if _, err := relativeInsideRoot(absoluteRoot, *manifestOutput); err != nil {
		fail(err)
	}
	if err := writeFile(*output, aggregateBytes); err != nil {
		fail(err)
	}
	if aggregate.Status != "passed" {
		fail(fmt.Errorf("release gate failed with %d failure(s); manifest was not written", len(aggregate.Failures)))
	}
	template, err := os.ReadFile(*manifestTemplate)
	if err != nil {
		fail(err)
	}
	aggregateSum := sha256.Sum256(aggregateBytes)
	linked, err := releasegate.LinkManifest(template, aggregate, releasegate.Evidence{
		Path: filepath.ToSlash(relativeOutput), SHA256: hex.EncodeToString(aggregateSum[:]),
	})
	if err != nil {
		fail(err)
	}
	if err := writeFile(*manifestOutput, linked); err != nil {
		fail(err)
	}
}

func relativeInsideRoot(root, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("output must be inside the repository root")
	}
	return relative, nil
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}

func fail(err error) {
	if _, writeErr := os.Stderr.WriteString("naatre-release-gate: " + err.Error() + "\n"); writeErr != nil {
		os.Exit(2)
	}
	os.Exit(1)
}
