package conformance_test

import (
	"os"
	"slices"
	"strings"
	"testing"
)

type observabilityIntegrationProfile struct {
	Profile string `json:"profile"`
	Owner   struct {
		Issue             int    `json:"issue"`
		CoreContractIssue int    `json:"coreContractIssue"`
		NormativeProfile  string `json:"normativeProfile"`
	} `json:"owner"`
	Implementation struct {
		Module       string   `json:"module"`
		GoVersion    string   `json:"goVersion"`
		Packages     []string `json:"packages"`
		Dependencies []struct {
			Module  string `json:"module"`
			Version string `json:"version"`
		} `json:"dependencies"`
	} `json:"implementation"`
	Runtime struct {
		Language                  string   `json:"language"`
		Minimum                   string   `json:"minimum"`
		PlatformProfile           string   `json:"platformProfile"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
	} `json:"runtime"`
	Limits struct {
		DefaultActiveSpans      int `json:"defaultActiveSpans"`
		DefaultCausalReferences int `json:"defaultCausalReferences"`
		DefaultLinksPerEvent    int `json:"defaultLinksPerEvent"`
		DefaultAuditBatch       int `json:"defaultAuditBatch"`
		DefaultAuditFieldBytes  int `json:"defaultAuditFieldBytes"`
	} `json:"limits"`
	Fixtures []struct {
		Name     string   `json:"name"`
		Polarity string   `json:"polarity"`
		Boundary string   `json:"boundary"`
		Tests    []string `json:"tests"`
	} `json:"fixtures"`
	Capabilities struct {
		Supported   []string `json:"supported"`
		Unsupported []string `json:"unsupported"`
	} `json:"capabilities"`
	Commands []string `json:"commands"`
}

func TestObservabilityIntegrationProfile(t *testing.T) {
	t.Parallel()
	var profile observabilityIntegrationProfile
	readFixture(t, "observability-integrations.json", &profile)
	if profile.Profile != "operations.observability-integrations-go-1" || profile.Owner.Issue != 107 ||
		profile.Owner.CoreContractIssue != 27 || profile.Owner.NormativeProfile != "operations.observability-1" {
		t.Fatalf("profile ownership = %#v", profile)
	}
	if profile.Implementation.Module != modulePath || profile.Implementation.GoVersion != "1.27.0" ||
		profile.Runtime.Language != "go" || profile.Runtime.Minimum != "1.27.0" ||
		profile.Runtime.PlatformProfile != "portable-go" || len(profile.Runtime.CertifiedOperatingSystems) != 0 {
		t.Fatalf("runtime boundary = %#v / %#v", profile.Implementation, profile.Runtime)
	}
	wantPackages := []string{modulePath + "/observability/audit", modulePath + "/observability/otel"}
	if !slices.Equal(profile.Implementation.Packages, wantPackages) {
		t.Fatalf("packages = %v, want %v", profile.Implementation.Packages, wantPackages)
	}
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	seenDependencies := make(map[string]bool, len(profile.Implementation.Dependencies))
	for _, dependency := range profile.Implementation.Dependencies {
		if dependency.Module == "" || dependency.Version == "" || seenDependencies[dependency.Module] {
			t.Fatalf("invalid dependency evidence %#v", dependency)
		}
		seenDependencies[dependency.Module] = true
		if !strings.Contains(string(goMod), dependency.Module+" "+dependency.Version) {
			t.Errorf("go.mod does not pin %s %s", dependency.Module, dependency.Version)
		}
	}
	for _, required := range []string{
		"go.opentelemetry.io/otel", "go.opentelemetry.io/otel/log", "go.opentelemetry.io/otel/metric",
		"go.opentelemetry.io/otel/sdk", "go.opentelemetry.io/otel/sdk/log", "go.opentelemetry.io/otel/sdk/metric",
		"go.opentelemetry.io/otel/trace",
	} {
		if !seenDependencies[required] {
			t.Errorf("dependency evidence is missing %s", required)
		}
	}
	if profile.Limits.DefaultActiveSpans <= 0 || profile.Limits.DefaultCausalReferences <= 0 ||
		profile.Limits.DefaultLinksPerEvent <= 0 || profile.Limits.DefaultAuditBatch <= 0 ||
		profile.Limits.DefaultAuditFieldBytes <= 0 {
		t.Fatalf("resource limits = %#v", profile.Limits)
	}
	polarities := map[string]bool{}
	names := map[string]bool{}
	for _, fixture := range profile.Fixtures {
		if fixture.Name == "" || fixture.Boundary == "" || len(fixture.Tests) == 0 || names[fixture.Name] {
			t.Fatalf("invalid or duplicate fixture %#v", fixture)
		}
		names[fixture.Name] = true
		polarities[fixture.Polarity] = true
		for _, testName := range fixture.Tests {
			if !strings.HasPrefix(testName, "Test") {
				t.Fatalf("fixture %q has invalid test evidence %q", fixture.Name, testName)
			}
		}
	}
	for _, polarity := range []string{"positive", "negative", "boundary", "cancellation"} {
		if !polarities[polarity] {
			t.Errorf("fixture evidence is missing %s coverage", polarity)
		}
	}
	if len(profile.Capabilities.Supported) == 0 || len(profile.Capabilities.Unsupported) == 0 {
		t.Fatal("supported and unsupported capability inventories are required")
	}
	for _, supported := range profile.Capabilities.Supported {
		if slices.Contains(profile.Capabilities.Unsupported, supported) {
			t.Errorf("capability %q is both supported and unsupported", supported)
		}
	}
	if len(profile.Commands) < 4 {
		t.Fatalf("commands = %v", profile.Commands)
	}
	for _, command := range profile.Commands {
		if !strings.Contains(command, "GOWORK=off go test") || !strings.Contains(command, "-count=1") {
			t.Errorf("command is not reproducible: %q", command)
		}
	}
}
