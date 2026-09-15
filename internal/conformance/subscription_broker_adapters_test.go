package conformance_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/subscriptionbroker"
)

type subscriptionBrokerAdapterFile struct {
	Profile string `json:"profile,omitempty"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

type subscriptionBrokerAdapterProfile struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Owner        struct {
		Issue              int    `json:"issue"`
		CoreContractIssue  int    `json:"coreContractIssue"`
		NormativeProfile   string `json:"normativeProfile"`
		DependencyRevision string `json:"dependencyRevision"`
	} `json:"owner"`
	Dependencies   []subscriptionBrokerAdapterFile `json:"dependencies"`
	Implementation struct {
		Module                     string                          `json:"module"`
		Package                    string                          `json:"package"`
		GoVersion                  string                          `json:"goVersion"`
		ExternalModuleDependencies []string                        `json:"externalModuleDependencies"`
		Files                      []subscriptionBrokerAdapterFile `json:"files"`
	} `json:"implementation"`
	Runtime struct {
		Language                  string   `json:"language"`
		Minimum                   string   `json:"minimum"`
		PlatformProfile           string   `json:"platformProfile"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
		CertifiedProductVersions  []string `json:"certifiedProductVersions"`
		DriverContract            string   `json:"driverContract"`
		DriverRevisionEvidence    string   `json:"driverRevisionEvidence"`
	} `json:"runtime"`
	Adapters []struct {
		Name           string `json:"name"`
		Implementation string `json:"implementation"`
		Driver         string `json:"driver"`
		Replay         string `json:"replay"`
		Evidence       string `json:"evidence"`
	} `json:"adapters"`
	Limits struct {
		DefaultBindingBytes  int    `json:"defaultBindingBytes"`
		DefaultSnapshotBytes int    `json:"defaultSnapshotBytes"`
		DefaultReplayEvents  int    `json:"defaultReplayEvents"`
		DefaultReplayBytes   uint64 `json:"defaultReplayBytes"`
		DefaultFrameBytes    int    `json:"defaultFrameBytes"`
		DefaultPendingEvents int    `json:"defaultPendingEvents"`
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
	RolloutEvidence struct {
		Profile         string   `json:"profile"`
		Fields          []string `json:"fields"`
		Outcomes        []string `json:"outcomes"`
		ForbiddenFields []string `json:"forbiddenFields"`
	} `json:"rolloutEvidence"`
	Commands []string `json:"commands"`
}

func TestSubscriptionBrokerAdapterProfile(t *testing.T) {
	t.Parallel()
	var profile subscriptionBrokerAdapterProfile
	readFixture(t, "subscription-broker-adapters.json", &profile)
	assertBrokerAdapterIdentity(t, profile)
	assertBrokerAdapterInventory(t, profile)
	assertBrokerAdapterEvidence(t, profile)
	assertBrokerAdapterFiles(t, profile)
}

func assertBrokerAdapterIdentity(t *testing.T, profile subscriptionBrokerAdapterProfile) {
	t.Helper()
	if profile.Profile != subscriptionbroker.Profile || profile.FixtureSuite != "1.0.0" || profile.Owner.Issue != 105 ||
		profile.Owner.CoreContractIssue != 67 || profile.Owner.NormativeProfile != "core.streaming-handles-1" || len(profile.Owner.DependencyRevision) != 40 {
		t.Fatalf("broker adapter ownership = %#v", profile.Owner)
	}
	if profile.Implementation.Module != modulePath || profile.Implementation.Package != modulePath+"/subscriptionbroker" ||
		profile.Implementation.GoVersion != "1.27.0" || len(profile.Implementation.ExternalModuleDependencies) != 0 ||
		profile.Runtime.Language != "go" || profile.Runtime.Minimum != "1.27.0" || profile.Runtime.PlatformProfile != "portable-go-library" ||
		profile.Runtime.DriverContract != "atomic-driver-1" || profile.Runtime.DriverRevisionEvidence != "deployment-required" ||
		len(profile.Runtime.CertifiedOperatingSystems) != 0 || len(profile.Runtime.CertifiedProductVersions) != 0 {
		t.Fatalf("broker adapter runtime boundary = %#v / %#v", profile.Implementation, profile.Runtime)
	}
}

func assertBrokerAdapterInventory(t *testing.T, profile subscriptionBrokerAdapterProfile) {
	t.Helper()
	wantAdapters := []string{"in-process", "nats-jetstream", "postgresql", "redis-streams"}
	gotAdapters := make([]string, 0, len(profile.Adapters))
	for _, adapter := range profile.Adapters {
		if adapter.Implementation == "" || adapter.Driver == "" || adapter.Replay == "" || !strings.HasPrefix(adapter.Evidence, "Test") {
			t.Fatalf("invalid adapter evidence %#v", adapter)
		}
		gotAdapters = append(gotAdapters, adapter.Name)
	}
	slices.Sort(gotAdapters)
	if !slices.Equal(gotAdapters, wantAdapters) {
		t.Fatalf("advertised adapters = %v, want %v", gotAdapters, wantAdapters)
	}

	limits := subscriptionbroker.DefaultLimits()
	if profile.Limits.DefaultBindingBytes != limits.MaxBindingBytes || profile.Limits.DefaultSnapshotBytes != limits.MaxSnapshotBytes ||
		profile.Limits.DefaultReplayEvents != limits.MaxReplayEvents || profile.Limits.DefaultReplayBytes != limits.MaxReplayBytes ||
		profile.Limits.DefaultFrameBytes != limits.MaxFrameBytes || profile.Limits.DefaultPendingEvents != limits.MaxPendingEvents {
		t.Fatalf("adapter limits = %#v, want %#v", profile.Limits, limits)
	}
}

func assertBrokerAdapterEvidence(t *testing.T, profile subscriptionBrokerAdapterProfile) {
	t.Helper()
	wantCodes := []string{
		subscriptionbroker.CodeCancelled, subscriptionbroker.CodeHistoryLost, subscriptionbroker.CodeInvalidConfig,
		subscriptionbroker.CodeInvalidRecord, subscriptionbroker.CodeResourceExhausted,
		subscriptionbroker.CodeSlowConsumer, subscriptionbroker.CodeUnavailable,
	}
	gotCodes := slices.Clone(profile.StableFailureCodes)
	slices.Sort(wantCodes)
	slices.Sort(gotCodes)
	if !slices.Equal(gotCodes, wantCodes) {
		t.Fatalf("stable failure codes = %v, want %v", gotCodes, wantCodes)
	}

	classes := map[string]bool{}
	for _, fixture := range profile.Fixtures {
		if fixture.Name == "" || fixture.Class == "" || len(fixture.Tests) == 0 {
			t.Fatalf("invalid adapter fixture %#v", fixture)
		}
		classes[fixture.Class] = true
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit", "security"} {
		if !classes[class] {
			t.Errorf("adapter fixture evidence omits %q coverage", class)
		}
	}
	if len(profile.Capabilities.Supported) < 15 || len(profile.Capabilities.Unsupported) < 20 ||
		!sliceUnique(profile.Capabilities.Supported) || !sliceUnique(profile.Capabilities.Unsupported) {
		t.Fatalf("broker adapter capability boundary = %#v", profile.Capabilities)
	}
	for _, supported := range profile.Capabilities.Supported {
		if slices.Contains(profile.Capabilities.Unsupported, supported) {
			t.Errorf("capability %q is both supported and unsupported", supported)
		}
	}
	if profile.RolloutEvidence.Profile != subscriptionbroker.Profile || len(profile.RolloutEvidence.Fields) != 5 ||
		len(profile.RolloutEvidence.Outcomes) != 5 || len(profile.RolloutEvidence.ForbiddenFields) < 8 {
		t.Fatalf("rollout evidence boundary = %#v", profile.RolloutEvidence)
	}
	for _, forbidden := range []string{"credential", "handle", "cursor", "payload", "schemaRevision", "authorizationRevision"} {
		if !slices.Contains(profile.RolloutEvidence.ForbiddenFields, forbidden) {
			t.Errorf("rollout evidence does not forbid %q", forbidden)
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
}

func assertBrokerAdapterFiles(t *testing.T, profile subscriptionBrokerAdapterProfile) {
	t.Helper()
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(goMod), "redis/go-redis") || strings.Contains(string(goMod), "nats-io/nats.go") || strings.Contains(string(goMod), "jackc/pgx") {
		t.Fatal("portable adapter profile unexpectedly added a product client dependency")
	}
	root := filepath.Join("..", "..")
	for _, file := range append(profile.Dependencies, profile.Implementation.Files...) {
		if file.Path == "" || len(file.SHA256) != 64 {
			t.Fatalf("invalid broker adapter revision %#v", file)
		}
		if got := conformanceFileDigest(t, filepath.Join(root, filepath.FromSlash(file.Path))); got != file.SHA256 {
			t.Errorf("broker adapter evidence %s digest = %s, want %s", file.Path, got, file.SHA256)
		}
	}
}
