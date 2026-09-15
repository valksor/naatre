package conformance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

type governanceFixture struct {
	Profile    string `json:"profile"`
	Version    string `json:"version"`
	OwnerIssue int    `json:"ownerIssue"`
	Manifest   struct {
		Profile                            string   `json:"profile"`
		Schema                             string   `json:"schema"`
		Example                            string   `json:"example"`
		RequiredComponents                 []string `json:"requiredComponents"`
		StatusValues                       []string `json:"statusValues"`
		SupportStatuses                    []string `json:"supportStatuses"`
		ImplementedEvidenceRequired        bool     `json:"implementedEvidenceRequired"`
		ConformanceReportRequiredForClaims bool     `json:"conformanceReportRequiredForClaims"`
	} `json:"manifest"`
	Governance struct {
		InterimOwner                            string   `json:"interimOwner"`
		PublicDecisionRequired                  bool     `json:"publicDecisionRequired"`
		NormativeApprovalsAfterSecondMaintainer int      `json:"normativeApprovalsAfterSecondMaintainer"`
		InterimDecisionFields                   []string `json:"interimDecisionFields"`
		DecisionStatuses                        []string `json:"decisionStatuses"`
		SecurityEmbargo                         struct {
			PublicIssueRequired                   bool `json:"publicIssueRequired"`
			PrivateRecordRequired                 bool `json:"privateRecordRequired"`
			MinimumParticipants                   bool `json:"minimumParticipants"`
			PostReleaseDisclosureRequiredWhenSafe bool `json:"postReleaseDisclosureRequiredWhenSafe"`
		} `json:"securityEmbargo"`
	} `json:"governance"`
	Versioning struct {
		Scheme                              string   `json:"scheme"`
		IndependentComponents               []string `json:"independentComponents"`
		CompatibilityClasses                []string `json:"compatibilityClasses"`
		V0BreakingMigrationGuidanceRequired bool     `json:"v0BreakingMigrationGuidanceRequired"`
		V1BreakingMajorRequired             bool     `json:"v1BreakingMajorRequired"`
		StableDeprecation                   struct {
			MinimumMinorReleases              int  `json:"minimumMinorReleases"`
			MinimumMonths                     int  `json:"minimumMonths"`
			SecurityExceptionRequiresDecision bool `json:"securityExceptionRequiresDecision"`
		} `json:"stableDeprecation"`
		RetiredNamesReservedForMajorLifetime bool     `json:"retiredNamesReservedForMajorLifetime"`
		ReservedKinds                        []string `json:"reservedKinds"`
	} `json:"versioning"`
	ExperimentalPromotion struct {
		ExplicitNamespaceOrProfile bool     `json:"explicitNamespaceOrProfile"`
		MayChangeV1Defaults        bool     `json:"mayChangeV1Defaults"`
		Requirements               []string `json:"requirements"`
	} `json:"experimentalPromotion"`
	SupplyChain struct {
		ArtifactEvidence                        []string `json:"artifactEvidence"`
		SignedWhereSupported                    bool     `json:"signedWhereSupported"`
		NotApplicableSignatureRequiresRationale bool     `json:"notApplicableSignatureRequiresRationale"`
		ReproducibleBuilds                      struct {
			CleanBuilds             int  `json:"cleanBuilds"`
			ByteIdenticalArtifacts  bool `json:"byteIdenticalArtifacts"`
			ByteIdenticalGeneration bool `json:"byteIdenticalGeneration"`
			PinnedToolchain         bool `json:"pinnedToolchain"`
		} `json:"reproducibleBuilds"`
		DependencyUpdates struct {
			LockfilesRequired                                  bool `json:"lockfilesRequired"`
			ReviewRequired                                     bool `json:"reviewRequired"`
			AutomationCannotApprove                            bool `json:"automationCannotApprove"`
			KnownExploitableDependencyRequiresSecurityDecision bool `json:"knownExploitableDependencyRequiresSecurityDecision"`
		} `json:"dependencyUpdates"`
		Revocation struct {
			SilentReplacementForbidden bool `json:"silentReplacementForbidden"`
			PublishAffectedDigests     bool `json:"publishAffectedDigests"`
			ReplacementReleaseRequired bool `json:"replacementReleaseRequired"`
			CredentialRotationRequired bool `json:"credentialRotationRequired"`
		} `json:"revocation"`
	} `json:"supplyChain"`
	ReleaseRecords struct {
		PinNormativeExternalReferences        bool `json:"pinNormativeExternalReferences"`
		PinGeneratedArtifacts                 bool `json:"pinGeneratedArtifacts"`
		PinThirdPartyNotices                  bool `json:"pinThirdPartyNotices"`
		PinDecisionStatus                     bool `json:"pinDecisionStatus"`
		CopiedCodeNoticesRequired             bool `json:"copiedCodeNoticesRequired"`
		BorrowedIdeasRequireCompatibilityMode bool `json:"borrowedIdeasRequireCompatibilityMode"`
	} `json:"releaseRecords"`
	SupportMatrix struct {
		IndependentRows              []string `json:"independentRows"`
		MaintainerRequired           bool     `json:"maintainerRequired"`
		OwnerIssueRequired           bool     `json:"ownerIssueRequired"`
		UnimplementedStatus          string   `json:"unimplementedStatus"`
		RepositoryPresenceIsEvidence bool     `json:"repositoryPresenceIsEvidence"`
	} `json:"supportMatrix"`
	Roadmap struct {
		Inventory           string `json:"inventory"`
		ComponentIssueRange struct {
			First int `json:"first"`
			Last  int `json:"last"`
		} `json:"componentIssueRange"`
		Coverage                  string   `json:"coverage"`
		AllowedCoverage           []string `json:"allowedCoverage"`
		ImplicitOmissionForbidden bool     `json:"implicitOmissionForbidden"`
		ScopeDecisionRequired     bool     `json:"scopeDecisionRequired"`
	} `json:"roadmap"`
	Dependencies []struct {
		Issue int    `json:"issue"`
		URL   string `json:"url"`
		Role  string `json:"role"`
	} `json:"dependencies"`
	ReleaseChecklist []string `json:"releaseChecklist"`
}

type releaseManifest struct {
	Profile string `json:"profile"`
	Release struct {
		ID             string   `json:"id"`
		Status         string   `json:"status"`
		SourceRevision string   `json:"sourceRevision"`
		Reproducible   bool     `json:"reproducible"`
		Revokes        []string `json:"revokes"`
	} `json:"release"`
	Components    map[string]json.RawMessage `json:"components"`
	Compatibility struct {
		Classification    string   `json:"classification"`
		Breaking          bool     `json:"breaking"`
		MigrationGuidance []string `json:"migrationGuidance"`
	} `json:"compatibility"`
	Support []struct {
		Surface    string            `json:"surface"`
		Maintainer string            `json:"maintainer"`
		Status     string            `json:"status"`
		OwnerIssue string            `json:"ownerIssue"`
		Evidence   []releaseEvidence `json:"evidence"`
	} `json:"support"`
	Artifacts []struct {
		Path       string          `json:"path"`
		SHA256     string          `json:"sha256"`
		SBOM       releaseEvidence `json:"sbom"`
		Provenance releaseEvidence `json:"provenance"`
		Signature  struct {
			Status    string `json:"status"`
			Bundle    string `json:"bundle"`
			Identity  string `json:"identity"`
			Rationale string `json:"rationale"`
		} `json:"signature"`
	} `json:"artifacts"`
	ConformanceClaims []struct {
		Profile string          `json:"profile"`
		Report  releaseEvidence `json:"report"`
	} `json:"conformanceClaims"`
	Decisions []json.RawMessage `json:"decisions"`
	Roadmap   struct {
		Inventory     string `json:"inventory"`
		Coverage      string `json:"coverage"`
		ScopeDecision string `json:"scopeDecision"`
	} `json:"roadmap"`
}

type releaseEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

var releaseSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var releaseRevision = regexp.MustCompile(`^[a-f0-9]{40,64}$`)

func TestGovernanceReleaseContract(t *testing.T) {
	t.Parallel()
	var fixture governanceFixture
	readFixture(t, "governance.json", &fixture)
	assertGovernanceIdentity(t, fixture)
	assertGovernanceEvolution(t, fixture)
	assertGovernanceSupplyChain(t, fixture)
	assertGovernanceSupportRoadmap(t, fixture)
	assertReleaseEvidenceGates(t, fixture.Manifest.Example)
	assertGovernanceDependencyLinks(t, fixture)
	assertGovernanceIssueMappings(t)
	assertGovernanceRoadmapStates(t)
}

func assertGovernanceIdentity(t *testing.T, fixture governanceFixture) {
	t.Helper()
	wantComponents := []string{"specification", "schemaModel", "canonicalization", "fixtures", "conformanceSuite", "goRuntime", "generators", "sdks", "workers"}
	if fixture.Profile != "governance.release-1" || fixture.Version != "1.0.0" || fixture.OwnerIssue != 49 ||
		fixture.Manifest.Profile != "naatre.release-manifest-1" || !reflect.DeepEqual(fixture.Manifest.RequiredComponents, wantComponents) ||
		!fixture.Manifest.ImplementedEvidenceRequired || !fixture.Manifest.ConformanceReportRequiredForClaims {
		t.Fatalf("release-manifest governance is incomplete: %#v", fixture.Manifest)
	}
	if fixture.Governance.InterimOwner != "@k0d3r1s" || !fixture.Governance.PublicDecisionRequired ||
		fixture.Governance.NormativeApprovalsAfterSecondMaintainer != 2 || fixture.Governance.SecurityEmbargo.PublicIssueRequired ||
		!fixture.Governance.SecurityEmbargo.PrivateRecordRequired {
		t.Fatalf("decision or security-embargo governance is incomplete: %#v", fixture.Governance)
	}
}

func assertGovernanceEvolution(t *testing.T, fixture governanceFixture) {
	t.Helper()
	if !fixture.Versioning.V0BreakingMigrationGuidanceRequired || !fixture.Versioning.V1BreakingMajorRequired ||
		!fixture.Versioning.RetiredNamesReservedForMajorLifetime || fixture.ExperimentalPromotion.MayChangeV1Defaults ||
		!fixture.ExperimentalPromotion.ExplicitNamespaceOrProfile {
		t.Fatalf("compatibility or promotion governance is incomplete")
	}
}

func assertGovernanceSupplyChain(t *testing.T, fixture governanceFixture) {
	t.Helper()
	if !reflect.DeepEqual(fixture.SupplyChain.ArtifactEvidence, []string{"sha256", "sbom", "provenance"}) ||
		!fixture.SupplyChain.SignedWhereSupported || fixture.SupplyChain.ReproducibleBuilds.CleanBuilds < 2 ||
		!fixture.SupplyChain.ReproducibleBuilds.ByteIdenticalArtifacts || !fixture.SupplyChain.ReproducibleBuilds.ByteIdenticalGeneration {
		t.Fatalf("supply-chain governance is incomplete: %#v", fixture.SupplyChain)
	}
}

func assertGovernanceSupportRoadmap(t *testing.T, fixture governanceFixture) {
	t.Helper()
	if !reflect.DeepEqual(fixture.SupportMatrix.IndependentRows, []string{"client", "remote-worker", "native-runtime"}) ||
		!fixture.SupportMatrix.MaintainerRequired || !fixture.SupportMatrix.OwnerIssueRequired || fixture.SupportMatrix.UnimplementedStatus != "planned" {
		t.Fatalf("support matrix does not preserve independent evidence roles")
	}
	if fixture.Roadmap.Coverage != "exactly-once" || !fixture.Roadmap.ImplicitOmissionForbidden || !fixture.Roadmap.ScopeDecisionRequired || len(fixture.ReleaseChecklist) != 10 {
		t.Fatalf("roadmap or release checklist is incomplete")
	}
}

func assertReleaseEvidenceGates(t *testing.T, examplePath string) {
	t.Helper()
	root := filepath.Join("..", "..")
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(examplePath)))
	if err != nil {
		t.Fatal(err)
	}
	var example releaseManifest
	if err := json.Unmarshal(content, &example); err != nil {
		t.Fatalf("decode example release manifest: %v", err)
	}
	if validationErr := validateReleaseManifest(example); validationErr != nil {
		t.Fatalf("example release manifest: %v", validationErr)
	}

	implementedWithoutEvidence := example
	implementedWithoutEvidence.Support = append([]struct {
		Surface    string            `json:"surface"`
		Maintainer string            `json:"maintainer"`
		Status     string            `json:"status"`
		OwnerIssue string            `json:"ownerIssue"`
		Evidence   []releaseEvidence `json:"evidence"`
	}{}, example.Support...)
	implementedWithoutEvidence.Support[0].Status = "implemented"
	if err := validateReleaseManifest(implementedWithoutEvidence); err == nil {
		t.Fatal("implemented support without evidence was accepted")
	}

	breakingWithoutGuidance := example
	breakingWithoutGuidance.Compatibility.Breaking = true
	breakingWithoutGuidance.Compatibility.Classification = "breaking"
	if err := validateReleaseManifest(breakingWithoutGuidance); err == nil {
		t.Fatal("breaking release without migration guidance was accepted")
	}

	publishedWithoutSupplyChain := example
	publishedWithoutSupplyChain.Release.Status = "published"
	if err := validateReleaseManifest(publishedWithoutSupplyChain); err == nil {
		t.Fatal("published release without artifacts and reports was accepted")
	}
}

func assertGovernanceDependencyLinks(t *testing.T, fixture governanceFixture) {
	t.Helper()
	wantDependencies := []int{1, 15, 36, 68}
	gotDependencies := make([]int, 0, len(fixture.Dependencies))
	for _, dependency := range fixture.Dependencies {
		gotDependencies = append(gotDependencies, dependency.Issue)
		wantURL := fmt.Sprintf("https://github.com/valksor/naatre/issues/%d", dependency.Issue)
		if dependency.URL != wantURL {
			t.Errorf("issue #%d dependency URL = %q, want %q", dependency.Issue, dependency.URL, wantURL)
		}
	}
	sort.Ints(gotDependencies)
	if !reflect.DeepEqual(gotDependencies, wantDependencies) {
		t.Fatalf("governance dependencies = %v, want %v", gotDependencies, wantDependencies)
	}
}

func assertGovernanceIssueMappings(t *testing.T) {
	t.Helper()
	var suite suiteManifest
	readFixture(t, "suite.json", &suite)
	counts := make(map[int]int, len(suite.IssueMappings))
	for _, mapping := range suite.IssueMappings {
		counts[mapping.Issue]++
	}
	for issue := 1; issue <= 110; issue++ {
		if counts[issue] != 1 {
			t.Errorf("roadmap coverage for issue #%d = %d, want exactly 1", issue, counts[issue])
		}
	}
	if len(counts) != 110 {
		t.Errorf("roadmap contains %d distinct issues, want 110", len(counts))
	}
}

func assertGovernanceRoadmapStates(t *testing.T) {
	t.Helper()
	var document map[string]json.RawMessage
	readFixture(t, "roadmap.json", &document)
	var issues map[string]struct {
		Coverage   string `json:"coverage"`
		OwnerIssue int    `json:"ownerIssue"`
		Execution  string `json:"execution"`
	}
	if err := json.Unmarshal(document["issues"], &issues); err != nil {
		t.Fatalf("decode roadmap issues: %v", err)
	}
	for key, issue := range issues {
		if issue.Coverage != "implemented-contract" && issue.Coverage != "deferred-to-owner" {
			t.Errorf("roadmap issue %s has implicit or unlabeled coverage %q", key, issue.Coverage)
		}
		if issue.OwnerIssue == 0 || issue.Execution == "" {
			t.Errorf("roadmap issue %s lacks explicit ownership or execution status", key)
		}
	}
}

func validateReleaseManifest(manifest releaseManifest) error {
	if err := validateReleaseIdentity(manifest); err != nil {
		return err
	}
	if err := validateReleaseSupport(manifest); err != nil {
		return err
	}
	if err := validateReleaseEvidence(manifest); err != nil {
		return err
	}
	return validateReleaseDecisions(manifest)
}

func validateReleaseIdentity(manifest releaseManifest) error {
	if manifest.Profile != "naatre.release-manifest-1" || !releaseRevision.MatchString(manifest.Release.SourceRevision) || !manifest.Release.Reproducible {
		return fmt.Errorf("invalid release identity or reproducibility")
	}
	for _, component := range []string{"specification", "schemaModel", "canonicalization", "fixtures", "conformanceSuite", "goRuntime", "generators", "sdks", "workers"} {
		if _, ok := manifest.Components[component]; !ok {
			return fmt.Errorf("missing exact %s version", component)
		}
	}
	if manifest.Compatibility.Breaking && (manifest.Compatibility.Classification != "breaking" || len(manifest.Compatibility.MigrationGuidance) == 0) {
		return fmt.Errorf("breaking release lacks classification or migration guidance")
	}
	return nil
}

func validateReleaseSupport(manifest releaseManifest) error {
	requiredSurfaces := map[string]bool{"client": false, "remote-worker": false, "native-runtime": false}
	for _, row := range manifest.Support {
		if _, ok := requiredSurfaces[row.Surface]; ok {
			requiredSurfaces[row.Surface] = true
		}
		if row.Maintainer == "" || row.OwnerIssue == "" {
			return fmt.Errorf("support row lacks maintainer or owner")
		}
		if row.Status == "implemented" && len(row.Evidence) == 0 {
			return fmt.Errorf("implemented %s lacks evidence", row.Surface)
		}
		if row.Status != "implemented" && len(row.Evidence) != 0 {
			return fmt.Errorf("non-implemented %s advertises evidence", row.Surface)
		}
	}
	for surface, present := range requiredSurfaces {
		if !present {
			return fmt.Errorf("missing independent %s support row", surface)
		}
	}
	return nil
}

func validateReleaseEvidence(manifest releaseManifest) error {
	if err := validateConformanceClaims(manifest); err != nil {
		return err
	}
	if err := validateReleaseArtifacts(manifest); err != nil {
		return err
	}
	if manifest.Release.Status == "published" && (len(manifest.Artifacts) == 0 || len(manifest.ConformanceClaims) == 0) {
		return fmt.Errorf("published release lacks supply-chain artifacts or conformance reports")
	}
	return nil
}

func validateConformanceClaims(manifest releaseManifest) error {
	for index := range manifest.ConformanceClaims {
		claim := &manifest.ConformanceClaims[index]
		if claim.Profile == "" || !validReleaseEvidence(claim.Report) {
			return fmt.Errorf("conformance claim lacks machine-readable report")
		}
	}
	return nil
}

func validateReleaseArtifacts(manifest releaseManifest) error {
	for _, artifact := range manifest.Artifacts {
		if artifact.Path == "" || !releaseSHA256.MatchString(artifact.SHA256) || !validReleaseEvidence(artifact.SBOM) || !validReleaseEvidence(artifact.Provenance) {
			return fmt.Errorf("artifact lacks checksum, SBOM, or provenance")
		}
		if artifact.Signature.Status == "signed" && (artifact.Signature.Bundle == "" || artifact.Signature.Identity == "") {
			return fmt.Errorf("signed artifact lacks bundle or identity")
		}
		if artifact.Signature.Status == "not-applicable" && artifact.Signature.Rationale == "" {
			return fmt.Errorf("unsigned artifact lacks rationale")
		}
	}
	return nil
}

func validateReleaseDecisions(manifest releaseManifest) error {
	if len(manifest.Decisions) == 0 || manifest.Roadmap.Inventory != "conformance/v1/suite.json#/issueMappings" ||
		manifest.Roadmap.Coverage != "every-component-issue-exactly-once" || manifest.Roadmap.ScopeDecision == "" {
		return fmt.Errorf("release lacks decision or roadmap scope record")
	}
	return nil
}

func validReleaseEvidence(evidence releaseEvidence) bool {
	return evidence.Path != "" && releaseSHA256.MatchString(evidence.SHA256)
}
