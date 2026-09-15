package conformancerunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/valksor/naatre/tooling/lsp"
)

const lspProfile = lsp.Profile

type lspEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type lspFixture struct {
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
			Issue    int           `json:"issue"`
			Revision string        `json:"revision"`
			Profile  string        `json:"profile"`
			Evidence []lspEvidence `json:"evidence"`
		} `json:"dependencies"`
	} `json:"implementation"`
	Runtime struct {
		Server             string   `json:"server"`
		MinimumGo          string   `json:"minimumGo"`
		Client             string   `json:"client"`
		ExecutedNode       string   `json:"executedNode"`
		Transport          string   `json:"transport"`
		PositionEncoding   string   `json:"positionEncoding"`
		PlatformProfile    string   `json:"platformProfile"`
		CertifiedOperating []string `json:"certifiedOperatingSystems"`
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
	Evidence []lspEvidence `json:"evidence"`
	Commands []string      `json:"commands"`
}

func (r *Runner) verifyLSP(_ context.Context, _ Request) Result {
	var fixture lspFixture
	_, fixtureEvidence, err := r.loadPinnedFixture("v1/lsp.json", &fixture)
	if err != nil || validateLSPFixture(fixture, r.manifest.FixtureVersion) != nil {
		return failureResult(lspProfile, "LSP_FIXTURE_INVALID", "v1/lsp.json")
	}
	evidence := []Evidence{fixtureEvidence}
	repositoryRoot := filepath.Dir(r.conformanceRoot)
	files := append([]lspEvidence(nil), fixture.Evidence...)
	files = append(files, fixture.Implementation.Dependencies[0].Evidence...)
	for _, pinned := range files {
		content, readErr := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(pinned.Path)))
		digest := sha256.Sum256(content)
		if readErr != nil || hex.EncodeToString(digest[:]) != pinned.SHA256 {
			return failureResult(lspProfile, "LSP_EVIDENCE_MISMATCH", pinned.Path)
		}
		evidence = append(evidence, Evidence{Fixture: pinned.Path, SHA256: pinned.SHA256})
	}
	result := emptyResult(lspProfile, "passed", "")
	result.Capabilities = []string{lspProfile}
	result.Evidence = evidence
	return result
}

func validateLSPFixture(fixture lspFixture, fixtureVersion string) error {
	defaults := lsp.DefaultServerLimits()
	if fixture.Profile != lsp.Profile || fixture.FixtureSuite != fixtureVersion || fixture.Owner.Issue != 94 ||
		fixture.Owner.CoreContractIssue != 55 || fixture.Owner.NormativeProfile != toolingProfile || fixture.Owner.Protocol != "lsp-3.17" ||
		fixture.Implementation.Module != "github.com/valksor/naatre" || fixture.Implementation.GoVersion != "1.27.0" ||
		!slices.Equal(fixture.Implementation.Packages, []string{"github.com/valksor/naatre/tooling/lsp", "github.com/valksor/naatre/cmd/naatre-lsp"}) ||
		len(fixture.Implementation.Dependencies) != 1 || fixture.Implementation.Dependencies[0].Issue != 55 ||
		fixture.Implementation.Dependencies[0].Revision == "" || fixture.Implementation.Dependencies[0].Profile != toolingProfile ||
		len(fixture.Implementation.Dependencies[0].Evidence) != 2 || fixture.Runtime.Server != "go-standard-library" ||
		fixture.Runtime.MinimumGo != "1.27.0" || fixture.Runtime.Client != "editors/vscode/client.mjs" || fixture.Runtime.ExecutedNode == "" ||
		fixture.Runtime.Transport != "stdio-content-length" || fixture.Runtime.PositionEncoding != lsp.PositionEncoding ||
		fixture.Runtime.PlatformProfile != "portable-process-stdio" || len(fixture.Runtime.CertifiedOperating) != 0 ||
		fixture.Limits.MessageBytes != defaults.MaxMessageBytes || fixture.Limits.DocumentBytes != defaults.MaxDocumentBytes ||
		fixture.Limits.OpenDocuments != defaults.MaxOpenDocuments || fixture.Limits.ChangesPerUpdate != defaults.MaxChangesPerUpdate ||
		fixture.Limits.ConcurrentRequests != defaults.MaxConcurrent || len(fixture.FailureCodes) != 12 ||
		len(fixture.Capabilities.Supported) == 0 || len(fixture.Capabilities.Unsupported) == 0 || len(fixture.Evidence) < 6 || len(fixture.Commands) < 4 {
		return errors.New("incompatible LSP fixture")
	}
	classes := make(map[string]bool)
	for _, item := range fixture.Fixtures {
		if item.Name == "" || len(item.Tests) == 0 {
			return errors.New("incomplete LSP protocol fixture")
		}
		classes[item.Class] = true
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classes[class] {
			return errors.New("incomplete LSP fixture classes")
		}
	}
	encoded, _ := json.Marshal(fixture)
	for _, code := range []string{lsp.CodeSchemaMismatch, lsp.CodeCancelled, lsp.CodeResourceLimit, lsp.CodeInternal} {
		if !slices.Contains(fixture.FailureCodes, code) || !bytes.Contains(encoded, []byte(code)) {
			return errors.New("incomplete LSP failure vocabulary")
		}
	}
	return nil
}
