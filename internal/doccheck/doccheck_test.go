package doccheck_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/internal/doccheck"
)

var markdownLink = regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)

type evidence struct {
	Profile  string `json:"profile"`
	Versions struct {
		Specification string `json:"specification"`
		Schema        string `json:"schema"`
		Fixtures      string `json:"fixtures"`
		Runtime       string `json:"runtime"`
		SDKRelease    string `json:"sdkRelease"`
	} `json:"versions"`
	Guides           []string `json:"guides"`
	ExpandedExamples string   `json:"expandedExamples"`
	QuickStarts      []struct {
		Language string `json:"language"`
		Surface  string `json:"surface"`
		Source   string `json:"source"`
		Verify   string `json:"verify"`
	} `json:"quickStarts"`
	SharedSchema struct {
		Path      string   `json:"path"`
		Consumers []string `json:"consumers"`
	} `json:"sharedSchema"`
	DomainExamples []struct {
		Feature string `json:"feature"`
		Fixture string `json:"fixture"`
		Key     string `json:"key"`
	} `json:"domainExamples"`
}

func TestPublishedDocumentationEvidence(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "docs", "v1", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest evidence
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	validateVersions(t, manifest)
	validateGuides(t, root, manifest.Guides)
	requireFile(t, root, manifest.ExpandedExamples)
	validateQuickStarts(t, root, manifest)
	validateSharedSchema(t, root, manifest)
	validateDomainExamples(t, root, manifest)
}

func validateVersions(t *testing.T, manifest evidence) {
	t.Helper()
	if manifest.Profile != "documentation.v1-1" || manifest.Versions.Specification != "1" ||
		manifest.Versions.Schema != "core.schema-1" || manifest.Versions.Fixtures != "1.0.0" ||
		manifest.Versions.Runtime != "0.0.0-development.1" || manifest.Versions.SDKRelease != "unpublished-development-snapshot" {
		t.Fatalf("invalid documentation version tuple: %#v", manifest.Versions)
	}
}

func validateGuides(t *testing.T, root string, guides []string) {
	t.Helper()
	for _, path := range guides {
		requireFile(t, root, path)
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || bytes.Contains(body, []byte("```")) {
			t.Fatalf("guide %s contains an untracked fenced sample or cannot be read: %v", path, err)
		}
		for _, match := range markdownLink.FindAllSubmatch(body, -1) {
			target := string(match[1])
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if info, statErr := os.Stat(filepath.Join(root, filepath.Dir(path), filepath.FromSlash(target))); statErr != nil || info.IsDir() {
				t.Fatalf("guide %s has broken local link %q: %v", path, target, statErr)
			}
		}
	}
}

func validateQuickStarts(t *testing.T, root string, manifest evidence) {
	t.Helper()
	wantStarts := []string{"go/client", "go/server", "javascript-typescript/client", "javascript-typescript/worker", "php/client", "php/worker", "python/client", "python/worker", "rust/client", "rust/worker"}
	var starts []string
	for _, start := range manifest.QuickStarts {
		requireFile(t, root, start.Source)
		if start.Verify == "" {
			t.Fatalf("quick start %s/%s lacks an executable verification command", start.Language, start.Surface)
		}
		starts = append(starts, start.Language+"/"+start.Surface)
	}
	slices.Sort(starts)
	if !slices.Equal(starts, wantStarts) {
		t.Fatalf("quick starts = %v, want %v", starts, wantStarts)
	}
}

func validateSharedSchema(t *testing.T, root string, manifest evidence) {
	t.Helper()
	requireFile(t, root, manifest.SharedSchema.Path)
	wantConsumers := []string{"go", "javascript-typescript", "php", "python", "rust"}
	slices.Sort(manifest.SharedSchema.Consumers)
	if !slices.Equal(manifest.SharedSchema.Consumers, wantConsumers) {
		t.Fatalf("shared-schema consumers = %v", manifest.SharedSchema.Consumers)
	}
}

func validateDomainExamples(t *testing.T, root string, manifest evidence) {
	t.Helper()
	wantFeatures := strings.Fields("adapters aliases batching collection-operations conditional-writes deep-calls deployment-lifecycle federation files filters item-mapping long-running-operations mutations nesting-unnesting pagination parallel-groups partial-errors persisted-operations pipelines subscriptions tooling validation-constraints")
	slices.Sort(wantFeatures)
	var features []string
	for _, example := range manifest.DomainExamples {
		path := filepath.Join(root, example.Fixture)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("domain fixture %s: %v", example.Fixture, err)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(content, &object) != nil || len(object[example.Key]) == 0 {
			t.Fatalf("domain fixture %s lacks executable key %q", example.Fixture, example.Key)
		}
		features = append(features, example.Feature)
	}
	slices.Sort(features)
	if !slices.Equal(features, wantFeatures) {
		t.Fatalf("domain features = %v, want %v", features, wantFeatures)
	}
}

func TestPublicErrorIndexMatchesExecutableFixtures(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	codes, err := doccheck.PublicCodes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) < 100 {
		t.Fatalf("public error inventory unexpectedly small: %d", len(codes))
	}
	want := doccheck.RenderErrorIndex(codes)
	got, err := os.ReadFile(filepath.Join(root, "docs", "v1", "error-codes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("public error index is stale; run go generate ./internal/doccheck")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func requireFile(t *testing.T, root, path string) {
	t.Helper()
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "..") {
		t.Fatalf("unsafe evidence path %q", path)
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || info.IsDir() {
		t.Fatalf("evidence path %q is not a file: %v", path, err)
	}
}
