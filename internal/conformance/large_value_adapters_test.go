package conformance_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/largevalueadapter"
)

type largeValueAdapterProfile struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Owner        struct {
		Issue              int    `json:"issue"`
		CoreContractIssue  int    `json:"coreContractIssue"`
		NormativeProfile   string `json:"normativeProfile"`
		DependencyRevision string `json:"dependencyRevision"`
	} `json:"owner"`
	Dependencies   []largeValueAdapterFile `json:"dependencies"`
	Implementation struct {
		Module               string                  `json:"module"`
		Package              string                  `json:"package"`
		GoVersion            string                  `json:"goVersion"`
		ExternalDependencies []string                `json:"externalDependencies"`
		Files                []largeValueAdapterFile `json:"files"`
	} `json:"implementation"`
	Runtime struct {
		Language                  string   `json:"language"`
		Minimum                   string   `json:"minimum"`
		PlatformProfile           string   `json:"platformProfile"`
		RequiresListener          bool     `json:"requiresListener"`
		RequiresNetworkFixtures   bool     `json:"requiresNetworkForFixtures"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
		CertifiedArchitectures    []string `json:"certifiedArchitectures"`
	} `json:"runtime"`
	Profiles []struct {
		Name           string `json:"name"`
		Upload         bool   `json:"upload"`
		Download       bool   `json:"download"`
		Implementation string `json:"implementation"`
	} `json:"profiles"`
	Limits struct {
		MaximumTransferBytes int64 `json:"maximumTransferBytes"`
		MaximumChunkBytes    int64 `json:"maximumChunkBytes"`
		MaximumParts         int   `json:"maximumParts"`
		MaximumSessions      int   `json:"maximumSessions"`
		MaximumHeaderBytes   int64 `json:"maximumHeaderBytes"`
		MaximumURLBytes      int   `json:"maximumURLBytes"`
		MaximumIdentifier    int   `json:"maximumIdentifierBytes"`
		MaximumRedirects     int   `json:"maximumRedirects"`
		MaximumCleanupBatch  int   `json:"maximumCleanupBatch"`
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

type largeValueAdapterFile struct {
	Profile string `json:"profile,omitempty"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

func TestLargeValueAdapterProfile(t *testing.T) {
	t.Parallel()
	var profile largeValueAdapterProfile
	readFixture(t, "large-value-adapters.json", &profile)
	if profile.Profile != largevalueadapter.Profile || profile.FixtureSuite != "1.0.0" || profile.Owner.Issue != 87 ||
		profile.Owner.CoreContractIssue != 46 || profile.Owner.NormativeProfile != "core.large-value-1" || len(profile.Owner.DependencyRevision) != 40 {
		t.Fatalf("adapter ownership = %#v", profile.Owner)
	}
	if profile.Implementation.Module != modulePath || profile.Implementation.Package != modulePath+"/largevalueadapter" ||
		profile.Implementation.GoVersion != "1.27.0" || len(profile.Implementation.ExternalDependencies) != 0 ||
		profile.Runtime.Language != "go" || profile.Runtime.Minimum != "1.27.0" ||
		profile.Runtime.PlatformProfile != "portable-go-library-and-net-http-handler" || profile.Runtime.RequiresListener ||
		profile.Runtime.RequiresNetworkFixtures || len(profile.Runtime.CertifiedOperatingSystems) != 0 || len(profile.Runtime.CertifiedArchitectures) != 0 {
		t.Fatalf("adapter implementation/runtime boundary = %#v / %#v", profile.Implementation, profile.Runtime)
	}

	wantProfiles := []string{"application", "direct", "multipart", "presigned", "resumable"}
	gotProfiles := make([]string, 0, len(profile.Profiles))
	for _, transfer := range profile.Profiles {
		if transfer.Implementation == "" || !transfer.Upload || ((transfer.Name == "direct" || transfer.Name == "presigned" || transfer.Name == "application") != transfer.Download) {
			t.Errorf("invalid transfer profile boundary %#v", transfer)
		}
		gotProfiles = append(gotProfiles, transfer.Name)
	}
	slices.Sort(gotProfiles)
	if !slices.Equal(gotProfiles, wantProfiles) {
		t.Fatalf("transfer profiles = %v, want %v", gotProfiles, wantProfiles)
	}

	limits := largevalueadapter.DefaultLimits()
	if profile.Limits.MaximumTransferBytes != limits.MaximumTransferBytes || profile.Limits.MaximumChunkBytes != limits.MaximumChunkBytes ||
		profile.Limits.MaximumParts != limits.MaximumParts || profile.Limits.MaximumSessions != limits.MaximumSessions ||
		profile.Limits.MaximumHeaderBytes != limits.MaximumHeaderBytes || profile.Limits.MaximumURLBytes != limits.MaximumURLBytes ||
		profile.Limits.MaximumIdentifier != limits.MaximumIdentifier || profile.Limits.MaximumRedirects != limits.MaximumRedirects ||
		profile.Limits.MaximumCleanupBatch != limits.MaximumCleanupBatch {
		t.Fatalf("adapter limits = %#v, want %#v", profile.Limits, limits)
	}

	wantCodes := []string{
		largevalueadapter.CodeCancelled, largevalueadapter.CodeCleanupFailed, largevalueadapter.CodeConflict,
		largevalueadapter.CodeEgressDenied, largevalueadapter.CodeIntegrityFailed, largevalueadapter.CodeInvalidConfig,
		largevalueadapter.CodeInvalidRequest, largevalueadapter.CodeResourceExhausted, largevalueadapter.CodeTransferFailed,
		largevalueadapter.CodeUnavailable, largevalueadapter.CodeUnsupportedCapability,
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
	if len(profile.Capabilities.Supported) != 14 || len(profile.Capabilities.Unsupported) != 26 ||
		!sliceUnique(profile.Capabilities.Supported) || !sliceUnique(profile.Capabilities.Unsupported) {
		t.Fatalf("adapter capability boundary = %#v", profile.Capabilities)
	}
	for _, supported := range profile.Capabilities.Supported {
		if slices.Contains(profile.Capabilities.Unsupported, supported) {
			t.Errorf("capability %q is both supported and unsupported", supported)
		}
	}
	if len(profile.Commands) != 5 || !strings.Contains(profile.Commands[1], "go test -race") || profile.Commands[4] != "GOWORK=off go generate ./..." {
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
