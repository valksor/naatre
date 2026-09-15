package asyncapi

import (
	"encoding/json"
	"errors"
	"slices"
)

type projectionConformanceFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Owner        struct {
		Issue              int    `json:"issue"`
		CoreContractIssue  int    `json:"coreContractIssue"`
		NormativeProfile   string `json:"normativeProfile"`
		DependencyRevision string `json:"dependencyRevision"`
	} `json:"owner"`
	Implementation struct {
		Module    string `json:"module"`
		Package   string `json:"package"`
		GoVersion string `json:"goVersion"`
		Files     []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	} `json:"implementation"`
	Runtime struct {
		Language                  string   `json:"language"`
		Minimum                   string   `json:"minimum"`
		PlatformProfile           string   `json:"platformProfile"`
		CertifiedOperatingSystems []string `json:"certifiedOperatingSystems"`
		BusinessHandlerCalls      int      `json:"businessHandlerCalls"`
	} `json:"runtime"`
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

// ValidateProjectionConformanceFixture validates the portable issue #102
// evidence index without executing or trusting any command from it.
func ValidateProjectionConformanceFixture(input []byte) error {
	var fixture projectionConformanceFixture
	if json.Unmarshal(input, &fixture) != nil {
		return errors.New("invalid AsyncAPI projection conformance fixture")
	}
	if !validProjectionFixtureIdentity(fixture) {
		return errors.New("invalid AsyncAPI projection identity or runtime boundary")
	}
	if !completeProjectionFixtureClasses(fixture) {
		return errors.New("incomplete AsyncAPI projection fixture classes")
	}
	if !completeProjectionFailureCodes(fixture.StableFailureCodes) {
		return errors.New("incomplete AsyncAPI projection failure codes")
	}
	if len(fixture.Capabilities.Supported) < 8 || len(fixture.Capabilities.Unsupported) < 12 || len(fixture.Commands) < 4 {
		return errors.New("incomplete AsyncAPI projection capability evidence")
	}
	return nil
}

func validProjectionFixtureIdentity(fixture projectionConformanceFixture) bool {
	if fixture.Profile != ProjectionProfile || fixture.FixtureSuite != "1.0.0" || fixture.Owner.Issue != 102 ||
		fixture.Owner.CoreContractIssue != 62 || fixture.Owner.NormativeProfile != Profile || len(fixture.Owner.DependencyRevision) != 40 {
		return false
	}
	if fixture.Implementation.Module != "github.com/valksor/naatre" || fixture.Implementation.Package != "github.com/valksor/naatre/asyncapi" ||
		fixture.Implementation.GoVersion != "1.27.0" || len(fixture.Implementation.Files) < 4 {
		return false
	}
	return fixture.Runtime.Language == "go" && fixture.Runtime.Minimum == "1.27.0" && fixture.Runtime.PlatformProfile == "portable-offline-library" &&
		len(fixture.Runtime.CertifiedOperatingSystems) == 0 && fixture.Runtime.BusinessHandlerCalls == 0
}

func completeProjectionFixtureClasses(fixture projectionConformanceFixture) bool {
	classes := make(map[string]bool)
	for _, item := range fixture.Fixtures {
		if item.Name == "" || len(item.Tests) == 0 {
			return false
		}
		classes[item.Class] = true
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classes[class] {
			return false
		}
	}
	return true
}

func completeProjectionFailureCodes(codes []string) bool {
	for _, code := range []string{"ASYNCAPI_DOCUMENT_INVALID", "ASYNCAPI_PROJECTION_CANCELLED", "ASYNCAPI_PROJECTION_DOCUMENT_LIMIT", "ASYNCAPI_PROJECTION_MISMATCH", "ASYNCAPI_SCHEMA_AUTHORITY_MISMATCH", "ASYNCAPI_SCHEMA_AUTHORITY_REQUIRED"} {
		if !slices.Contains(codes, code) {
			return false
		}
	}
	return true
}
