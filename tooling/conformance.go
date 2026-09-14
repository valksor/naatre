package tooling

import (
	"encoding/json"
	"errors"
	"slices"
)

const ToolingProfile = "tooling.workflow-1"

// ValidateConformanceFixture validates the complete portable tooling contract,
// rather than accepting a fixture based only on its profile label.
func ValidateConformanceFixture(input []byte) error {
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
		FormatIdentity struct {
			Preserved []string `json:"preserved"`
			Mutable   []string `json:"mutable"`
		} `json:"formatIdentity"`
		SideEffectFree struct {
			Operations           []string `json:"operations"`
			BusinessHandlerCalls int      `json:"businessHandlerCalls"`
			Redacted             []string `json:"redacted"`
		} `json:"sideEffectFree"`
		Editor struct {
			Features   []string `json:"features"`
			Transport  string   `json:"transport"`
			OwnerIssue int      `json:"ownerIssue"`
		} `json:"editor"`
		Playground struct {
			OptIn            bool     `json:"optIn"`
			Features         []string `json:"features"`
			RedactedSurfaces []string `json:"redactedSurfaces"`
			OwnerIssue       int      `json:"ownerIssue"`
		} `json:"playground"`
		Mocks struct {
			Scenarios           []string `json:"scenarios"`
			Pagination          bool     `json:"pagination"`
			StreamFrames        []string `json:"streamFrames"`
			SameSeedSameResult  bool     `json:"sameSeedSameResult"`
			ApplicationEvidence bool     `json:"applicationEvidence"`
		} `json:"mocks"`
		Configuration struct {
			Precedence []string `json:"precedence"`
			SchemaMode string   `json:"schemaMode"`
			Plugin     []string `json:"plugin"`
		} `json:"configuration"`
		Compatibility struct {
			ManifestProfile      string   `json:"manifestProfile"`
			Checks               []string `json:"checks"`
			GlobalDiffSufficient bool     `json:"globalDiffSufficient"`
		} `json:"compatibility"`
		Workflow struct {
			Steps     []string `json:"steps"`
			Artifacts []string `json:"artifacts"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(input, &fixture); err != nil {
		return err
	}
	valid := fixture.Profile == ToolingProfile && fixture.Version == "1.0.0" &&
		fixture.Diagnostics.Version == DiagnosticVersion && slices.Equal(fixture.Diagnostics.SharedFields, []string{"phase", "code", "path"}) &&
		slices.Equal(fixture.Commands.Names, []string{"validate", "format", "canonicalize", "hash", "schema export", "schema diff", "generate", "manifest", "compatibility", "explain", "mock", "conformance"}) &&
		fixture.Commands.ExitCodes["success"] == 0 && fixture.Commands.ExitCodes["diagnostic"] == 1 && fixture.Commands.ExitCodes["usage"] == 2 && fixture.Commands.ExitCodes["io"] == 3 && fixture.Commands.ExitCodes["internal"] == 4 &&
		slices.Equal(fixture.FormatIdentity.Preserved, []string{"operations", "fragments", "selections", "stages", "branches", "literals", "extensions"}) && slices.Equal(fixture.FormatIdentity.Mutable, []string{"whitespace", "object-key-order"}) &&
		slices.Equal(fixture.SideEffectFree.Operations, []string{"explain", "completion", "introspection", "mock"}) && fixture.SideEffectFree.BusinessHandlerCalls == 0 && slices.Equal(fixture.SideEffectFree.Redacted, []string{"literal-arguments", "authorization-policy", "hidden-schema-names"}) &&
		slices.Equal(fixture.Editor.Features, []string{"diagnostics", "completion", "hover", "definition", "rename", "deprecation"}) && fixture.Editor.Transport == "lsp-3.17-or-documented-adapter" && fixture.Editor.OwnerIssue == 94 &&
		fixture.Playground.OptIn && slices.Equal(fixture.Playground.Features, []string{"schema", "variables", "request-error-paths", "stream-frames", "sdk-examples"}) && slices.Equal(fixture.Playground.RedactedSurfaces, []string{"history", "urls", "logs", "snippets"}) && fixture.Playground.OwnerIssue == 91 &&
		slices.Equal(fixture.Mocks.Scenarios, []string{"success", "null", "missing", "failure", "unknown-variant"}) && fixture.Mocks.Pagination &&
		slices.Equal(fixture.Mocks.StreamFrames, []string{"open", "next", "complete"}) && fixture.Mocks.SameSeedSameResult && !fixture.Mocks.ApplicationEvidence &&
		slices.Equal(fixture.Configuration.Precedence, []string{"flags", "environment", "project-file", "defaults"}) && fixture.Configuration.SchemaMode == "offline" && slices.Equal(fixture.Configuration.Plugin, []string{"absolute-path", "explicit-trust", "stable-id", "version", "sha256"}) &&
		fixture.Compatibility.ManifestProfile == ManifestVersion && slices.Equal(fixture.Compatibility.Checks, []string{"document-digest", "operation-name", "operation-kind", "schema-plan"}) && !fixture.Compatibility.GlobalDiffSufficient &&
		slices.Equal(fixture.Workflow.Steps, []string{"validate-document", "generate-client", "emit-manifest", "check-schema-compatibility"}) && slices.Equal(fixture.Workflow.Artifacts, []string{"generator-model-1", "generator-output", "operation-manifest-1", "schema-digest"})
	if !valid {
		return errors.New("invalid tooling conformance fixture")
	}
	return nil
}
