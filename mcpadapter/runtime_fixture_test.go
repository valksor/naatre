package mcpadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRuntimeConformanceEvidenceMatchesImplementation(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../conformance/v1/mcp-runtime.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile             string    `json:"profile"`
		OwnerIssue          int       `json:"ownerIssue"`
		NormativeOwnerIssue int       `json:"normativeOwnerIssue"`
		CombinedMatrixOwner int       `json:"combinedMatrixOwnerIssue"`
		MappingMatrix       []Mapping `json:"mappingMatrix"`
		Supported           []string  `json:"supported"`
		Unsupported         []string  `json:"unsupported"`
		FailureCodes        []string  `json:"failureCodes"`
		Dependencies        struct {
			Issue64Commit string                        `json:"issue64Commit"`
			CoreFixture   struct{ Path, SHA256 string } `json:"coreFixture"`
			Specification struct{ Path, SHA256 string } `json:"specification"`
			GoMod         struct{ Path, SHA256 string } `json:"goMod"`
		} `json:"dependencies"`
		Limits struct {
			RequestBytes, ResponseBytes, Pending, Pages, Sessions int
		} `json:"limits"`
		Cases []struct {
			ID, Class, Status string
		} `json:"cases"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	evidence := runtimeEvidence(Consume, Stdio, DefaultWireLimits())
	if fixture.Profile != RuntimeProfile || fixture.OwnerIssue != 103 || fixture.NormativeOwnerIssue != 64 || fixture.CombinedMatrixOwner != 69 || fixture.Dependencies.Issue64Commit != Issue64Revision {
		t.Fatalf("profile identity or ownership = %#v", fixture)
	}
	if !reflect.DeepEqual(fixture.MappingMatrix, evidence.Mappings) || !reflect.DeepEqual(fixture.Supported, evidence.Supported) || !reflect.DeepEqual(fixture.Unsupported, evidence.Unsupported) || !reflect.DeepEqual(fixture.FailureCodes, evidence.FailureCodes) {
		t.Fatal("runtime evidence lists diverged from the implementation")
	}
	if fixture.Limits.RequestBytes != evidence.WireLimits.MaxRequestBytes || fixture.Limits.ResponseBytes != evidence.WireLimits.MaxResponseBytes || fixture.Limits.Pending != evidence.WireLimits.MaxPending || fixture.Limits.Pages != evidence.WireLimits.MaxPages || fixture.Limits.Sessions != evidence.WireLimits.MaxSessions {
		t.Fatalf("runtime limits = %#v", fixture.Limits)
	}
	for _, dependency := range []struct{ Path, SHA256 string }{fixture.Dependencies.CoreFixture, fixture.Dependencies.Specification, fixture.Dependencies.GoMod} {
		assertRuntimeDigest(t, dependency.Path, dependency.SHA256)
	}
	classes := map[string]bool{"positive": false, "negative": false, "boundary": false, "cancellation": false, "resource-limit": false}
	for _, testCase := range fixture.Cases {
		if testCase.ID == "" || testCase.Status != "passed" {
			t.Fatalf("invalid runtime case %#v", testCase)
		}
		classes[testCase.Class] = true
	}
	for class, covered := range classes {
		if !covered {
			t.Fatalf("runtime evidence omits %s cases", class)
		}
	}
}

func assertRuntimeDigest(t testing.TB, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", path))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected {
		t.Fatalf("digest mismatch for %s", path)
	}
}
