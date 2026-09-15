package conformance_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOpenAPIAdapterEvidencePinsDependenciesAndExecutedBoundaries(t *testing.T) {
	t.Parallel()
	var evidence struct {
		Profile           string `json:"profile"`
		FixtureSuite      string `json:"fixtureSuite"`
		Issue             int    `json:"issue"`
		Specification     string `json:"specification"`
		JSONSchemaDialect string `json:"jsonSchemaDialect"`
		Implementation    struct {
			Language       string `json:"language"`
			Package        string `json:"package"`
			MinimumRuntime string `json:"minimumRuntime"`
		} `json:"implementation"`
		Dependencies struct {
			RepositoryBase      string `json:"repositoryBase"`
			CoreAdapterMerge    string `json:"coreAdapterMerge"`
			CoreAdapterRevision string `json:"coreAdapterRevision"`
			Sources             []struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			} `json:"sources"`
		} `json:"dependencies"`
		Directions []struct {
			Direction string   `json:"direction"`
			Status    string   `json:"status"`
			Evidence  []string `json:"evidence"`
			Reason    string   `json:"reason"`
		} `json:"directions"`
		Cases []struct {
			Name         string `json:"name"`
			Category     string `json:"category"`
			Status       string `json:"status"`
			Expected     string `json:"expected"`
			ExpectedCode string `json:"expectedCode"`
		} `json:"cases"`
		StablePublicCodes []string `json:"stablePublicCodes"`
		Reproduce         []string `json:"reproduce"`
	}
	readFixture(t, "openapi-adapter.json", &evidence)
	if evidence.Profile != "adapter.openapi-1" || evidence.FixtureSuite != "1.0.0" || evidence.Issue != 93 ||
		evidence.Specification != "https://spec.openapis.org/oas/v3.2.0" || evidence.JSONSchemaDialect != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("OpenAPI evidence identity = %#v", evidence)
	}
	if evidence.Implementation.Language != "go" || evidence.Implementation.Package != "github.com/valksor/naatre/openapiadapter" || evidence.Implementation.MinimumRuntime != "go1.27.0" {
		t.Fatalf("OpenAPI implementation boundary = %#v", evidence.Implementation)
	}
	for name, revision := range map[string]string{
		"repository base":       evidence.Dependencies.RepositoryBase,
		"core adapter merge":    evidence.Dependencies.CoreAdapterMerge,
		"core adapter revision": evidence.Dependencies.CoreAdapterRevision,
	} {
		if len(revision) != 40 {
			t.Errorf("%s is not an exact revision: %q", name, revision)
		}
	}
	for _, source := range evidence.Dependencies.Sources {
		content, err := os.ReadFile(filepath.Join("../..", filepath.FromSlash(source.Path)))
		if err != nil {
			t.Errorf("read dependency %s: %v", source.Path, err)
			continue
		}
		digest := sha256.Sum256(content)
		if actual := hex.EncodeToString(digest[:]); actual != source.SHA256 {
			t.Errorf("dependency %s digest = %s, want %s", source.Path, actual, source.SHA256)
		}
	}
	statuses := make(map[string]string)
	for _, direction := range evidence.Directions {
		statuses[direction.Direction] = direction.Status
		if direction.Status == "passed" && len(direction.Evidence) == 0 {
			t.Errorf("passed direction %s has no executable evidence", direction.Direction)
		}
	}
	if statuses["schema-import"] != "passed" || statuses["schema-export"] != "passed" || statuses["runtime-consume"] != "passed" || statuses["runtime-expose"] != "unsupported" {
		t.Fatalf("direction statuses = %#v", statuses)
	}
	categories := make(map[string]bool)
	for _, testCase := range evidence.Cases {
		if testCase.Name == "" || testCase.Status != "passed" {
			t.Errorf("invalid evidence case %#v", testCase)
		}
		categories[testCase.Category] = true
		if testCase.ExpectedCode != "" && !slices.Contains(evidence.StablePublicCodes, testCase.ExpectedCode) {
			t.Errorf("case %s uses undeclared code %s", testCase.Name, testCase.ExpectedCode)
		}
	}
	for _, category := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit", "security"} {
		if !categories[category] {
			t.Errorf("missing %s evidence", category)
		}
	}
	if len(evidence.Reproduce) < 6 || !slices.IsSorted(evidence.StablePublicCodes) || len(evidence.StablePublicCodes) != len(slices.Compact(slices.Clone(evidence.StablePublicCodes))) {
		t.Fatal("public codes must be sorted and unique and reproducible commands complete")
	}
	if _, err := json.Marshal(evidence); err != nil {
		t.Fatal(err)
	}
}
