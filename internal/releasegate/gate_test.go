package releasegate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEvaluateCompleteMatrix(t *testing.T) {
	t.Parallel()
	input, wantCells := completeInput(t)

	report := Evaluate(input)

	if report.Status != "passed" || len(report.Failures) != 0 {
		t.Fatalf("Evaluate() status = %s, failures = %#v", report.Status, report.Failures)
	}
	if len(report.Matrix) != wantCells {
		t.Fatalf("Evaluate() matrix cells = %d, want %d", len(report.Matrix), wantCells)
	}
	if len(report.RunnerPaths) != 16 {
		t.Fatalf("Evaluate() runner paths = %d, want 16", len(report.RunnerPaths))
	}
	if len(report.SharedVectors) != 4 {
		t.Fatalf("Evaluate() shared vectors = %d, want 4", len(report.SharedVectors))
	}
}

func TestEvaluateFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*testing.T, *Input)
		code string
	}{
		{
			name: "missing advertised profile",
			edit: func(t *testing.T, input *Input) { input.ProfileReports = input.ProfileReports[1:] },
			code: "MATRIX_EVIDENCE_INCOMPLETE",
		},
		{
			name: "version skew",
			edit: func(t *testing.T, input *Input) {
				mutateProfileReport(t, input.ProfileReports[0], func(report *profileReport) { report.Versions.Fixtures = "9.9.9" })
			},
			code: "VERSION_SKEW",
		},
		{
			name: "shared output mismatch",
			edit: func(t *testing.T, input *Input) {
				mutateProfileReport(t, clientReport(t, *input, "ruby"), func(report *profileReport) {
					report.Run.Artifacts[0].SHA256 = "f" + report.Run.Artifacts[0].SHA256[1:]
				})
			},
			code: "SHARED_VECTOR_MISMATCH",
		},
		{
			name: "infrastructure failure",
			edit: func(t *testing.T, input *Input) {
				mutateProfileReport(t, input.ProfileReports[0], func(report *profileReport) {
					report.Results[0].Status = "infrastructure-failure"
					report.Results[0].Diagnostics = []diagnostic{{Code: "TOOL_UNAVAILABLE", Message: "tool unavailable"}}
					report.Claims = nil
				})
			},
			code: "INFRASTRUCTURE_FAILURE",
		},
		{
			name: "missing independent runner",
			edit: func(t *testing.T, input *Input) { input.IndependentReport = "" },
			code: "RUNNER_EVIDENCE_MISSING",
		},
		{
			name: "unbounded report identifier",
			edit: func(t *testing.T, input *Input) {
				mutateProfileReport(t, input.ProfileReports[0], func(report *profileReport) {
					report.Run.ID = string(bytes.Repeat([]byte{'x'}, 130))
				})
			},
			code: "REPORT_INVALID",
		},
		{
			name: "credential shaped report metadata",
			edit: func(t *testing.T, input *Input) {
				mutateProfileReport(t, input.ProfileReports[0], func(report *profileReport) {
					report.Results[0].Diagnostics = []diagnostic{{Code: "RUN_FAILED", Message: "Bearer credential-value"}}
				})
			},
			code: "REPORT_UNSAFE",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input, _ := completeInput(t)
			test.edit(t, &input)
			report := Evaluate(input)
			if report.Status != "failed" || !slices.ContainsFunc(report.Failures, func(failure Failure) bool {
				return failure.Code == test.code
			}) {
				t.Fatalf("Evaluate() = %s %#v, want %s", report.Status, report.Failures, test.code)
			}
		})
	}
}

func TestLinkManifest(t *testing.T) {
	t.Parallel()
	input, _ := completeInput(t)
	aggregate := Evaluate(input)
	template := []byte(`{
  "profile":"naatre.release-manifest-1",
  "release":{"id":"1.0.0","sourceRevision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "support":[{"component":"future","status":"planned","evidence":[]}],
  "conformanceClaims":[]
}`)
	evidence := Evidence{Path: "releases/1.0.0/release-gate.json", SHA256: "b" + string(make([]byte, 63))}
	evidence.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	linked, err := LinkManifest(template, aggregate, evidence)
	if err != nil {
		t.Fatalf("LinkManifest() error = %v", err)
	}
	var manifest releaseManifest
	if err := json.Unmarshal(linked, &manifest); err != nil {
		t.Fatalf("decode linked manifest: %v", err)
	}
	if len(manifest.ConformanceClaims) != 1 || manifest.ConformanceClaims[0].Profile != "release.stable-1" || manifest.ConformanceClaims[0].Report != evidence {
		t.Fatalf("linked claims = %#v", manifest.ConformanceClaims)
	}
}

func TestEvaluateAcceptsExplicitUnsupportedOptionalCapability(t *testing.T) {
	t.Parallel()
	input, _ := completeInput(t)
	streaming := profileReportPath(t, input, "javascript-typescript", "transport.streaming-1")
	var unsupported profileReport
	content, err := os.ReadFile(streaming)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &unsupported); err != nil {
		t.Fatal(err)
	}
	capability := unsupported.Claims[0].Capabilities[0]
	unsupported.Claims[0].Capabilities = unsupported.Claims[0].Capabilities[1:]
	unsupported.Results[0].Capabilities = unsupported.Results[0].Capabilities[1:]
	writeJSON(t, streaming, unsupported)
	unsupported.Run.ID = "javascript-typescript-unsupported"
	unsupported.Claims = []claim{}
	unsupported.Results = []result{{
		Profile: "transport.streaming-1", EvidenceRole: "streaming-transport", Status: "unsupported", Capability: capability,
		Capabilities: []string{}, ExecutedClauses: []string{}, Evidence: []profileEvidence{}, Diagnostics: []diagnostic{},
	}}
	unsupportedPath := filepath.Join(filepath.Dir(streaming), "javascript-typescript-streaming-unsupported.json")
	writeJSON(t, unsupportedPath, unsupported)
	input.ProfileReports = append(input.ProfileReports, unsupportedPath)

	report := Evaluate(input)
	if report.Status != "passed" {
		t.Fatalf("Evaluate() status = %s, failures = %#v", report.Status, report.Failures)
	}
}

func TestLinkManifestRejectsPlannedEvidence(t *testing.T) {
	t.Parallel()
	input, _ := completeInput(t)
	aggregate := Evaluate(input)
	template := []byte(`{
  "profile":"naatre.release-manifest-1",
  "release":{"id":"1.0.0","sourceRevision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "support":[{"component":"future","status":"planned","evidence":[{"path":"fake.json","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}],
  "conformanceClaims":[]
}`)
	_, err := LinkManifest(template, aggregate, Evidence{Path: "gate.json", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if err == nil {
		t.Fatal("LinkManifest() accepted planned-only evidence")
	}
}

func completeInput(t *testing.T) (Input, int) {
	t.Helper()
	root := t.TempDir()
	copyGateFiles(t, root)
	data, failures := loadGateData(Input{Root: root})
	if len(failures) != 0 {
		t.Fatalf("load gate data: %#v", failures)
	}
	reportsDirectory := filepath.Join(root, "evidence", "profiles")
	if err := os.MkdirAll(reportsDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	var reportPaths []string
	cellCount := 0
	for _, implementation := range data.matrix.Implementations {
		for _, profileID := range implementation.PlannedProfiles {
			path := profilePathFor(profileID, implementation.Language)
			report := exampleProfileReport(data, implementation.Implementation, implementation.Language, profileID, path)
			file := filepath.Join(reportsDirectory, implementation.Language+"-"+profileID+".json")
			writeJSON(t, file, report)
			reportPaths = append(reportPaths, file)
			cellCount++
		}
	}
	for _, integration := range data.matrix.IntegrationPaths {
		path := ExecutionPath{Source: Endpoint{Kind: integration.Source.Kind, Language: integration.Source.Language}, Destination: Endpoint{Kind: integration.Destination.Kind, Language: integration.Destination.Language}}
		report := exampleProfileReport(data, integration.Destination.Implementation, integration.Destination.Language, integration.Profile, path)
		file := filepath.Join(reportsDirectory, integration.Destination.Language+"-worker.json")
		writeJSON(t, file, report)
		reportPaths = append(reportPaths, file)
		cellCount++
	}
	independent := filepath.Join(root, "evidence", "independent.json")
	writeJSON(t, independent, exampleRunnerReport(data, "naatre-independent-js", "javascript-typescript", "suite.contract-1", true))
	security := filepath.Join(root, "evidence", "security.json")
	writeJSON(t, security, exampleRunnerReport(data, "naatre-go", "go", "suite.security-gate-1", false))
	return Input{Root: root, ProfileReports: reportPaths, IndependentReport: independent, SecurityReport: security, SourceRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, cellCount
}

func copyGateFiles(t *testing.T, root string) {
	t.Helper()
	for _, path := range []string{
		"conformance/v1/release-gate.json", "conformance/v1/suite.json", "conformance/v1/profiles.json",
		"conformance/v1/compatibility.json", "conformance/profile-report.schema.json", "conformance/release-gate-report.schema.json",
		"conformance/independent/runner.mjs", "releases/release-manifest.schema.json",
	} {
		content, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		destination := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exampleProfileReport(data *gateData, implementation, language, profileID string, path ExecutionPath) profileReport {
	profile := data.profiles[profileID]
	var report profileReport
	report.Protocol = data.registry.ReportProtocol
	report.Run.ID = language + "-run"
	report.Run.Operator = "release-orchestrator"
	report.Run.Command = []string{"verify", language, profileID}
	for index, name := range data.fixture.SharedVectorArtifacts {
		report.Run.Artifacts = append(report.Run.Artifacts, namedArtifact{Name: name, SHA256: repeatedDigest(byte('1' + index))})
	}
	report.Versions.Spec = data.registry.SpecVersion
	report.Versions.Fixtures = data.registry.FixtureVersion
	report.Versions.Profiles = data.registry.RegistryVersion
	report.Versions.RunnerProtocol = data.registry.RunnerProtocol
	report.Versions.Canonicalization = "c14n-1"
	report.Versions.SchemaRevision = data.schemaDigest
	report.Revisions = revisionSet{
		Specification: revision{Version: data.registry.SpecVersion, SHA256: repeatedDigest('5')},
		Schema:        revision{Version: "schema-1", SHA256: data.schemaDigest},
		Canonical:     revision{Version: "c14n-1", SHA256: repeatedDigest('6')},
		Fixtures:      revision{Version: data.registry.FixtureVersion, SHA256: repeatedDigest('7')},
		Generator:     applicableRevision{Status: "pinned", Version: "generator-1", SHA256: repeatedDigest('8')},
		Runtime:       applicableRevision{Status: "pinned", Version: "runtime-1", SHA256: repeatedDigest('9')},
		SDK:           applicableRevision{Status: "pinned", Version: "sdk-1", SHA256: repeatedDigest('a')},
		Transport:     applicableRevision{Status: "pinned", Version: "transport-1", SHA256: repeatedDigest('b')},
	}
	report.Implementation.Name = implementation
	report.Implementation.Version = "1.0.0"
	report.Implementation.Language = language
	report.Implementation.RuntimeVersion = "runtime-1"
	report.Implementation.SpecVersion = report.Versions.Spec
	report.Implementation.FixtureVersion = report.Versions.Fixtures
	report.Implementation.ProfileVersion = report.Versions.Profiles
	report.Implementation.SchemaRevision = report.Versions.SchemaRevision
	report.Environment.OS = "linux"
	report.Environment.Architecture = "amd64"
	report.Environment.WireTransports = []string{"http-1"}
	report.Environment.StreamTransports = []string{}
	report.Environment.FeatureFlags = []string{}
	report.Environment.ScalarPrecision = []string{"arbitrary-precision"}
	report.Environment.CancellationCapabilities = []string{"context"}
	report.Path = path
	capabilities := make([]string, 0, len(profile.OptionalCapabilities))
	for _, optional := range profile.OptionalCapabilities {
		capabilities = append(capabilities, optional.ID)
	}
	report.Claims = []claim{{Profile: profileID, Capabilities: capabilities}}
	item := result{Profile: profileID, EvidenceRole: profile.EvidenceRole, Status: "passed", Capabilities: capabilities, ExecutedClauses: profile.RequiredClauses, Diagnostics: []diagnostic{}}
	for _, fixture := range profile.RequiredFixtures {
		item.Evidence = append(item.Evidence, profileEvidence{Fixture: fixture.Path, SHA256: fixture.SHA256})
	}
	report.Results = []result{item}
	return report
}

func profilePathFor(profileID, language string) ExecutionPath {
	source := Endpoint{Language: language}
	destination := Endpoint{Language: language}
	switch profileID {
	case "wire.codec-1":
		source.Kind, destination.Kind = "codec", "codec"
	case "sdk.client-1":
		source.Kind, destination.Kind, destination.Language = "sdk", "native-runtime", "go"
	case "transport.http.server-1":
		source.Kind, destination.Kind = "sdk", "http-server"
	case "runtime.execution-1":
		source.Kind, destination.Kind = "sdk", "native-runtime"
	case "transport.streaming-1":
		source.Kind, destination.Kind = "sdk", "stream-transport"
	case "runtime.persisted-1":
		source.Kind, destination.Kind = "sdk", "persisted-store"
	case "schema.tooling-1":
		source.Kind, destination.Kind = "sdk", "schema-tool"
	case "runtime.federation-1":
		source.Kind, destination.Kind = "gateway", "federation-coordinator"
	case "runtime.extensions-1":
		source.Kind, destination.Kind = "sdk", "extension-host"
	}
	return ExecutionPath{Source: source, Destination: destination}
}

func exampleRunnerReport(data *gateData, name, language, profile string, fullSuite bool) runnerReport {
	var report runnerReport
	report.Protocol = data.fixture.RunnerProtocol
	report.FixtureVersion = data.fixture.FixtureVersion
	report.Runner.Name = name
	report.Runner.Version = "1.0.0"
	report.Runner.Language = language
	report.Runner.Platform = "linux-amd64"
	if fullSuite {
		report.Path = ExecutionPath{Source: Endpoint{Kind: "codec", Language: language}, Destination: Endpoint{Kind: "codec", Language: language}}
	} else {
		report.Path = ExecutionPath{Source: Endpoint{Kind: "gateway", Language: "go"}, Destination: Endpoint{Kind: "native-runtime", Language: "go"}}
	}
	item := struct {
		Profile  string            `json:"profile"`
		Status   string            `json:"status"`
		Evidence []profileEvidence `json:"evidence"`
	}{Profile: profile, Status: "passed"}
	if fullSuite {
		for _, fixture := range data.suite.Files {
			item.Evidence = append(item.Evidence, profileEvidence{Fixture: fixture.Path, SHA256: fixture.SHA256})
		}
	} else {
		for _, fixture := range data.suite.Files {
			if fixture.Path == "v1/security-gate.json" {
				item.Evidence = append(item.Evidence, profileEvidence{Fixture: fixture.Path, SHA256: fixture.SHA256})
			}
		}
	}
	report.Results = append(report.Results, item)
	return report
}

func mutateProfileReport(t *testing.T, path string, mutate func(*profileReport)) {
	t.Helper()
	var report profileReport
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	mutate(&report)
	writeJSON(t, path, report)
}

func clientReport(t *testing.T, input Input, language string) string {
	t.Helper()
	for _, path := range input.ProfileReports {
		var report profileReport
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(content, &report); err != nil {
			t.Fatal(err)
		}
		if report.Implementation.Language == language && len(report.Claims) == 1 && report.Claims[0].Profile == "sdk.client-1" {
			return path
		}
	}
	t.Fatalf("client report for %s not found", language)
	return ""
}

func profileReportPath(t *testing.T, input Input, language, profile string) string {
	t.Helper()
	for _, path := range input.ProfileReports {
		var report profileReport
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(content, &report); err != nil {
			t.Fatal(err)
		}
		if report.Implementation.Language == language && len(report.Claims) == 1 && report.Claims[0].Profile == profile {
			return path
		}
	}
	t.Fatalf("profile report for %s %s not found", language, profile)
	return ""
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repeatedDigest(character byte) string {
	return string(bytes.Repeat([]byte{character}, 64))
}
