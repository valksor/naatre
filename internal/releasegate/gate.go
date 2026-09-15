package releasegate

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	digestPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	revisionPattern    = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
	secretPattern      = regexp.MustCompile(`(?i)(token|secret|password|authorization|cookie|bearer\s+|github_pat_|gh[pousr]_)`)
	unsafeKeyPattern   = regexp.MustCompile(`(?i)(token|secret|password|authorization|cookie)`)
	unsafeValuePattern = regexp.MustCompile(`(?i)(gh[pousr]_|github_pat_|bearer\s+|https?://[^/\s:@]+:[^/\s@]+@)`)
	identifierPattern  = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._@/+-]{0,126}|[A-Za-z0-9][A-Za-z0-9._@/+-]{0,127})$`)
	diagnosticPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

type loadedReport struct {
	path     string
	evidence Evidence
	report   profileReport
	results  map[string]result
	claims   map[string]claim
}

type gateData struct {
	root         string
	fixture      gateFixture
	suite        suiteManifest
	registry     profileRegistry
	matrix       compatibilityMatrix
	profiles     map[string]profileDefinition
	schemaDigest string
	inputs       []Evidence
}

// Enumerate returns the complete advertised matrix without executing it.
func Enumerate(root, fixturePath string) (Inventory, error) {
	data, failures := loadGateData(Input{Root: root, FixturePath: fixturePath})
	if len(failures) != 0 {
		return Inventory{}, fmt.Errorf("release-gate inventory is invalid: %s", failures[0].Message)
	}
	inventory := Inventory{
		Protocol: "naatre.release-gate.inventory-1",
		Versions: Versions{Gate: data.fixture.GateVersion, Spec: data.suite.SpecVersion, Fixtures: data.suite.FixtureVersion,
			Profiles: data.registry.RegistryVersion, RunnerProtocol: data.registry.RunnerProtocol, ReportProtocol: data.registry.ReportProtocol,
			Canonicalization: "c14n-1", SchemaRevision: data.schemaDigest},
		Languages: slices.Clone(data.suite.Languages),
	}
	for _, implementation := range data.matrix.Implementations {
		for _, profileID := range implementation.PlannedProfiles {
			profile := data.profiles[profileID]
			inventory.Matrix = append(inventory.Matrix, ExpectedMatrix{Kind: "implementation", Implementation: implementation.Implementation,
				Language: implementation.Language, Profile: profileID, EvidenceRole: profile.EvidenceRole, EligiblePaths: slices.Clone(profile.EligiblePaths)})
		}
	}
	for _, path := range data.matrix.IntegrationPaths {
		profile := data.profiles[path.Profile]
		inventory.Matrix = append(inventory.Matrix, ExpectedMatrix{Kind: "remote-worker", Implementation: path.Destination.Implementation,
			Language: path.Destination.Language, Profile: path.Profile, EvidenceRole: profile.EvidenceRole, EligiblePaths: []ProfilePath{{Source: path.Source.Kind, Destination: path.Destination.Kind}}})
	}
	for _, language := range data.suite.Languages {
		inventory.RunnerPaths = append(inventory.RunnerPaths, ExpectedRunner{Kind: "sdk-to-go", Language: language, Profile: "sdk.client-1"})
	}
	for _, path := range data.matrix.IntegrationPaths {
		inventory.RunnerPaths = append(inventory.RunnerPaths, ExpectedRunner{Kind: "gateway-to-worker", Language: path.Destination.Language, Profile: path.Profile})
	}
	inventory.RunnerPaths = append(inventory.RunnerPaths,
		ExpectedRunner{Kind: "independent-non-go", Language: data.fixture.IndependentRunner.Language, Profile: data.fixture.IndependentRunner.Profile},
		ExpectedRunner{Kind: "security-gate", Language: data.fixture.SecurityGate.Language, Profile: data.fixture.SecurityGate.Profile},
	)
	slices.Sort(inventory.Languages)
	slices.SortFunc(inventory.Matrix, func(left, right ExpectedMatrix) int {
		return strings.Compare(left.Kind+"\x00"+left.Language+"\x00"+left.Profile, right.Kind+"\x00"+right.Language+"\x00"+right.Profile)
	})
	slices.SortFunc(inventory.RunnerPaths, func(left, right ExpectedRunner) int {
		return strings.Compare(left.Kind+"\x00"+left.Language, right.Kind+"\x00"+right.Language)
	})
	return inventory, nil
}

// Evaluate validates all supplied evidence and returns a deterministic,
// machine-readable release decision. Any missing or untrustworthy input makes
// the report fail closed.
func Evaluate(input Input) Report {
	report := Report{Protocol: "naatre.release-gate.report-1", Status: "failed", SourceRevision: input.SourceRevision}
	data, failures := loadGateData(input)
	if data == nil {
		report.Failures = failures
		return report
	}
	report.Protocol = data.fixture.AggregateProtocol
	report.Versions = Versions{
		Gate: data.fixture.GateVersion, Spec: data.suite.SpecVersion, Fixtures: data.suite.FixtureVersion,
		Profiles: data.registry.RegistryVersion, RunnerProtocol: data.registry.RunnerProtocol,
		ReportProtocol: data.registry.ReportProtocol, Canonicalization: "c14n-1", SchemaRevision: data.schemaDigest,
	}
	report.Inputs = append(report.Inputs, data.inputs...)
	if !revisionPattern.MatchString(input.SourceRevision) {
		addFailure(&failures, "SOURCE_REVISION_INVALID", "source revision must be an exact 40 to 64 character lowercase hexadecimal revision")
	}

	reports := make([]loadedReport, 0, len(input.ProfileReports))
	for _, path := range input.ProfileReports {
		loaded, reportFailures := loadProfileReport(data, path)
		failures = append(failures, reportFailures...)
		if loaded != nil {
			reports = append(reports, *loaded)
			report.Inputs = append(report.Inputs, loaded.evidence)
		}
	}
	slices.SortFunc(reports, func(left, right loadedReport) int { return strings.Compare(left.path, right.path) })

	matrixResults, runnerPaths, matrixFailures := validateMatrixCoverage(data, reports)
	report.Matrix = matrixResults
	report.RunnerPaths = runnerPaths
	failures = append(failures, matrixFailures...)
	failures = append(failures, validateImplementationRevisionConsistency(reports)...)

	vectors, vectorFailures := validateSharedVectors(data, reports)
	report.SharedVectors = vectors
	failures = append(failures, vectorFailures...)

	independentPath, independentEvidence, independentFailures := validateRunnerReport(data, input.IndependentReport, "independent-non-go")
	if independentPath != nil {
		report.RunnerPaths = append(report.RunnerPaths, *independentPath)
		report.Inputs = append(report.Inputs, independentEvidence)
	}
	failures = append(failures, independentFailures...)
	securityPath, securityEvidence, securityFailures := validateRunnerReport(data, input.SecurityReport, "security-gate")
	if securityPath != nil {
		report.RunnerPaths = append(report.RunnerPaths, *securityPath)
		report.Inputs = append(report.Inputs, securityEvidence)
	}
	failures = append(failures, securityFailures...)

	slices.SortFunc(report.Matrix, compareMatrixResults)
	slices.SortFunc(report.RunnerPaths, func(left, right RunnerPath) int {
		return strings.Compare(left.Kind+"\x00"+left.Language, right.Kind+"\x00"+right.Language)
	})
	slices.SortFunc(report.Inputs, func(left, right Evidence) int { return strings.Compare(left.Path, right.Path) })
	report.Failures = sortedFailures(failures)
	if len(report.Failures) == 0 {
		report.Status = "passed"
	}
	return report
}

func loadGateData(input Input) (*gateData, []Failure) {
	root, err := filepath.Abs(input.Root)
	if err != nil {
		return nil, []Failure{{Code: "ROOT_INVALID", Message: "resolve repository root: " + err.Error()}}
	}
	fixturePath := input.FixturePath
	if fixturePath == "" {
		fixturePath = filepath.Join(root, "conformance", "v1", "release-gate.json")
	} else if !filepath.IsAbs(fixturePath) {
		fixturePath = filepath.Join(root, fixturePath)
	}
	var fixture gateFixture
	fixtureBytes, readErr := os.ReadFile(fixturePath)
	if readErr != nil {
		return nil, []Failure{{Code: "GATE_FIXTURE_INVALID", Message: readErr.Error()}}
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		return nil, []Failure{{Code: "GATE_FIXTURE_INVALID", Message: err.Error()}}
	}
	conformanceRoot := filepath.Dir(filepath.Dir(fixturePath))
	data := &gateData{root: root, fixture: fixture}
	if relative, relativeErr := safeRelative(root, fixturePath); relativeErr == nil {
		data.inputs = append(data.inputs, Evidence{Path: relative, SHA256: digest(fixtureBytes)})
	}
	var failures []Failure
	if fixture.Profile != "suite.release-gate-1" || fixture.AggregateProtocol != "naatre.release-gate.report-1" ||
		fixture.ExecutionOwnerIssue != 69 || fixture.PublicationOwnerIssue != 69 {
		addFailure(&failures, "GATE_FIXTURE_INVALID", "release-gate identity or ownership is invalid")
	}
	if !sameSet(fixture.RequiredRunnerPaths, []string{"sdk-to-go", "gateway-to-worker", "independent-non-go"}) ||
		!sameSet(fixture.SharedVectorArtifacts, []string{"canonical-vectors", "schema-vectors", "validation-vectors", "generated-filter-sort-vectors"}) ||
		fixture.SecurityGate.Implementation != "naatre-go-security-gate" || fixture.SecurityGate.Language != "go" || fixture.SecurityGate.Profile != "suite.security-gate-1" ||
		fixture.IndependentRunner.Name != "naatre-independent-js" || fixture.IndependentRunner.Language != "javascript-typescript" || fixture.IndependentRunner.Profile != "suite.contract-1" {
		addFailure(&failures, "GATE_FIXTURE_INVALID", "release-gate runner or shared-vector requirements are incomplete")
	}
	if err := decodeFile(filepath.Join(conformanceRoot, "v1", "suite.json"), &data.suite); err != nil {
		addFailure(&failures, "SUITE_INVALID", err.Error())
	}
	for _, path := range []string{"v1/suite.json", fixture.ProfileRegistry, fixture.CompatibilityMatrix, fixture.ProfileReportSchema, fixture.AggregateReportSchema, fixture.ReleaseManifestSchema, fixture.IndependentRunner.Source} {
		absolute := filepath.Join(conformanceRoot, path)
		content, inputErr := os.ReadFile(absolute)
		if inputErr != nil {
			addFailure(&failures, "GATE_INPUT_MISSING", inputErr.Error())
			continue
		}
		relative, relativeErr := safeRelative(root, absolute)
		if relativeErr != nil {
			addFailure(&failures, "GATE_INPUT_INVALID", relativeErr.Error())
			continue
		}
		data.inputs = append(data.inputs, Evidence{Path: relative, SHA256: digest(content)})
	}
	if err := decodeFile(filepath.Join(conformanceRoot, fixture.ProfileRegistry), &data.registry); err != nil {
		addFailure(&failures, "PROFILE_REGISTRY_INVALID", err.Error())
	}
	if err := decodeFile(filepath.Join(conformanceRoot, fixture.CompatibilityMatrix), &data.matrix); err != nil {
		addFailure(&failures, "COMPATIBILITY_MATRIX_INVALID", err.Error())
	}
	schemaBytes, err := os.ReadFile(filepath.Join(conformanceRoot, fixture.ProfileReportSchema))
	if err != nil {
		addFailure(&failures, "REPORT_SCHEMA_INVALID", err.Error())
	} else {
		data.schemaDigest = digest(schemaBytes)
	}
	data.profiles = make(map[string]profileDefinition, len(data.registry.Profiles))
	for _, profile := range data.registry.Profiles {
		if _, duplicate := data.profiles[profile.ID]; duplicate || profile.ID == "" {
			addFailure(&failures, "PROFILE_REGISTRY_INVALID", "profile registry contains an empty or duplicate profile")
		}
		data.profiles[profile.ID] = profile
	}
	if data.suite.SpecVersion != fixture.SpecVersion || data.suite.FixtureVersion != fixture.FixtureVersion ||
		data.suite.ProfileRegistryVersion != fixture.ProfileRegistryVersion || data.suite.RunnerProtocol != fixture.RunnerProtocol ||
		data.suite.ReportProtocol != fixture.ReportProtocol || data.registry.RegistryVersion != fixture.ProfileRegistryVersion ||
		data.registry.ReportSchemaSHA256 != data.schemaDigest || data.matrix.ProfileRegistryVersion != fixture.ProfileRegistryVersion ||
		data.matrix.SpecVersion != fixture.SpecVersion || data.matrix.FixtureVersion != fixture.FixtureVersion {
		addFailure(&failures, "VERSION_SKEW", "release-gate, suite, registry, matrix, or report-schema revisions differ")
	}
	digestSkew := data.schemaDigest != fixture.ProfileReportSchemaSHA256
	for _, pinned := range []struct {
		path   string
		sha256 string
	}{
		{fixture.AggregateReportSchema, fixture.AggregateReportSchemaSHA256},
		{fixture.ReleaseManifestSchema, fixture.ReleaseManifestSchemaSHA256},
		{fixture.IndependentRunner.Source, fixture.IndependentRunner.SourceSHA256},
	} {
		content, readErr := os.ReadFile(filepath.Join(conformanceRoot, pinned.path))
		if readErr != nil || digest(content) != pinned.sha256 {
			digestSkew = true
		}
	}
	if digestSkew {
		addFailure(&failures, "GATE_SCHEMA_SKEW", "profile, aggregate, manifest, or independent-runner bytes differ from the gate fixture")
	}
	if data.matrix.Profile != "suite.compatibility-1" || data.matrix.ReleaseStatus != "awaiting-complete-evidence" ||
		data.matrix.ExecutionOwnerIssue != 69 || data.matrix.PublicationOwnerIssue != 69 {
		addFailure(&failures, "COMPATIBILITY_MATRIX_INVALID", "compatibility matrix is not the unevidenced issue 69 inventory")
	}
	if !sameSet(data.suite.Languages, []string{"go", "javascript-typescript", "php", "python", "rust", "jvm", "dotnet", "swift", "dart", "ruby"}) {
		addFailure(&failures, "LANGUAGE_INVENTORY_INCOMPLETE", "suite must enumerate all ten official languages")
	}
	for _, pinned := range []struct {
		path string
		file string
	}{
		{path: fixture.ProfileRegistry, file: filepath.Join(conformanceRoot, fixture.ProfileRegistry)},
		{path: fixture.CompatibilityMatrix, file: filepath.Join(conformanceRoot, fixture.CompatibilityMatrix)},
		{path: "v1/release-gate.json", file: fixturePath},
	} {
		if !suitePinsFile(data.suite, pinned.path, pinned.file) {
			addFailure(&failures, "FIXTURE_DIGEST_MISMATCH", pinned.path+" is not pinned to its exact bytes by the suite")
		}
	}
	if len(failures) > 0 {
		return nil, sortedFailures(failures)
	}
	return data, nil
}

func loadProfileReport(data *gateData, path string) (*loadedReport, []Failure) {
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(data.root, path)
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return nil, []Failure{{Code: "REPORT_MISSING", Message: path + ": " + err.Error()}}
	}
	var value profileReport
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, []Failure{{Code: "REPORT_INVALID", Message: path + ": " + err.Error()}}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, []Failure{{Code: "REPORT_INVALID", Message: path + ": trailing JSON content"}}
	}
	if containsPublishUnsafeValue(value) {
		return nil, []Failure{{Code: "REPORT_UNSAFE", Message: path + ": report contains credential-shaped metadata"}}
	}
	if !validProfileReportShape(value) {
		return nil, []Failure{{Code: "REPORT_INVALID", Message: path + ": report violates the bounded profile-report schema"}}
	}
	relative, err := safeRelative(data.root, absolute)
	if err != nil {
		return nil, []Failure{{Code: "REPORT_PATH_INVALID", Message: path + ": " + err.Error()}}
	}
	loaded := &loadedReport{path: relative, evidence: Evidence{Path: relative, SHA256: digest(content)}, report: value, results: map[string]result{}, claims: map[string]claim{}}
	failures := validateProfileReport(data, loaded)
	return loaded, failures
}

func validateProfileReport(data *gateData, loaded *loadedReport) []Failure {
	report := &loaded.report
	var failures []Failure
	prefix := loaded.path + ": "
	if report.Protocol != data.registry.ReportProtocol || report.Versions.Spec != data.registry.SpecVersion ||
		report.Versions.Fixtures != data.registry.FixtureVersion || report.Versions.Profiles != data.registry.RegistryVersion ||
		report.Versions.RunnerProtocol != data.registry.RunnerProtocol || report.Versions.Canonicalization != "c14n-1" ||
		report.Versions.SchemaRevision != data.schemaDigest {
		addFailure(&failures, "VERSION_SKEW", prefix+"report versions do not match the gate")
	}
	if report.Implementation.SpecVersion != report.Versions.Spec || report.Implementation.FixtureVersion != report.Versions.Fixtures ||
		report.Implementation.ProfileVersion != report.Versions.Profiles || report.Implementation.SchemaRevision != report.Versions.SchemaRevision {
		addFailure(&failures, "VERSION_SKEW", prefix+"implementation versions do not match report versions")
	}
	if !validateRevisions(report.Revisions, report.Versions) {
		addFailure(&failures, "REVISION_BINDING_INVALID", prefix+"specification, schema, canonicalization, fixture, generator, runtime, SDK, or transport revision is incomplete")
	}
	if report.Implementation.Name == "" || !slices.Contains(data.suite.Languages, report.Implementation.Language) ||
		report.Implementation.Version == "" || report.Implementation.RuntimeVersion == "" ||
		!slices.Contains([]string{"linux", "darwin", "windows", "other"}, report.Environment.OS) ||
		!slices.Contains([]string{"amd64", "arm64", "other"}, report.Environment.Architecture) ||
		report.Environment.FeatureFlags == nil || report.Environment.WireTransports == nil || report.Environment.StreamTransports == nil ||
		report.Environment.ScalarPrecision == nil || report.Environment.CancellationCapabilities == nil ||
		report.Claims == nil || report.Results == nil || report.Run.ID == "" || report.Run.Operator == "" || len(report.Run.Command) == 0 || len(report.Run.Artifacts) == 0 ||
		report.Path.Source.Kind == "" || report.Path.Source.Language == "" || report.Path.Destination.Kind == "" || report.Path.Destination.Language == "" {
		addFailure(&failures, "REPORT_METADATA_INCOMPLETE", prefix+"implementation, environment, command, or artifact identity is incomplete")
	}
	for _, argument := range report.Run.Command {
		if secretPattern.MatchString(argument) {
			addFailure(&failures, "REPORT_UNSAFE", prefix+"command contains credential-shaped metadata")
		}
	}
	artifactNames := make(map[string]struct{}, len(report.Run.Artifacts))
	for _, artifact := range report.Run.Artifacts {
		if artifact.Name == "" || !digestPattern.MatchString(artifact.SHA256) {
			addFailure(&failures, "REPORT_METADATA_INCOMPLETE", prefix+"run artifact is missing an exact digest")
		}
		if _, duplicate := artifactNames[artifact.Name]; duplicate {
			addFailure(&failures, "REPORT_INVALID", prefix+"run artifact names must be unique")
		}
		artifactNames[artifact.Name] = struct{}{}
	}
	for _, item := range report.Results {
		profile, known := data.profiles[item.Profile]
		if !known {
			addFailure(&failures, "UNKNOWN_PROFILE", prefix+"result references "+item.Profile)
			continue
		}
		if _, duplicate := loaded.results[item.Profile]; duplicate {
			addFailure(&failures, "DUPLICATE_RESULT", prefix+"duplicate result for "+item.Profile)
		}
		loaded.results[item.Profile] = item
		if item.Capabilities == nil || item.ExecutedClauses == nil || item.Evidence == nil || item.Diagnostics == nil {
			addFailure(&failures, "REPORT_METADATA_INCOMPLETE", prefix+item.Profile+" result omits a required collection")
		}
		if !uniqueNonEmptyStrings(item.Capabilities) || !uniqueNonEmptyStrings(item.ExecutedClauses) {
			addFailure(&failures, "REPORT_INVALID", prefix+item.Profile+" result contains duplicate capabilities or clauses")
		}
		if item.EvidenceRole != profile.EvidenceRole {
			addFailure(&failures, "EVIDENCE_ROLE_MISMATCH", prefix+item.Profile+" has the wrong evidence role")
		}
		failures = append(failures, validateResultStatus(prefix, profile, item)...)
	}
	for _, claimed := range report.Claims {
		if claimed.Capabilities == nil {
			addFailure(&failures, "REPORT_METADATA_INCOMPLETE", prefix+claimed.Profile+" claim omits capabilities")
		}
		if !uniqueNonEmptyStrings(claimed.Capabilities) {
			addFailure(&failures, "REPORT_INVALID", prefix+claimed.Profile+" claim contains duplicate capabilities")
		}
		if _, duplicate := loaded.claims[claimed.Profile]; duplicate {
			addFailure(&failures, "DUPLICATE_CLAIM", prefix+"duplicate claim for "+claimed.Profile)
			continue
		}
		loaded.claims[claimed.Profile] = claimed
		profile, known := data.profiles[claimed.Profile]
		item, executed := loaded.results[claimed.Profile]
		if !known || !executed || item.Status != "passed" {
			addFailure(&failures, "UNEXECUTED_CLAIM", prefix+claimed.Profile+" was claimed without a passing result")
			continue
		}
		if !sameSet(item.ExecutedClauses, profile.RequiredClauses) || fixtureEvidenceDiffers(item.Evidence, profile.RequiredFixtures) {
			addFailure(&failures, "PARTIAL_RUN", prefix+claimed.Profile+" lacks exact clauses or fixture evidence")
		}
		if !slices.ContainsFunc(profile.EligiblePaths, func(eligible ProfilePath) bool {
			return eligible.Source == report.Path.Source.Kind && eligible.Destination == report.Path.Destination.Kind
		}) {
			addFailure(&failures, "INVALID_EVIDENCE_PATH", prefix+claimed.Profile+" cannot be certified by this path")
		}
		for _, capability := range claimed.Capabilities {
			if findOptionalCapability(profile, capability) == nil || !slices.Contains(item.Capabilities, capability) {
				addFailure(&failures, "UNEXECUTED_CAPABILITY", prefix+claimed.Profile+" claims "+capability+" without execution")
			}
		}
	}
	return failures
}

func validateResultStatus(prefix string, profile profileDefinition, item result) []Failure {
	switch item.Status {
	case "passed":
		return nil
	case "unsupported":
		if item.Capability == "" || findOptionalCapability(profile, item.Capability) == nil {
			return []Failure{{Code: "INVALID_SKIP", Message: prefix + profile.ID + " uses unsupported for a required profile or fixture"}}
		}
		return nil
	case "invalid-skip":
		return []Failure{{Code: "INVALID_SKIP", Message: prefix + profile.ID + " contains an invalid skip"}}
	case "failed":
		return []Failure{{Code: "PROFILE_FAILED", Message: prefix + profile.ID + " failed"}}
	case "infrastructure-failure":
		return []Failure{{Code: "INFRASTRUCTURE_FAILURE", Message: prefix + profile.ID + " had an infrastructure failure"}}
	default:
		return []Failure{{Code: "REPORT_INVALID", Message: prefix + profile.ID + " has an unknown status"}}
	}
}

func validateRevisions(revisions revisionSet, versions struct {
	Spec             string `json:"spec"`
	Fixtures         string `json:"fixtures"`
	Profiles         string `json:"profiles"`
	RunnerProtocol   string `json:"runnerProtocol"`
	Canonicalization string `json:"canonicalization"`
	SchemaRevision   string `json:"schemaRevision"`
}) bool {
	if revisions.Specification.Version != versions.Spec || revisions.Fixtures.Version != versions.Fixtures ||
		revisions.Canonical.Version != versions.Canonicalization || revisions.Schema.SHA256 != versions.SchemaRevision {
		return false
	}
	for _, pinned := range []revision{revisions.Specification, revisions.Schema, revisions.Canonical, revisions.Fixtures} {
		if !identifierPattern.MatchString(pinned.Version) || !digestPattern.MatchString(pinned.SHA256) {
			return false
		}
	}
	for _, applicable := range []applicableRevision{revisions.Generator, revisions.Runtime, revisions.SDK, revisions.Transport} {
		switch applicable.Status {
		case "pinned":
			if !identifierPattern.MatchString(applicable.Version) || !digestPattern.MatchString(applicable.SHA256) || applicable.Reason != "" {
				return false
			}
		case "not-applicable":
			if applicable.Reason == "" || len(applicable.Reason) > 1024 || applicable.Version != "" || applicable.SHA256 != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateMatrixCoverage(data *gateData, reports []loadedReport) ([]MatrixResult, []RunnerPath, []Failure) {
	var matrix []MatrixResult
	var paths []RunnerPath
	var failures []Failure
	expected := make(map[string]struct{})
	for _, implementation := range data.matrix.Implementations {
		if implementation.Status != "planned" {
			addFailure(&failures, "PLANNED_INVENTORY_INVALID", implementation.Implementation+" is already advertised before release evidence")
		}
		for _, profileID := range implementation.PlannedProfiles {
			expected[implementation.Implementation+"\x00"+implementation.Language+"\x00"+profileID] = struct{}{}
			matches := matchingClaims(reports, implementation.Implementation, implementation.Language, profileID)
			if len(matches) != 1 {
				addFailure(&failures, "MATRIX_EVIDENCE_INCOMPLETE", fmt.Sprintf("%s %s requires exactly one report, got %d", implementation.Implementation, profileID, len(matches)))
				continue
			}
			loaded := matches[0]
			if loaded.report.Path.Source.Language != implementation.Language {
				addFailure(&failures, "INVALID_EVIDENCE_PATH", implementation.Implementation+" "+profileID+" source language does not match the implementation")
			}
			profile := data.profiles[profileID]
			if profileID == "sdk.client-1" && (loaded.report.Revisions.Generator.Status != "pinned" ||
				loaded.report.Revisions.Runtime.Status != "pinned" || loaded.report.Revisions.SDK.Status != "pinned" ||
				loaded.report.Revisions.Transport.Status != "pinned") {
				addFailure(&failures, "REVISION_BINDING_INVALID", implementation.Implementation+" client report lacks pinned generator, runtime, SDK, or transport revisions")
			}
			optional, optionalFailures := optionalStatuses(reports, loaded, profile)
			failures = append(failures, optionalFailures...)
			matrix = append(matrix, MatrixResult{Kind: "implementation", Implementation: implementation.Implementation, Language: implementation.Language,
				Profile: profileID, EvidenceRole: profile.EvidenceRole, Path: loaded.report.Path, Report: loaded.evidence, Status: "passed", OptionalCapabilities: optional})
			if profileID == "sdk.client-1" {
				if loaded.report.Path.Source.Kind != "sdk" || loaded.report.Path.Source.Language != implementation.Language ||
					loaded.report.Path.Destination.Language != "go" {
					addFailure(&failures, "SDK_TO_GO_PATH_MISSING", implementation.Language+" client report is not an SDK-to-Go path")
				} else {
					paths = append(paths, RunnerPath{Kind: "sdk-to-go", Language: implementation.Language, Profile: profileID, Report: loaded.evidence, Status: "passed"})
				}
			}
		}
	}
	for _, integration := range data.matrix.IntegrationPaths {
		expected[integration.Destination.Implementation+"\x00"+integration.Destination.Language+"\x00"+integration.Profile] = struct{}{}
		matches := matchingClaims(reports, integration.Destination.Implementation, integration.Destination.Language, integration.Profile)
		if len(matches) != 1 {
			addFailure(&failures, "WORKER_EVIDENCE_INCOMPLETE", fmt.Sprintf("%s requires exactly one remote-worker report, got %d", integration.Destination.Implementation, len(matches)))
			continue
		}
		loaded := matches[0]
		if loaded.report.Revisions.Runtime.Status != "pinned" || loaded.report.Revisions.Transport.Status != "pinned" {
			addFailure(&failures, "REVISION_BINDING_INVALID", integration.Destination.Implementation+" worker report lacks pinned runtime or transport revisions")
		}
		wantPath := ExecutionPath{Source: Endpoint{Kind: integration.Source.Kind, Language: integration.Source.Language}, Destination: Endpoint{Kind: integration.Destination.Kind, Language: integration.Destination.Language}}
		if loaded.report.Path != wantPath {
			addFailure(&failures, "WORKER_PATH_INVALID", integration.Destination.Implementation+" report is not the declared Go-gateway-to-worker path")
		}
		profile := data.profiles[integration.Profile]
		matrix = append(matrix, MatrixResult{Kind: "remote-worker", Implementation: integration.Destination.Implementation, Language: integration.Destination.Language,
			Profile: integration.Profile, EvidenceRole: profile.EvidenceRole, Path: loaded.report.Path, Report: loaded.evidence, Status: "passed", OptionalCapabilities: []OptionalCapability{}})
		paths = append(paths, RunnerPath{Kind: "gateway-to-worker", Language: integration.Destination.Language, Profile: integration.Profile, Report: loaded.evidence, Status: "passed"})
	}
	for index := range reports {
		loaded := &reports[index]
		used := false
		for profileID := range loaded.claims {
			_, used = expected[loaded.report.Implementation.Name+"\x00"+loaded.report.Implementation.Language+"\x00"+profileID]
			if used {
				break
			}
		}
		if !used {
			for profileID, item := range loaded.results {
				_, advertised := expected[loaded.report.Implementation.Name+"\x00"+loaded.report.Implementation.Language+"\x00"+profileID]
				if advertised && item.Status == "unsupported" && findOptionalCapability(data.profiles[profileID], item.Capability) != nil {
					used = true
					break
				}
			}
		}
		if !used {
			addFailure(&failures, "UNADVERTISED_REPORT", loaded.path+" does not provide evidence for an advertised matrix cell")
		}
	}
	return matrix, paths, failures
}

func validateImplementationRevisionConsistency(reports []loadedReport) []Failure {
	type identity struct {
		version   string
		runtime   applicableRevision
		generator applicableRevision
		sdk       applicableRevision
	}
	identities := make(map[string]identity)
	var failures []Failure
	for _, loaded := range reports {
		key := loaded.report.Implementation.Name + "\x00" + loaded.report.Implementation.Language
		current := identity{version: loaded.report.Implementation.Version, runtime: loaded.report.Revisions.Runtime, generator: loaded.report.Revisions.Generator, sdk: loaded.report.Revisions.SDK}
		previous, found := identities[key]
		if !found {
			identities[key] = current
			continue
		}
		if previous.version != current.version || previous.runtime != current.runtime ||
			previous.generator != current.generator || previous.sdk != current.sdk {
			addFailure(&failures, "IMPLEMENTATION_VERSION_SKEW", loaded.report.Implementation.Name+" reports inconsistent implementation, generator, runtime, or SDK revisions")
		}
	}
	return failures
}

func optionalStatuses(reports []loadedReport, passing *loadedReport, profile profileDefinition) ([]OptionalCapability, []Failure) {
	statuses := make([]OptionalCapability, 0, len(profile.OptionalCapabilities))
	var failures []Failure
	passed := passing.results[profile.ID]
	for _, optional := range profile.OptionalCapabilities {
		if slices.Contains(passed.Capabilities, optional.ID) {
			statuses = append(statuses, OptionalCapability{ID: optional.ID, Status: "passed"})
			continue
		}
		count := 0
		for index := range reports {
			candidate := &reports[index]
			if candidate.report.Implementation.Name != passing.report.Implementation.Name || candidate.report.Implementation.Language != passing.report.Implementation.Language {
				continue
			}
			if item, ok := candidate.results[profile.ID]; ok && item.Status == "unsupported" && item.Capability == optional.ID {
				count++
			}
		}
		if count != 1 {
			addFailure(&failures, "OPTIONAL_STATUS_MISSING", passing.report.Implementation.Name+" must report "+optional.ID+" exactly once as passed or unsupported")
			continue
		}
		statuses = append(statuses, OptionalCapability{ID: optional.ID, Status: "unsupported"})
	}
	return statuses, failures
}

func validateSharedVectors(data *gateData, reports []loadedReport) ([]SharedVector, []Failure) {
	clients := make(map[string]*loadedReport, len(data.suite.Languages))
	for index := range reports {
		loaded := &reports[index]
		if _, ok := loaded.claims["sdk.client-1"]; ok {
			clients[loaded.report.Implementation.Language] = loaded
		}
	}
	var vectors []SharedVector
	var failures []Failure
	for _, artifactName := range data.fixture.SharedVectorArtifacts {
		byDigest := make(map[string][]string)
		for _, language := range data.suite.Languages {
			loaded := clients[language]
			if loaded == nil {
				continue
			}
			artifact, found := findArtifact(loaded.report.Run.Artifacts, artifactName)
			if !found {
				addFailure(&failures, "SHARED_VECTOR_MISSING", language+" client report lacks "+artifactName)
				continue
			}
			byDigest[artifact.SHA256] = append(byDigest[artifact.SHA256], language)
		}
		if len(byDigest) != 1 {
			addFailure(&failures, "SHARED_VECTOR_MISMATCH", artifactName+" differs across advertised languages")
			continue
		}
		for value, languages := range byDigest {
			slices.Sort(languages)
			if len(languages) != len(data.suite.Languages) {
				addFailure(&failures, "SHARED_VECTOR_MISSING", artifactName+" is not reported by all ten languages")
				continue
			}
			vectors = append(vectors, SharedVector{Name: artifactName, SHA256: value, Languages: languages})
		}
	}
	slices.SortFunc(vectors, func(left, right SharedVector) int { return strings.Compare(left.Name, right.Name) })
	return vectors, failures
}

func validateRunnerReport(data *gateData, path, kind string) (*RunnerPath, Evidence, []Failure) {
	if path == "" {
		return nil, Evidence{}, []Failure{{Code: "RUNNER_EVIDENCE_MISSING", Message: kind + " runner report is required"}}
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(data.root, path)
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return nil, Evidence{}, []Failure{{Code: "RUNNER_EVIDENCE_MISSING", Message: path + ": " + err.Error()}}
	}
	var receipt runnerReport
	if err := json.Unmarshal(content, &receipt); err != nil {
		return nil, Evidence{}, []Failure{{Code: "RUNNER_EVIDENCE_INVALID", Message: path + ": " + err.Error()}}
	}
	relative, err := safeRelative(data.root, absolute)
	if err != nil {
		return nil, Evidence{}, []Failure{{Code: "RUNNER_EVIDENCE_INVALID", Message: err.Error()}}
	}
	evidence := Evidence{Path: relative, SHA256: digest(content)}
	profile := data.fixture.SecurityGate.Profile
	language := data.fixture.SecurityGate.Language
	if kind == "independent-non-go" {
		profile = data.fixture.IndependentRunner.Profile
		language = data.fixture.IndependentRunner.Language
	}
	var failures []Failure
	if receipt.Protocol != data.fixture.RunnerProtocol || receipt.FixtureVersion != data.fixture.FixtureVersion || receipt.Runner.Language != language ||
		receipt.Runner.Name == "" || receipt.Runner.Version == "" || receipt.Runner.Platform == "" {
		addFailure(&failures, "RUNNER_VERSION_SKEW", kind+" report has protocol, fixture, or language skew")
	}
	if kind == "independent-non-go" {
		if receipt.Runner.Name != data.fixture.IndependentRunner.Name || receipt.Runner.Language == "go" {
			addFailure(&failures, "INDEPENDENT_RUNNER_INVALID", "independent runner is missing or is implemented in Go")
		}
		source, readErr := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Join(data.root, "conformance", "v1", "release-gate.json"))), data.fixture.IndependentRunner.Source))
		if readErr != nil || regexp.MustCompile(`(?m)(internal/conformancerunner|cmd/naatre-conformance|\bgo run\b|\.go["'])`).Match(source) {
			addFailure(&failures, "INDEPENDENT_RUNNER_IMPORTS_GO", "independent runner imports or executes Go source/runtime")
		}
	}
	wantPath := ExecutionPath{Source: Endpoint{Kind: "gateway", Language: "go"}, Destination: Endpoint{Kind: "native-runtime", Language: "go"}}
	if kind == "independent-non-go" {
		wantPath = ExecutionPath{Source: Endpoint{Kind: "codec", Language: language}, Destination: Endpoint{Kind: "codec", Language: language}}
	}
	if receipt.Path != wantPath {
		addFailure(&failures, "RUNNER_PATH_INVALID", kind+" receipt does not identify the required execution path")
	}
	matched := 0
	for _, item := range receipt.Results {
		if item.Profile != profile {
			addFailure(&failures, "RUNNER_PROFILE_FAILED", kind+" receipt includes an unrequested profile "+item.Profile)
		} else if item.Status == "passed" {
			matched++
		} else {
			addFailure(&failures, "RUNNER_PROFILE_FAILED", kind+" did not pass "+profile)
		}
		if kind == "independent-non-go" && !sameSuiteEvidence(item.Evidence, data.suite.Files) {
			addFailure(&failures, "INDEPENDENT_RUNNER_PARTIAL", "independent runner did not consume the complete pinned suite")
		}
		if kind == "security-gate" && !hasPinnedRunnerEvidence(item.Evidence, data.suite.Files, "v1/security-gate.json") {
			addFailure(&failures, "SECURITY_GATE_PARTIAL", "security gate did not publish its exact pinned fixture evidence")
		}
	}
	if matched != 1 || len(receipt.Results) != 1 {
		addFailure(&failures, "RUNNER_PROFILE_FAILED", kind+" must contain exactly one passing "+profile+" result")
	}
	return &RunnerPath{Kind: kind, Language: language, Profile: profile, Report: evidence, Status: "passed"}, evidence, failures
}

func matchingClaims(reports []loadedReport, implementation, language, profile string) []*loadedReport {
	var matches []*loadedReport
	for index := range reports {
		candidate := &reports[index]
		if candidate.report.Implementation.Name == implementation && candidate.report.Implementation.Language == language {
			if _, ok := candidate.claims[profile]; ok {
				matches = append(matches, candidate)
			}
		}
	}
	return matches
}

func findArtifact(artifacts []namedArtifact, name string) (namedArtifact, bool) {
	for _, artifact := range artifacts {
		if artifact.Name == name && digestPattern.MatchString(artifact.SHA256) {
			return artifact, true
		}
	}
	return namedArtifact{}, false
}

func findOptionalCapability(profile profileDefinition, capability string) *optionalDefinition {
	for index := range profile.OptionalCapabilities {
		optional := &profile.OptionalCapabilities[index]
		if optional.ID == capability && !optional.Required {
			return optional
		}
	}
	return nil
}

func fixtureEvidenceDiffers(got []profileEvidence, want []requiredFixture) bool {
	if len(got) != len(want) {
		return true
	}
	for index := range want {
		if got[index].Fixture != want[index].Path || got[index].SHA256 != want[index].SHA256 {
			return true
		}
	}
	return false
}

func sameSuiteEvidence(got []profileEvidence, want []struct {
	Path    string `json:"path"`
	Profile string `json:"profile"`
	SHA256  string `json:"sha256"`
}) bool {
	if len(got) != len(want) {
		return false
	}
	actual := make(map[string]string, len(got))
	for _, item := range got {
		actual[item.Fixture] = item.SHA256
	}
	for _, item := range want {
		if actual[item.Path] != item.SHA256 {
			return false
		}
	}
	return true
}

func hasPinnedRunnerEvidence(got []profileEvidence, want []struct {
	Path    string `json:"path"`
	Profile string `json:"profile"`
	SHA256  string `json:"sha256"`
}, path string) bool {
	pinnedIndex := slices.IndexFunc(want, func(candidate struct {
		Path    string `json:"path"`
		Profile string `json:"profile"`
		SHA256  string `json:"sha256"`
	}) bool {
		return candidate.Path == path
	})
	return pinnedIndex >= 0 && slices.ContainsFunc(got, func(evidence profileEvidence) bool {
		return evidence.Fixture == path && evidence.SHA256 == want[pinnedIndex].SHA256
	})
}

func suitePinsFile(suite suiteManifest, path, absolute string) bool {
	content, err := os.ReadFile(absolute)
	if err != nil {
		return false
	}
	want := digest(content)
	for _, file := range suite.Files {
		if file.Path == path && file.SHA256 == want {
			return true
		}
	}
	return false
}

func sameSet(left, right []string) bool {
	left = slices.Clone(left)
	right = slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func uniqueNonEmptyStrings(values []string) bool {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	for index, value := range ordered {
		if value == "" || index > 0 && value == ordered[index-1] {
			return false
		}
	}
	return true
}

func containsPublishUnsafeValue(report profileReport) bool {
	content, err := json.Marshal(report)
	if err != nil {
		return true
	}
	var value any
	if json.Unmarshal(content, &value) != nil {
		return true
	}
	return unsafeJSONValue(value, "")
}

func unsafeJSONValue(value any, key string) bool {
	if unsafeKeyPattern.MatchString(key) {
		return true
	}
	switch typed := value.(type) {
	case string:
		return unsafeValuePattern.MatchString(typed) || len(typed) > 4096
	case []any:
		if len(typed) > 2048 {
			return true
		}
		for _, entry := range typed {
			if unsafeJSONValue(entry, "") {
				return true
			}
		}
	case map[string]any:
		if len(typed) > 128 {
			return true
		}
		for name, entry := range typed {
			if unsafeJSONValue(entry, name) {
				return true
			}
		}
	}
	return false
}

func validProfileReportShape(report profileReport) bool {
	if !identifierPattern.MatchString(report.Run.ID) || !identifierPattern.MatchString(report.Run.Operator) ||
		len(report.Run.Command) < 1 || len(report.Run.Command) > 32 || len(report.Run.Artifacts) < 1 || len(report.Run.Artifacts) > 128 ||
		len(report.Claims) > 128 || len(report.Results) > 128 || !identifierPattern.MatchString(report.Implementation.Name) ||
		!identifierPattern.MatchString(report.Implementation.Version) || !identifierPattern.MatchString(report.Implementation.RuntimeVersion) ||
		!identifierPattern.MatchString(report.Implementation.Language) || !identifierPattern.MatchString(report.Environment.OS) ||
		!identifierPattern.MatchString(report.Environment.Architecture) ||
		!validEndpoint(report.Path.Source) || !validEndpoint(report.Path.Destination) {
		return false
	}
	for _, argument := range report.Run.Command {
		if len(argument) < 1 || len(argument) > 512 {
			return false
		}
	}
	artifactNames := make([]string, 0, len(report.Run.Artifacts))
	for _, artifact := range report.Run.Artifacts {
		if !identifierPattern.MatchString(artifact.Name) || !digestPattern.MatchString(artifact.SHA256) {
			return false
		}
		artifactNames = append(artifactNames, artifact.Name)
	}
	if !uniqueNonEmptyStrings(artifactNames) || !validStringSet(report.Environment.FeatureFlags) || !validStringSet(report.Environment.WireTransports) ||
		!validStringSet(report.Environment.StreamTransports) || !validStringSet(report.Environment.ScalarPrecision) ||
		!validStringSet(report.Environment.CancellationCapabilities) {
		return false
	}
	for _, claimed := range report.Claims {
		if !identifierPattern.MatchString(claimed.Profile) || !validStringSet(claimed.Capabilities) {
			return false
		}
	}
	for _, item := range report.Results {
		if !identifierPattern.MatchString(item.Profile) || !validStringSet(item.Capabilities) || !validStringSet(item.ExecutedClauses) ||
			len(item.Evidence) > 256 || len(item.Diagnostics) > 128 {
			return false
		}
		if item.Capability != "" && !identifierPattern.MatchString(item.Capability) {
			return false
		}
		for _, evidence := range item.Evidence {
			if evidence.Fixture == "" || len(evidence.Fixture) > 1024 || !digestPattern.MatchString(evidence.SHA256) {
				return false
			}
		}
		for _, diagnostic := range item.Diagnostics {
			if !diagnosticPattern.MatchString(diagnostic.Code) || diagnostic.Message == "" || len(diagnostic.Message) > 1024 {
				return false
			}
		}
		if item.Skip != nil && (item.Skip.Fixture == "" || len(item.Skip.Fixture) > 1024 || item.Skip.Reason == "" || len(item.Skip.Reason) > 1024) {
			return false
		}
	}
	return true
}

func validEndpoint(endpoint Endpoint) bool {
	return slices.Contains([]string{"sdk", "codec", "gateway", "worker", "http-server", "native-runtime", "stream-transport", "persisted-store", "schema-tool", "federation-coordinator", "extension-host"}, endpoint.Kind) &&
		identifierPattern.MatchString(endpoint.Language)
}

func validStringSet(values []string) bool {
	if values == nil || len(values) > 1024 || !uniqueNonEmptyStrings(values) {
		return false
	}
	for _, value := range values {
		if !identifierPattern.MatchString(value) {
			return false
		}
	}
	return true
}

func compareMatrixResults(left, right MatrixResult) int {
	return strings.Compare(left.Kind+"\x00"+left.Language+"\x00"+left.Implementation+"\x00"+left.Profile,
		right.Kind+"\x00"+right.Language+"\x00"+right.Implementation+"\x00"+right.Profile)
}

func addFailure(failures *[]Failure, code, message string) {
	if code == "" || message == "" {
		panic("releasegate: empty failure code or message")
	}
	*failures = append(*failures, Failure{Code: code, Message: message})
}

func sortedFailures(failures []Failure) []Failure {
	slices.SortFunc(failures, func(left, right Failure) int {
		return strings.Compare(left.Code+"\x00"+left.Message, right.Code+"\x00"+right.Message)
	})
	return failures
}

func decodeFile(path string, target any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	decodeErr := json.Unmarshal(content, target)
	if decodeErr != nil {
		return fmt.Errorf("decode %s: %w", path, decodeErr)
	}
	return nil
}

func safeRelative(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence path escapes repository root")
	}
	return filepath.ToSlash(relative), nil
}

func digest(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}
