package conformance_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type lspProfileEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type lspProfileFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Owner        struct {
		Issue             int    `json:"issue"`
		CoreContractIssue int    `json:"coreContractIssue"`
		NormativeProfile  string `json:"normativeProfile"`
		Protocol          string `json:"protocol"`
	} `json:"owner"`
	Implementation struct {
		Module       string   `json:"module"`
		GoVersion    string   `json:"goVersion"`
		Packages     []string `json:"packages"`
		Dependencies []struct {
			Issue    int                  `json:"issue"`
			Revision string               `json:"revision"`
			Profile  string               `json:"profile"`
			Evidence []lspProfileEvidence `json:"evidence"`
		} `json:"dependencies"`
	} `json:"implementation"`
	Runtime struct {
		Server                    string   `json:"server"`
		MinimumGo                 string   `json:"minimumGo"`
		Client                    string   `json:"client"`
		ExecutedNode              string   `json:"executedNode"`
		Transport                 string   `json:"transport"`
		PositionEncoding          string   `json:"positionEncoding"`
		PlatformProfile           string   `json:"platformProfile"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
	} `json:"runtime"`
	Limits struct {
		MessageBytes       int `json:"messageBytes"`
		DocumentBytes      int `json:"documentBytes"`
		OpenDocuments      int `json:"openDocuments"`
		ChangesPerUpdate   int `json:"changesPerUpdate"`
		ConcurrentRequests int `json:"concurrentRequests"`
	} `json:"limits"`
	FailureCodes []string `json:"failureCodes"`
	Fixtures     []struct {
		Name  string   `json:"name"`
		Class string   `json:"class"`
		Tests []string `json:"tests"`
	} `json:"fixtures"`
	Capabilities struct {
		Supported   []string `json:"supported"`
		Unsupported []string `json:"unsupported"`
	} `json:"capabilities"`
	Evidence []lspProfileEvidence `json:"evidence"`
	Commands []string             `json:"commands"`
}

func TestLSPProfilePublishesExactBoundariesAndEvidence(t *testing.T) {
	var fixture lspProfileFixture
	readFixture(t, "lsp.json", &fixture)
	if fixture.Profile != "tooling.lsp-1" || fixture.FixtureSuite != "1.0.0" || fixture.Owner.Issue != 94 ||
		fixture.Owner.CoreContractIssue != 55 || fixture.Owner.NormativeProfile != "tooling.workflow-1" || fixture.Owner.Protocol != "lsp-3.17" {
		t.Fatalf("LSP ownership = %#v", fixture)
	}
	if fixture.Implementation.Module != modulePath || fixture.Implementation.GoVersion != "1.27.0" ||
		!slices.Equal(fixture.Implementation.Packages, []string{modulePath + "/tooling/lsp", modulePath + "/cmd/naatre-lsp"}) ||
		len(fixture.Implementation.Dependencies) != 1 || fixture.Implementation.Dependencies[0].Issue != 55 ||
		fixture.Implementation.Dependencies[0].Revision == "" || fixture.Implementation.Dependencies[0].Profile != "tooling.workflow-1" {
		t.Fatalf("LSP implementation boundary = %#v", fixture.Implementation)
	}
	if fixture.Runtime.Server != "go-standard-library" || fixture.Runtime.MinimumGo != "1.27.0" ||
		fixture.Runtime.Client != "editors/vscode/client.mjs" || fixture.Runtime.ExecutedNode != "26.8.2" ||
		fixture.Runtime.Transport != "stdio-content-length" || fixture.Runtime.PositionEncoding != "utf-16" ||
		fixture.Runtime.PlatformProfile != "portable-process-stdio" || len(fixture.Runtime.CertifiedOperatingSystems) != 0 {
		t.Fatalf("LSP runtime boundary = %#v", fixture.Runtime)
	}
	if fixture.Limits.MessageBytes != 4<<20 || fixture.Limits.DocumentBytes != 1<<20 || fixture.Limits.OpenDocuments != 64 ||
		fixture.Limits.ChangesPerUpdate != 256 || fixture.Limits.ConcurrentRequests != 16 {
		t.Fatalf("LSP limits = %#v", fixture.Limits)
	}
	classes := make(map[string]bool)
	for _, item := range fixture.Fixtures {
		classes[item.Class] = item.Name != "" && len(item.Tests) > 0
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classes[class] {
			t.Errorf("missing %s fixture", class)
		}
	}
	if len(fixture.FailureCodes) != 12 || len(fixture.Capabilities.Supported) == 0 || len(fixture.Capabilities.Unsupported) < 40 {
		t.Fatal("failure and capability inventories must be closed and non-empty")
	}
	for _, command := range fixture.Commands {
		if !strings.Contains(command, "go ") && !strings.HasPrefix(command, "node ") {
			t.Errorf("non-reproducible command %q", command)
		}
	}
	for _, evidence := range append(fixture.Evidence, fixture.Implementation.Dependencies[0].Evidence...) {
		content, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(evidence.Path)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != evidence.SHA256 {
			t.Errorf("evidence digest drift for %s", evidence.Path)
		}
	}
}
