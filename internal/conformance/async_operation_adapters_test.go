package conformance_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/asyncoperation"
	naatreruntime "github.com/valksor/naatre/runtime"
)

type asyncOperationAdapterProfile struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Owner        struct {
		Issue              int    `json:"issue"`
		CoreContractIssue  int    `json:"coreContractIssue"`
		NormativeProfile   string `json:"normativeProfile"`
		DependencyRevision string `json:"dependencyRevision"`
	} `json:"owner"`
	Dependencies   []asyncOperationAdapterFile `json:"dependencies"`
	Implementation struct {
		Module       string `json:"module"`
		Package      string `json:"package"`
		GoVersion    string `json:"goVersion"`
		Dependencies []struct {
			Module  string `json:"module"`
			Version string `json:"version"`
		} `json:"dependencies"`
		Files []asyncOperationAdapterFile `json:"files"`
	} `json:"implementation"`
	Runtime struct {
		Language                  string   `json:"language"`
		Minimum                   string   `json:"minimum"`
		Database                  string   `json:"database"`
		Driver                    string   `json:"driver"`
		PlatformProfile           string   `json:"platformProfile"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
	} `json:"runtime"`
	Limits struct {
		DefaultIdentifierBytes int `json:"defaultIdentifierBytes"`
		DefaultBindingBytes    int `json:"defaultBindingBytes"`
		DefaultPayloadBytes    int `json:"defaultPayloadBytes"`
		DefaultRecordBytes     int `json:"defaultRecordBytes"`
		DefaultPendingBatch    int `json:"defaultPendingBatch"`
		DefaultCollectionBatch int `json:"defaultCollectionBatch"`
	} `json:"limits"`
	Fixtures []struct {
		Name  string   `json:"name"`
		Class string   `json:"class"`
		Tests []string `json:"tests"`
	} `json:"fixtures"`
	StableFailureCodes []string `json:"stableFailureCodes"`
	Capabilities       struct {
		Supported   []string `json:"supported"`
		Unsupported []string `json:"unsupported"`
	} `json:"capabilities"`
	Commands []string `json:"commands"`
}

type asyncOperationAdapterFile struct {
	Profile string `json:"profile,omitempty"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

func TestAsyncOperationAdapterProfile(t *testing.T) {
	t.Parallel()
	var profile asyncOperationAdapterProfile
	readFixture(t, "async-operation-adapters.json", &profile)
	if profile.Profile != asyncoperation.Profile || profile.FixtureSuite != "1.0.0" || profile.Owner.Issue != 89 ||
		profile.Owner.CoreContractIssue != 47 || profile.Owner.NormativeProfile != "operations.async-1" || len(profile.Owner.DependencyRevision) != 40 {
		t.Fatalf("adapter ownership = %#v", profile.Owner)
	}
	if profile.Implementation.Module != modulePath || profile.Implementation.Package != modulePath+"/asyncoperation" ||
		profile.Implementation.GoVersion != "1.27.0" || profile.Runtime.Language != "go" || profile.Runtime.Minimum != "1.27.0" ||
		profile.Runtime.Database != "sqlite" || profile.Runtime.Driver != "modernc.org/sqlite" ||
		profile.Runtime.PlatformProfile != "portable-go-library" || len(profile.Runtime.CertifiedOperatingSystems) != 0 {
		t.Fatalf("adapter runtime boundary = %#v / %#v", profile.Implementation, profile.Runtime)
	}
	if len(profile.Implementation.Dependencies) != 1 || profile.Implementation.Dependencies[0].Module != "modernc.org/sqlite" ||
		profile.Implementation.Dependencies[0].Version != "v1.58.0" {
		t.Fatalf("adapter module dependencies = %#v", profile.Implementation.Dependencies)
	}
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "modernc.org/sqlite v1.58.0") {
		t.Fatal("go.mod does not pin the declared SQLite dependency")
	}

	limits := asyncoperation.DefaultLimits()
	if profile.Limits.DefaultIdentifierBytes != limits.MaxIdentifierBytes || profile.Limits.DefaultBindingBytes != limits.MaxBindingBytes ||
		profile.Limits.DefaultPayloadBytes != limits.MaxPayloadBytes || profile.Limits.DefaultRecordBytes != limits.MaxRecordBytes ||
		profile.Limits.DefaultPendingBatch != limits.MaxPendingBatch || profile.Limits.DefaultCollectionBatch != limits.MaxCollectionBatch {
		t.Fatalf("adapter limits = %#v, want %#v", profile.Limits, limits)
	}

	wantCodes := []string{
		asyncoperation.CodeCancelled, asyncoperation.CodeFenceRejected, asyncoperation.CodeInvalidConfig,
		asyncoperation.CodeInvalidRecord, asyncoperation.CodeLeaseExpired, asyncoperation.CodeResourceExhausted,
		asyncoperation.CodeStoreUnavailable, asyncoperation.CodeUnavailable, naatreruntime.CodeIdempotencyConflict,
	}
	gotCodes := slices.Clone(profile.StableFailureCodes)
	slices.Sort(wantCodes)
	slices.Sort(gotCodes)
	if !slices.Equal(gotCodes, wantCodes) {
		t.Fatalf("stable failure codes = %v, want %v", gotCodes, wantCodes)
	}

	classes := map[string]bool{}
	names := map[string]bool{}
	for _, fixture := range profile.Fixtures {
		if fixture.Name == "" || fixture.Class == "" || len(fixture.Tests) == 0 || names[fixture.Name] {
			t.Fatalf("invalid or duplicate adapter fixture %#v", fixture)
		}
		names[fixture.Name] = true
		classes[fixture.Class] = true
		for _, testName := range fixture.Tests {
			if !strings.HasPrefix(testName, "Test") {
				t.Fatalf("fixture %q has invalid test evidence %q", fixture.Name, testName)
			}
		}
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classes[class] {
			t.Errorf("adapter fixture evidence omits %q coverage", class)
		}
	}
	if len(profile.Capabilities.Supported) != 10 || len(profile.Capabilities.Unsupported) != 25 ||
		!sliceUnique(profile.Capabilities.Supported) || !sliceUnique(profile.Capabilities.Unsupported) {
		t.Fatalf("adapter capability boundary = %#v", profile.Capabilities)
	}
	for _, supported := range profile.Capabilities.Supported {
		if slices.Contains(profile.Capabilities.Unsupported, supported) {
			t.Errorf("capability %q is both supported and unsupported", supported)
		}
	}
	if len(profile.Commands) != 4 || !strings.Contains(profile.Commands[1], "go test -race") {
		t.Fatalf("adapter commands = %v", profile.Commands)
	}
	for _, command := range profile.Commands {
		if !strings.HasPrefix(command, "GOWORK=off go ") {
			t.Errorf("adapter command is not workspace-independent: %q", command)
		}
	}

	root := filepath.Join("..", "..")
	for _, file := range append(profile.Dependencies, profile.Implementation.Files...) {
		if file.Path == "" || len(file.SHA256) != 64 {
			t.Fatalf("invalid adapter revision %#v", file)
		}
		if got := conformanceFileDigest(t, filepath.Join(root, filepath.FromSlash(file.Path))); got != file.SHA256 {
			t.Errorf("adapter evidence %s digest = %s, want %s", file.Path, got, file.SHA256)
		}
	}
}

func sliceUnique(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
