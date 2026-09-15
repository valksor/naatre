package conformance_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/webhook"
)

type webhookAdapterFile struct {
	Profile string `json:"profile,omitempty"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

type webhookAdapterProfile struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Owner        struct {
		Issue              int    `json:"issue"`
		CoreContractIssue  int    `json:"coreContractIssue"`
		NormativeProfile   string `json:"normativeProfile"`
		DependencyRevision string `json:"dependencyRevision"`
	} `json:"owner"`
	Dependencies   []webhookAdapterFile `json:"dependencies"`
	Implementation struct {
		Module       string `json:"module"`
		Package      string `json:"package"`
		GoVersion    string `json:"goVersion"`
		Dependencies []struct {
			Module  string `json:"module"`
			Version string `json:"version"`
		} `json:"dependencies"`
		Files []webhookAdapterFile `json:"files"`
	} `json:"implementation"`
	Runtime struct {
		Language                  string   `json:"language"`
		Minimum                   string   `json:"minimum"`
		Database                  string   `json:"database"`
		Driver                    string   `json:"driver"`
		HTTPStack                 string   `json:"httpStack"`
		MinimumTLS                string   `json:"minimumTLS"`
		PlatformProfile           string   `json:"platformProfile"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
	} `json:"runtime"`
	Limits struct {
		DefaultIdentifierBytes int     `json:"defaultIdentifierBytes"`
		DefaultPayloadBytes    int     `json:"defaultPayloadBytes"`
		DefaultResponseBytes   int64   `json:"defaultResponseBytes"`
		DefaultBatchEvents     int     `json:"defaultBatchEvents"`
		DefaultDispatchBatch   int     `json:"defaultDispatchBatch"`
		DefaultRecoveryBatch   int     `json:"defaultRecoveryBatch"`
		MaximumAttempts        uint32  `json:"maximumAttempts"`
		RetryDelaysMillis      []int64 `json:"retryDelaysMilliseconds"`
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

func TestWebhookAdapterProfile(t *testing.T) {
	t.Parallel()
	var profile webhookAdapterProfile
	readFixture(t, "webhook-adapters.json", &profile)
	if profile.Profile != webhook.Profile || profile.FixtureSuite != "1.0.0" || profile.Owner.Issue != 90 ||
		profile.Owner.CoreContractIssue != 48 || profile.Owner.NormativeProfile != "core.events-1" || len(profile.Owner.DependencyRevision) != 40 {
		t.Fatalf("webhook ownership = %#v", profile.Owner)
	}
	if profile.Implementation.Module != modulePath || profile.Implementation.Package != modulePath+"/webhook" ||
		profile.Implementation.GoVersion != "1.27.0" || profile.Runtime.Language != "go" || profile.Runtime.Minimum != "1.27.0" ||
		profile.Runtime.Database != "sqlite" || profile.Runtime.Driver != "modernc.org/sqlite" || profile.Runtime.HTTPStack != "net/http" ||
		profile.Runtime.MinimumTLS != "1.2" || profile.Runtime.PlatformProfile != "portable-go-library" || len(profile.Runtime.CertifiedOperatingSystems) != 0 {
		t.Fatalf("webhook runtime boundary = %#v / %#v", profile.Implementation, profile.Runtime)
	}
	if len(profile.Implementation.Dependencies) != 1 || profile.Implementation.Dependencies[0].Module != "modernc.org/sqlite" ||
		profile.Implementation.Dependencies[0].Version != "v1.58.0" {
		t.Fatalf("webhook dependencies = %#v", profile.Implementation.Dependencies)
	}
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "modernc.org/sqlite v1.58.0") {
		t.Fatal("go.mod does not pin the declared SQLite dependency")
	}

	limits := webhook.DefaultLimits()
	if profile.Limits.DefaultIdentifierBytes != limits.MaxIdentifierBytes || profile.Limits.DefaultPayloadBytes != limits.MaxPayloadBytes ||
		profile.Limits.DefaultResponseBytes != limits.MaxResponseBytes || profile.Limits.DefaultBatchEvents != limits.MaxBatchEvents ||
		profile.Limits.DefaultDispatchBatch != limits.MaxDispatchBatch || profile.Limits.DefaultRecoveryBatch != limits.MaxRecoveryBatch ||
		profile.Limits.MaximumAttempts != 6 || !slices.Equal(profile.Limits.RetryDelaysMillis, []int64{1000, 2000, 4000, 8000, 8000}) {
		t.Fatalf("webhook limits = %#v, want %#v", profile.Limits, limits)
	}

	wantCodes := []string{
		webhook.CodeCancelled, webhook.CodeDNSRebinding, webhook.CodeEndpointRevoked, webhook.CodeEventReordered,
		webhook.CodeFenceRejected, webhook.CodeHTTPPermanent, webhook.CodeHTTPTransient, webhook.CodeInvalidConfig,
		webhook.CodeInvalidRecord, webhook.CodeKeyRevoked, webhook.CodeRedirect, webhook.CodeReplayStore,
		webhook.CodeResourceExhausted, webhook.CodeRetryExhausted, webhook.CodeSignatureInvalid, webhook.CodeStale,
		webhook.CodeStoreUnavailable, webhook.CodeTransport,
	}
	gotCodes := slices.Clone(profile.StableFailureCodes)
	slices.Sort(wantCodes)
	slices.Sort(gotCodes)
	if !slices.Equal(gotCodes, wantCodes) {
		t.Fatalf("stable failure codes = %v, want %v", gotCodes, wantCodes)
	}

	testSource, err := os.ReadFile("../../webhook/webhook_test.go")
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	names := map[string]bool{}
	for _, fixture := range profile.Fixtures {
		if fixture.Name == "" || fixture.Class == "" || len(fixture.Tests) == 0 || names[fixture.Name] {
			t.Fatalf("invalid or duplicate webhook fixture %#v", fixture)
		}
		names[fixture.Name] = true
		classes[fixture.Class] = true
		for _, testName := range fixture.Tests {
			rootName := strings.Split(testName, "/")[0]
			if !strings.HasPrefix(rootName, "Test") || !strings.Contains(string(testSource), "func "+rootName+"(") {
				t.Fatalf("fixture %q has missing test evidence %q", fixture.Name, testName)
			}
		}
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classes[class] {
			t.Errorf("webhook fixture evidence omits %q coverage", class)
		}
	}
	if len(profile.Capabilities.Supported) != 16 || len(profile.Capabilities.Unsupported) != 36 ||
		!sliceUnique(profile.Capabilities.Supported) || !sliceUnique(profile.Capabilities.Unsupported) {
		t.Fatalf("webhook capability boundary = %#v", profile.Capabilities)
	}
	for _, supported := range profile.Capabilities.Supported {
		if slices.Contains(profile.Capabilities.Unsupported, supported) {
			t.Errorf("capability %q is both supported and unsupported", supported)
		}
	}
	if len(profile.Commands) != 5 || !strings.Contains(profile.Commands[1], "go test -race") || profile.Commands[4] != "node conformance/independent/events.mjs" {
		t.Fatalf("webhook commands = %v", profile.Commands)
	}
	for _, command := range profile.Commands[:4] {
		if !strings.HasPrefix(command, "GOWORK=off go ") {
			t.Errorf("webhook command is not workspace-independent: %q", command)
		}
	}

	root := filepath.Join("..", "..")
	for _, file := range append(profile.Dependencies, profile.Implementation.Files...) {
		if file.Path == "" || len(file.SHA256) != 64 {
			t.Fatalf("invalid webhook revision %#v", file)
		}
		if got := conformanceFileDigest(t, filepath.Join(root, filepath.FromSlash(file.Path))); got != file.SHA256 {
			t.Errorf("webhook evidence %s digest = %s, want %s", file.Path, got, file.SHA256)
		}
	}
}
