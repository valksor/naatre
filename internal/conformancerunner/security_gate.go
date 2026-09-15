package conformancerunner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const securityGateProfile = "suite.security-gate-1"

var securityGateClasses = []string{"positive", "negative", "adversarial", "cancellation", "exhaustion"}

var securityGateSurfaces = []string{
	"plan-cache",
	"request-batching",
	"normalized-cache",
	"persisted-operations",
	"streaming-replay",
	"secure-handles",
	"large-values",
	"webhooks",
	"remote-worker",
	"lifecycle",
}

var securityGateVectors = []string{
	"same-tenant-authorized",
	"authorization-deny-default",
	"tenant-crossing",
	"identity-expired",
	"identity-revoked",
	"policy-revision-changed",
	"cancel-before-work",
	"aggregate-exhaustion",
}

type securityGateFixture struct {
	Profile                string `json:"profile"`
	GateVersion            string `json:"gateVersion"`
	FixtureVersion         string `json:"fixtureVersion"`
	ProfileRegistryVersion string `json:"profileRegistryVersion"`
	Policies               struct {
		Security  evidenceFile `json:"security"`
		Resources evidenceFile `json:"resources"`
	} `json:"policies"`
	Implementation struct {
		Module   string         `json:"module"`
		Revision string         `json:"revision"`
		Files    []evidenceFile `json:"files"`
	} `json:"implementation"`
	RequiredClasses  []string              `json:"requiredClasses"`
	RequiredSurfaces []string              `json:"requiredSurfaces"`
	Vectors          []securityGateVector  `json:"vectors"`
	Profiles         []securityGateSurface `json:"profiles"`
	Release          struct {
		ConsumerProfile         string `json:"consumerProfile"`
		ExecutionOwnerIssue     int    `json:"executionOwnerIssue"`
		RequiredEvidenceProfile string `json:"requiredEvidenceProfile"`
	} `json:"release"`
}

type securityGateSurface struct {
	Profile           string         `json:"profile"`
	Surface           string         `json:"surface"`
	Status            string         `json:"status"`
	Fixture           evidenceFile   `json:"fixture"`
	Implementation    []evidenceFile `json:"implementation"`
	Classes           []string       `json:"classes"`
	BudgetDimensions  []string       `json:"budgetDimensions"`
	ProtectedMetadata []string       `json:"protectedMetadata"`
	Unsupported       []string       `json:"unsupported"`
	Reason            string         `json:"reason"`
}

type securityGateVector struct {
	Name          string `json:"name"`
	Class         string `json:"class"`
	Authorization struct {
		Present bool `json:"present"`
		Allowed bool `json:"allowed"`
	} `json:"authorization"`
	Identity struct {
		TenantMatches   bool `json:"tenantMatches"`
		CurrentRevision bool `json:"currentRevision"`
		Expired         bool `json:"expired"`
		Revoked         bool `json:"revoked"`
	} `json:"identity"`
	Cancelled    bool     `json:"cancelled"`
	HiddenInputs []string `json:"hiddenInputs"`
	Budget       struct {
		Limit    int `json:"limit"`
		Attempts int `json:"attempts"`
	} `json:"budget"`
	Expected securityGateOutcome `json:"expected"`
}

type securityGateOutcome struct {
	HandlerStarts             int    `json:"handlerStarts"`
	ProviderStarts            int    `json:"providerStarts"`
	Deliveries                int    `json:"deliveries"`
	Code                      string `json:"code"`
	Message                   string `json:"message"`
	FailureMetadataObservable bool   `json:"failureMetadataObservable"`
	HiddenIdentifiers         bool   `json:"hiddenIdentifiers"`
	Bounded                   bool   `json:"bounded"`
}

func (r *Runner) verifySecurityGate(_ context.Context, _ Request) Result {
	fixture, fixtureEvidence, loadErr := loadProfileFixture(r, "v1/security-gate.json", func(fixture securityGateFixture) bool {
		return fixture.valid(r.manifest.FixtureVersion, r.manifest.ProfileRegistryVersion)
	})
	evidence := []Evidence(nil)
	failureCode := "SECURITY_GATE_FIXTURE_INVALID"
	if loadErr == nil {
		evidence, failureCode = r.assessSecurityGate(fixture)
	}
	if failureCode != "" {
		return failureResult(securityGateProfile, failureCode, "v1/security-gate.json")
	}
	return passedSecurityGateResult(fixtureEvidence, evidence)
}

func (fixture securityGateFixture) valid(fixtureVersion, registryVersion string) bool {
	return fixture.Profile == securityGateProfile && fixture.GateVersion == "1.0.0" &&
		fixture.FixtureVersion == fixtureVersion && fixture.ProfileRegistryVersion == registryVersion &&
		fixture.Implementation.Module == "github.com/valksor/naatre" &&
		fixture.Implementation.Revision == "naatre-go-security-gate-1"
}

func (r *Runner) assessSecurityGate(fixture securityGateFixture) ([]Evidence, string) {
	evidence, err := r.verifySecurityGateEvidence(fixture)
	switch {
	case err != nil:
		return nil, "SECURITY_GATE_EVIDENCE_MISMATCH"
	case validateSecurityGate(fixture) != nil:
		return nil, "SECURITY_GATE_EXECUTION_FAILED"
	default:
		return evidence, ""
	}
}

func (r *Runner) verifySecurityGateEvidence(fixture securityGateFixture) ([]Evidence, error) {
	files := []evidenceFile{fixture.Policies.Security, fixture.Policies.Resources}
	files = append(files, fixture.Implementation.Files...)
	for _, profile := range fixture.Profiles {
		files = append(files, profile.Fixture)
		files = append(files, profile.Implementation...)
	}
	evidence, _, err := r.verifyEvidenceFiles(files)
	return evidence, err
}

func validateSecurityGate(fixture securityGateFixture) error {
	checks := []func() error{
		func() error { return validateSecurityGateInventory(fixture) },
		func() error { return validateSecurityGateVectors(fixture.Vectors) },
		func() error { return validateSecurityGateProfiles(fixture.Profiles, fixture.Vectors) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func validateSecurityGateInventory(fixture securityGateFixture) error {
	if !slices.Equal(fixture.RequiredClasses, securityGateClasses) {
		return errors.New("security gate fixture classes are incomplete")
	}
	if !slices.Equal(fixture.RequiredSurfaces, securityGateSurfaces) {
		return errors.New("security gate surface inventory is incomplete")
	}
	if fixture.Release.ConsumerProfile != "release.stable-1" || fixture.Release.ExecutionOwnerIssue != 69 ||
		fixture.Release.RequiredEvidenceProfile != securityGateProfile {
		return errors.New("stable release does not consume the security gate")
	}
	return nil
}

func validateSecurityGateVectors(vectors []securityGateVector) error {
	vectorNames := make([]string, 0, len(vectors))
	for _, vector := range vectors {
		vectorNames = append(vectorNames, vector.Name)
		if err := validateSecurityGateVector(vector); err != nil {
			return err
		}
	}
	if !slices.Equal(vectorNames, securityGateVectors) {
		return errors.New("security gate vectors are incomplete")
	}
	return nil
}

func validateSecurityGateVector(vector securityGateVector) error {
	if !slices.Contains(securityGateClasses, vector.Class) {
		return fmt.Errorf("security gate vector %s has an unknown class", vector.Name)
	}
	if vector.Budget.Limit <= 0 || vector.Budget.Attempts <= 0 {
		return fmt.Errorf("security gate vector %s has an invalid aggregate budget", vector.Name)
	}
	if vector.Expected.FailureMetadataObservable || vector.Expected.HiddenIdentifiers || !vector.Expected.Bounded {
		return fmt.Errorf("security gate vector %s permits an unsafe failure", vector.Name)
	}
	switch vector.Class {
	case "positive":
		if vector.Expected.Code != "" || vector.Expected.Deliveries != vector.Budget.Attempts {
			return fmt.Errorf("security gate vector %s has an invalid positive outcome", vector.Name)
		}
	case "negative", "adversarial", "cancellation":
		if vector.Expected.HandlerStarts != 0 || vector.Expected.ProviderStarts != 0 || vector.Expected.Deliveries != 0 || vector.Expected.Code == "" {
			return fmt.Errorf("security gate vector %s starts protected work before admission", vector.Name)
		}
	case "exhaustion":
		if vector.Budget.Attempts <= vector.Budget.Limit || vector.Expected.HandlerStarts > vector.Budget.Limit || vector.Expected.Code != "RESOURCE_EXHAUSTED" {
			return fmt.Errorf("security gate vector %s does not enforce its aggregate budget", vector.Name)
		}
	}
	return nil
}

func validateSecurityGateProfiles(profiles []securityGateSurface, vectors []securityGateVector) error {
	seenProfiles := make(map[string]bool, len(profiles))
	seenSurfaces := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Profile == "" || seenProfiles[profile.Profile] {
			return errors.New("security gate profile identities must be unique")
		}
		seenProfiles[profile.Profile] = true
		seenSurfaces = append(seenSurfaces, profile.Surface)
		if err := validateSecurityGateProfile(profile, vectors); err != nil {
			return err
		}
	}
	if !slices.Equal(seenSurfaces, securityGateSurfaces) {
		return errors.New("security gate profiles do not cover every surface")
	}
	return nil
}

func validateSecurityGateProfile(profile securityGateSurface, vectors []securityGateVector) error {
	if !slices.Equal(profile.Classes, securityGateClasses) || len(profile.BudgetDimensions) == 0 || len(profile.ProtectedMetadata) == 0 {
		return fmt.Errorf("security gate profile %s has incomplete class or budget coverage", profile.Profile)
	}
	switch profile.Status {
	case "supported":
		if profile.Fixture.Path == "" || profile.Fixture.SHA256 == "" || len(profile.Implementation) == 0 || profile.Reason != "" {
			return fmt.Errorf("supported security gate profile %s has incomplete evidence", profile.Profile)
		}
		for _, vector := range vectors {
			if actual := executeSecurityGateVector(profile, vector); actual != vector.Expected {
				return fmt.Errorf("security gate profile %s vector %s: got %#v, want %#v", profile.Profile, vector.Name, actual, vector.Expected)
			}
		}
	case "unsupported":
		if profile.Reason == "" || len(profile.Implementation) != 0 {
			return fmt.Errorf("unsupported security gate profile %s is not explicit", profile.Profile)
		}
	default:
		return fmt.Errorf("security gate profile %s has invalid status", profile.Profile)
	}
	return nil
}

func passedSecurityGateResult(fixture Evidence, evidence []Evidence) Result {
	result := emptyResult(securityGateProfile, "passed", "")
	result.Capabilities = append(result.Capabilities, securityGateProfile)
	result.Evidence = append(result.Evidence, fixture)
	result.Evidence = append(result.Evidence, evidence...)
	return result
}

func executeSecurityGateVector(profile securityGateSurface, vector securityGateVector) securityGateOutcome {
	outcome := securityGateOutcome{Bounded: true}
	fail := func(code, message string) securityGateOutcome {
		outcome.Code = code
		outcome.Message = message
		for _, hidden := range append(slices.Clone(profile.ProtectedMetadata), vector.HiddenInputs...) {
			if strings.Contains(code, hidden) || strings.Contains(message, hidden) {
				outcome.HiddenIdentifiers = true
			}
		}
		return outcome
	}
	if vector.Cancelled {
		return fail("CANCELLED", "request cancelled")
	}
	identityAllowed := vector.Authorization.Present && vector.Authorization.Allowed &&
		vector.Identity.TenantMatches && vector.Identity.CurrentRevision &&
		!vector.Identity.Expired && !vector.Identity.Revoked
	if !identityAllowed {
		return fail("UNAUTHORIZED", "request is not authorized")
	}
	if vector.Budget.Limit <= 0 || vector.Budget.Attempts <= 0 {
		return fail("RESOURCE_EXHAUSTED", "resource budget exhausted")
	}
	for range vector.Budget.Attempts {
		if outcome.HandlerStarts >= vector.Budget.Limit {
			return fail("RESOURCE_EXHAUSTED", "resource budget exhausted")
		}
		outcome.HandlerStarts++
		outcome.ProviderStarts++
		outcome.Deliveries++
	}
	return outcome
}
