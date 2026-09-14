package conformance_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestDeveloperToolingFixtureContract(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile     string `json:"profile"`
		Version     string `json:"version"`
		Diagnostics struct {
			Version      string   `json:"version"`
			SharedFields []string `json:"sharedFields"`
		} `json:"diagnostics"`
		Commands struct {
			Names     []string       `json:"names"`
			ExitCodes map[string]int `json:"exitCodes"`
		} `json:"commands"`
		SideEffectFree struct {
			Operations           []string `json:"operations"`
			BusinessHandlerCalls int      `json:"businessHandlerCalls"`
		} `json:"sideEffectFree"`
		Playground struct {
			OptIn            bool     `json:"optIn"`
			RedactedSurfaces []string `json:"redactedSurfaces"`
		} `json:"playground"`
		Mocks struct {
			Scenarios           []string `json:"scenarios"`
			SameSeedSameResult  bool     `json:"sameSeedSameResult"`
			ApplicationEvidence bool     `json:"applicationEvidence"`
		} `json:"mocks"`
		Workflow struct {
			Steps []string `json:"steps"`
		} `json:"workflow"`
	}
	content, err := os.ReadFile("../../conformance/v1/tooling.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Profile != "tooling.workflow-1" || fixture.Version != "1.0.0" || fixture.Diagnostics.Version != "naatre.tooling.diagnostics-1" {
		t.Fatalf("tooling fixture versions = %q %q %q", fixture.Profile, fixture.Version, fixture.Diagnostics.Version)
	}
	for _, field := range []string{"phase", "code", "path"} {
		if !slices.Contains(fixture.Diagnostics.SharedFields, field) {
			t.Errorf("shared diagnostics omit %q", field)
		}
	}
	for _, command := range []string{"validate", "format", "hash", "schema diff", "generate", "manifest", "compatibility", "explain", "conformance"} {
		if !slices.Contains(fixture.Commands.Names, command) {
			t.Errorf("tooling commands omit %q", command)
		}
	}
	if fixture.Commands.ExitCodes["success"] != 0 || fixture.Commands.ExitCodes["diagnostic"] != 1 || fixture.Commands.ExitCodes["usage"] != 2 || fixture.Commands.ExitCodes["io"] != 3 || fixture.Commands.ExitCodes["internal"] != 4 {
		t.Fatalf("published exit codes = %#v", fixture.Commands.ExitCodes)
	}
	if fixture.SideEffectFree.BusinessHandlerCalls != 0 || len(fixture.SideEffectFree.Operations) != 4 {
		t.Fatalf("side-effect-free contract = %#v", fixture.SideEffectFree)
	}
	if !fixture.Playground.OptIn || !slices.Equal(fixture.Playground.RedactedSurfaces, []string{"history", "urls", "logs", "snippets"}) {
		t.Fatalf("playground contract = %#v", fixture.Playground)
	}
	if !fixture.Mocks.SameSeedSameResult || fixture.Mocks.ApplicationEvidence || len(fixture.Mocks.Scenarios) != 5 {
		t.Fatalf("mock contract = %#v", fixture.Mocks)
	}
	if !slices.Equal(fixture.Workflow.Steps, []string{"validate-document", "generate-client", "emit-manifest", "check-schema-compatibility"}) {
		t.Fatalf("workflow steps = %#v", fixture.Workflow.Steps)
	}
}
