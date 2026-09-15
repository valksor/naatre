package conformancerunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

var requiredProfileIDs = []string{
	"release.stable-1",
	"runtime.execution-1",
	"runtime.extensions-1",
	"runtime.federation-1",
	"runtime.persisted-1",
	"schema.tooling-1",
	"sdk.client-1",
	"transport.http.server-1",
	"transport.streaming-1",
	"wire.codec-1",
	"worker.remote-1",
}

var requiredProfileFixtureClasses = []string{"cancellation", "limit", "malformed", "negative", "positive", "security"}

type suiteFixtureRef struct {
	Path string `json:"path"`
}

type suiteNormativeSource struct {
	CaseID      string            `json:"caseId"`
	Spec        string            `json:"spec"`
	Clauses     []string          `json:"clauses"`
	FixtureRefs []suiteFixtureRef `json:"fixtureRefs"`
}

type profileFixture struct {
	Path    string `json:"path"`
	Profile string `json:"profile"`
	SHA256  string `json:"sha256"`
}

type profilePath struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type profileCapability struct {
	ID       string `json:"id"`
	Fixture  string `json:"fixture"`
	Required bool   `json:"required"`
}

type requiredProfileEvidence struct {
	Implementation string `json:"implementation"`
	Language       string `json:"language"`
	Profile        string `json:"profile"`
}

type profileDefinition struct {
	ID                               string                    `json:"id"`
	Stability                        string                    `json:"stability"`
	EvidenceRole                     string                    `json:"evidenceRole"`
	RequiredNormativeSources         []string                  `json:"requiredNormativeSources"`
	RequiredSpecs                    []string                  `json:"requiredSpecs"`
	RequiredClauses                  []string                  `json:"requiredClauses"`
	RequiredFixtures                 []profileFixture          `json:"requiredFixtures"`
	RequiredFixtureClasses           []string                  `json:"requiredFixtureClasses"`
	OptionalCapabilities             []profileCapability       `json:"optionalCapabilities"`
	EligiblePaths                    []profilePath             `json:"eligiblePaths"`
	RequiredEvidence                 []requiredProfileEvidence `json:"requiredEvidence"`
	CertificationExecutionOwnerIssue int                       `json:"certificationExecutionOwnerIssue"`
	PublicationOwnerIssue            int                       `json:"publicationOwnerIssue"`
}

type profileRequirements struct {
	Specs    []string
	Clauses  []string
	Fixtures []profileFixture
}

type profileStatus struct {
	Status     string `json:"status"`
	Certifying bool   `json:"certifying"`
	Scope      string `json:"scope"`
}

type profileRegistry struct {
	Profile            string              `json:"profile"`
	RegistryVersion    string              `json:"registryVersion"`
	SpecVersion        string              `json:"specVersion"`
	FixtureVersion     string              `json:"fixtureVersion"`
	RunnerProtocol     string              `json:"runnerProtocol"`
	ReportProtocol     string              `json:"reportProtocol"`
	ReportSchema       string              `json:"reportSchema"`
	ReportSchemaSHA256 string              `json:"reportSchemaSHA256"`
	ResultStatuses     []profileStatus     `json:"resultStatuses"`
	Profiles           []profileDefinition `json:"profiles"`
}

type compatibilityImplementation struct {
	Language        string            `json:"language"`
	Status          string            `json:"status"`
	PlannedProfiles []string          `json:"plannedProfiles"`
	Claims          []json.RawMessage `json:"claims"`
	Reports         []json.RawMessage `json:"reports"`
}

type compatibilityEndpoint struct {
	Implementation string `json:"implementation"`
	Language       string `json:"language"`
	Kind           string `json:"kind"`
}

type compatibilityPath struct {
	Source              compatibilityEndpoint `json:"source"`
	Destination         compatibilityEndpoint `json:"destination"`
	Profile             string                `json:"profile"`
	Status              string                `json:"status"`
	ExecutionOwnerIssue int                   `json:"executionOwnerIssue"`
	Report              json.RawMessage       `json:"report"`
}

type compatibilityMatrix struct {
	Profile                string                        `json:"profile"`
	ProfileRegistryVersion string                        `json:"profileRegistryVersion"`
	SpecVersion            string                        `json:"specVersion"`
	FixtureVersion         string                        `json:"fixtureVersion"`
	ExecutionOwnerIssue    int                           `json:"executionOwnerIssue"`
	PublicationOwnerIssue  int                           `json:"publicationOwnerIssue"`
	ReleaseStatus          string                        `json:"releaseStatus"`
	IntegrationPaths       []compatibilityPath           `json:"integrationPaths"`
	Implementations        []compatibilityImplementation `json:"implementations"`
	ThirdParty             []json.RawMessage             `json:"thirdPartyImplementations"`
}

func (r *Runner) verifyProfileRegistry(_ context.Context, _ Request) Result {
	var registry profileRegistry
	_, registryEvidence, err := r.loadPinnedFixture("v1/profiles.json", &registry)
	if err != nil {
		return profileValidationFailure("PROFILE_REGISTRY_UNAVAILABLE", err)
	}
	var matrix compatibilityMatrix
	_, matrixEvidence, err := r.loadPinnedFixture("v1/compatibility.json", &matrix)
	if err != nil {
		return profileValidationFailure("COMPATIBILITY_MATRIX_UNAVAILABLE", err)
	}
	if err := r.validateProfileRegistry(registry, matrix); err != nil {
		return profileValidationFailure("PROFILE_REGISTRY_INVALID", err)
	}
	schemaPath, err := r.fixturePath(registry.ReportSchema)
	if err != nil {
		return profileValidationFailure("REPORT_SCHEMA_INVALID", err)
	}
	schema, err := os.ReadFile(schemaPath)
	if err != nil || !json.Valid(schema) {
		return profileValidationFailure("REPORT_SCHEMA_INVALID", err)
	}
	digest := sha256.Sum256(schema)
	schemaDigest := hex.EncodeToString(digest[:])
	if schemaDigest != registry.ReportSchemaSHA256 {
		return profileValidationFailure("REPORT_SCHEMA_DIGEST_MISMATCH", errors.New("report schema digest does not match the registry"))
	}
	result := emptyResult("suite.profiles-1", "passed", "")
	result.Capabilities = []string{"suite.profiles-1"}
	result.Evidence = []Evidence{
		registryEvidence,
		matrixEvidence,
		{Fixture: registry.ReportSchema, SHA256: schemaDigest},
	}
	return result
}

func (r *Runner) validateProfileRegistry(registry profileRegistry, matrix compatibilityMatrix) error {
	if registry.Profile != "suite.profiles-1" || registry.SpecVersion != r.manifest.SpecVersion ||
		registry.FixtureVersion != r.manifest.FixtureVersion || registry.RunnerProtocol != Protocol ||
		registry.ReportProtocol != ReportProtocol || registry.RegistryVersion != r.manifest.ProfileRegistryVersion {
		return errors.New("profile registry version skew")
	}
	statuses := make([]string, 0, len(registry.ResultStatuses))
	for _, status := range registry.ResultStatuses {
		statuses = append(statuses, status.Status)
		if status.Status == "unsupported" && status.Scope != "optional-capability-only" {
			return errors.New("unsupported status is not limited to optional capabilities")
		}
	}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []string{"failed", "infrastructure-failure", "invalid-skip", "passed", "unsupported"}) {
		return errors.New("profile result status vocabulary is incomplete")
	}
	if err := r.validateProfileDefinitions(registry.Profiles); err != nil {
		return err
	}
	return r.validateCompatibilityMatrix(registry.RegistryVersion, matrix)
}

func (r *Runner) validateProfileDefinitions(profiles []profileDefinition) error {
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if err := r.validateProfileDefinition(profile); err != nil {
			return err
		}
		ids = append(ids, profile.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, requiredProfileIDs) {
		return errors.New("profile inventory is incomplete")
	}
	stable := profiles[slices.IndexFunc(profiles, func(profile profileDefinition) bool { return profile.ID == "release.stable-1" })]
	return validateStableRelease(stable)
}

func (r *Runner) validateProfileDefinition(profile profileDefinition) error {
	if profile.ID == "" || profile.EvidenceRole == "" || (profile.Stability != "stable" && profile.Stability != "experimental") {
		return fmt.Errorf("incomplete profile %q", profile.ID)
	}
	required, err := r.profileRequirements(profile)
	if err != nil {
		return err
	}
	if !slices.Equal(profile.RequiredSpecs, required.Specs) {
		return fmt.Errorf("profile %s has incomplete normative sources", profile.ID)
	}
	if !slices.Equal(profile.RequiredClauses, required.Clauses) {
		return fmt.Errorf("profile %s has incomplete normative clauses", profile.ID)
	}
	if !slices.Equal(profile.RequiredFixtures, required.Fixtures) {
		return fmt.Errorf("profile %s has incomplete fixtures", profile.ID)
	}
	classes := slices.Clone(profile.RequiredFixtureClasses)
	slices.Sort(classes)
	if !slices.Equal(classes, requiredProfileFixtureClasses) {
		return fmt.Errorf("profile %s has incomplete fixture classes", profile.ID)
	}
	return validateOptionalCapabilities(profile)
}

func (r *Runner) profileRequirements(profile profileDefinition) (profileRequirements, error) {
	sourceIDs := make(map[string]bool, len(profile.RequiredNormativeSources))
	for _, sourceID := range profile.RequiredNormativeSources {
		if sourceIDs[sourceID] {
			return profileRequirements{}, fmt.Errorf("profile %s has duplicate normative source %s", profile.ID, sourceID)
		}
		sourceIDs[sourceID] = true
	}
	if len(sourceIDs) == 0 {
		return profileRequirements{}, fmt.Errorf("profile %s has no normative sources", profile.ID)
	}

	required := profileRequirements{}
	paths := make([]string, 0)
	for _, source := range r.manifest.NormativeSources {
		if !sourceIDs[source.CaseID] {
			continue
		}
		delete(sourceIDs, source.CaseID)
		required.Specs = append(required.Specs, source.Spec)
		required.Clauses = append(required.Clauses, source.Clauses...)
		for _, reference := range source.FixtureRefs {
			if reference.Path != "v1/suite.json" {
				paths = append(paths, reference.Path)
			}
		}
	}
	if len(sourceIDs) != 0 {
		return profileRequirements{}, fmt.Errorf("profile %s references unknown normative sources", profile.ID)
	}
	slices.Sort(required.Specs)
	required.Specs = slices.Compact(required.Specs)
	slices.Sort(required.Clauses)
	required.Clauses = slices.Compact(required.Clauses)
	fixtures, err := r.resolveProfileFixtures(profile.ID, paths)
	if err != nil {
		return profileRequirements{}, err
	}
	required.Fixtures = fixtures
	return required, nil
}

func (r *Runner) resolveProfileFixtures(profileID string, paths []string) ([]profileFixture, error) {
	slices.Sort(paths)
	fixtures := make([]profileFixture, 0, len(paths))
	for _, path := range slices.Compact(paths) {
		index := slices.IndexFunc(r.manifest.Files, func(file fixtureFile) bool { return file.Path == path })
		if index < 0 {
			return nil, fmt.Errorf("profile %s references unpinned fixture %s", profileID, path)
		}
		file := r.manifest.Files[index]
		fixtures = append(fixtures, profileFixture(file))
	}
	return fixtures, nil
}

func validateOptionalCapabilities(profile profileDefinition) error {
	for _, capability := range profile.OptionalCapabilities {
		pinned := slices.ContainsFunc(profile.RequiredFixtures, func(fixture profileFixture) bool { return fixture.Path == capability.Fixture })
		if capability.Required || !pinned {
			return fmt.Errorf("profile %s has invalid optional capability %s", profile.ID, capability.ID)
		}
	}
	return nil
}

func validateStableRelease(profile profileDefinition) error {
	if profile.CertificationExecutionOwnerIssue != 69 || profile.PublicationOwnerIssue != 69 ||
		!hasRequiredEvidence(profile, "go", "runtime.execution-1") ||
		!hasRequiredEvidence(profile, "javascript-typescript", "sdk.client-1") ||
		!hasRequiredEvidence(profile, "go", securityGateProfile) {
		return errors.New("stable release evidence or ownership is incomplete")
	}
	return nil
}

func hasRequiredEvidence(profile profileDefinition, language, requiredProfile string) bool {
	return slices.ContainsFunc(profile.RequiredEvidence, func(evidence requiredProfileEvidence) bool {
		return evidence.Language == language && evidence.Profile == requiredProfile
	})
}

func (r *Runner) validateCompatibilityMatrix(registryVersion string, matrix compatibilityMatrix) error {
	if matrix.Profile != "suite.compatibility-1" || matrix.ProfileRegistryVersion != registryVersion ||
		matrix.SpecVersion != r.manifest.SpecVersion || matrix.FixtureVersion != r.manifest.FixtureVersion ||
		matrix.ExecutionOwnerIssue != 69 || matrix.PublicationOwnerIssue != 69 ||
		matrix.ReleaseStatus != "awaiting-complete-evidence" || len(matrix.ThirdParty) != 0 {
		return errors.New("compatibility matrix metadata is invalid")
	}
	workerLanguages := make([]string, 0, len(matrix.IntegrationPaths))
	for _, path := range matrix.IntegrationPaths {
		if path.Source.Implementation != "naatre-go" || path.Source.Language != "go" || path.Source.Kind != "gateway" ||
			path.Destination.Kind != "worker" || !strings.HasSuffix(path.Destination.Implementation, "-worker") || path.Profile != "worker.remote-1" ||
			path.Status != "planned" || path.ExecutionOwnerIssue != 69 || string(path.Report) != "null" {
			return errors.New("remote-worker integration path is invalid")
		}
		workerLanguages = append(workerLanguages, path.Destination.Language)
	}
	slices.Sort(workerLanguages)
	if !slices.Equal(workerLanguages, []string{"javascript-typescript", "php", "python", "rust"}) {
		return errors.New("remote-worker integration path inventory is incomplete")
	}
	languages := make([]string, 0, len(matrix.Implementations))
	for _, implementation := range matrix.Implementations {
		if implementation.Status != "planned" || slices.Contains(implementation.PlannedProfiles, "worker.remote-1") ||
			len(implementation.Claims) != 0 || len(implementation.Reports) != 0 {
			return fmt.Errorf("implementation %s is advertised without evidence", implementation.Language)
		}
		languages = append(languages, implementation.Language)
	}
	slices.Sort(languages)
	want := slices.Clone(r.manifest.Languages)
	slices.Sort(want)
	if !slices.Equal(languages, want) {
		return errors.New("compatibility matrix language inventory is incomplete")
	}
	return nil
}

func profileValidationFailure(code string, cause error) Result {
	result := failureResult("suite.profiles-1", code, "")
	if cause != nil {
		result.Diagnostics[0].Message = "conformance profile validation failed: " + cause.Error()
	}
	return result
}
