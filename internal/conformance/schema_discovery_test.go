package conformance_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	transporthttp "github.com/valksor/naatre/transport/http"
)

type schemaDiscoveryFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	OwnerIssue   int    `json:"ownerIssue"`
	Dependencies []struct {
		Profile string `json:"profile"`
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
	} `json:"dependencies"`
	Implementation struct {
		Language string `json:"language"`
		Runtime  string `json:"runtime"`
		Package  string `json:"package"`
		Files    []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	} `json:"implementation"`
	Endpoints []struct {
		Method   string `json:"method"`
		Path     string `json:"path"`
		Response string `json:"response"`
	} `json:"endpoints"`
	Defaults struct {
		Enabled             bool   `json:"enabled"`
		MaxConcurrent       int    `json:"maxConcurrent"`
		MaxRevisionBytes    int    `json:"maxRevisionBytes"`
		MaxDocumentBytes    int    `json:"maxDocumentBytes"`
		MaxResponseBytes    int    `json:"maxResponseBytes"`
		MaxMigrationChanges int    `json:"maxMigrationChanges"`
		RevisionHistory     string `json:"revisionHistory"`
	} `json:"defaults"`
	Cases []struct {
		Name         string `json:"name"`
		Class        string `json:"class"`
		ExpectedCode string `json:"expectedCode"`
	} `json:"cases"`
	SemanticDiffCoverage []struct {
		Change         string `json:"change"`
		Path           string `json:"path"`
		Classification string `json:"classification"`
	} `json:"semanticDiffCoverage"`
	StableFailureCodes []string `json:"stableFailureCodes"`
	FailureStatuses    []struct {
		Code       string `json:"code"`
		HTTPStatus *int   `json:"httpStatus"`
		Surface    string `json:"surface"`
	} `json:"failureStatuses"`
	Supported struct {
		Runtime            string   `json:"runtime"`
		Adapter            string   `json:"adapter"`
		Platform           string   `json:"platform"`
		CertifiedPlatforms []string `json:"certifiedPlatforms"`
	} `json:"supported"`
	Unsupported []string `json:"unsupported"`
	Commands    []string `json:"commands"`
}

func TestSchemaDiscoveryProfileEvidence(t *testing.T) {
	t.Parallel()
	var fixture schemaDiscoveryFixture
	readFixture(t, "schema-discovery.json", &fixture)
	if fixture.Profile != transporthttp.DiscoveryProfile || fixture.FixtureSuite != "1.0.0" || fixture.OwnerIssue != 106 {
		t.Fatalf("schema discovery identity = %#v", fixture)
	}
	limits := transporthttp.DefaultDiscoveryLimits()
	if fixture.Defaults.Enabled || fixture.Defaults.MaxConcurrent != limits.MaxConcurrent ||
		fixture.Defaults.MaxRevisionBytes != limits.MaxRevisionBytes || fixture.Defaults.MaxDocumentBytes != limits.MaxDocumentBytes ||
		fixture.Defaults.MaxResponseBytes != limits.MaxResponseBytes ||
		fixture.Defaults.MaxMigrationChanges != limits.MaxMigrationChanges || fixture.Defaults.RevisionHistory == "" {
		t.Fatalf("schema discovery defaults = %#v", fixture.Defaults)
	}
	if len(fixture.Endpoints) != 2 || fixture.Endpoints[0].Path != transporthttp.DiscoveryPath ||
		fixture.Endpoints[1].Path != transporthttp.DiscoveryDiffPath+"?from={revision}" {
		t.Fatalf("schema discovery endpoints = %#v", fixture.Endpoints)
	}
	if fixture.Implementation.Language != "go" || fixture.Implementation.Runtime != "go1.27" ||
		fixture.Implementation.Package != "github.com/valksor/naatre/transport/http" ||
		fixture.Supported.Runtime != "go1.27" || fixture.Supported.Adapter != "net/http" ||
		fixture.Supported.Platform != "portable-go-library" || len(fixture.Supported.CertifiedPlatforms) != 0 {
		t.Fatalf("schema discovery runtime boundary = %#v %#v", fixture.Implementation, fixture.Supported)
	}

	requiredCases := []string{
		"active-cancellation", "authenticated-filtered-referential", "authentication-required", "authorization-denied",
		"cache-authorization-identity", "concurrency-limit", "current-revision-byte-limit", "default-off", "document-byte-limit",
		"failure-redaction", "filtered-migration-classifications", "from-revision-byte-limit",
		"history-retention-limit", "migration-change-limit", "response-byte-limit", "revision-reuse-conflict",
		"single-revision-during-install",
	}
	wantCaseCodes := map[string]string{
		"active-cancellation":                transporthttp.CodeDiscoveryCancelled,
		"authenticated-filtered-referential": "OK",
		"authentication-required":            transporthttp.CodeDiscoveryUnauthenticated,
		"authorization-denied":               transporthttp.CodeDiscoveryForbidden,
		"cache-authorization-identity":       "OK",
		"concurrency-limit":                  transporthttp.CodeDiscoveryBusy,
		"current-revision-byte-limit":        transporthttp.CodeDiscoveryInternal,
		"default-off":                        transporthttp.CodeDiscoveryDisabled,
		"document-byte-limit":                transporthttp.CodeDiscoveryLimit,
		"failure-redaction":                  transporthttp.CodeDiscoveryInternal,
		"filtered-migration-classifications": "OK",
		"from-revision-byte-limit":           transporthttp.CodeDiscoveryBadRequest,
		"history-retention-limit":            transporthttp.CodeDiscoveryRevisionMissing,
		"migration-change-limit":             transporthttp.CodeDiscoveryLimit,
		"response-byte-limit":                transporthttp.CodeDiscoveryLimit,
		"revision-reuse-conflict":            transporthttp.CodeDiscoveryRevisionConflict,
		"single-revision-during-install":     "OK",
	}
	caseNames := make([]string, 0, len(fixture.Cases))
	classes := make(map[string]bool)
	for _, test := range fixture.Cases {
		if want, found := wantCaseCodes[test.Name]; !found || test.ExpectedCode != want {
			t.Errorf("case %q result = %q, want %q", test.Name, test.ExpectedCode, want)
		}
		caseNames = append(caseNames, test.Name)
		classes[test.Class] = true
	}
	slices.Sort(caseNames)
	if !slices.Equal(caseNames, requiredCases) {
		t.Fatalf("schema discovery cases = %v, want %v", caseNames, requiredCases)
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classes[class] {
			t.Errorf("schema discovery evidence omits %q cases", class)
		}
	}
	wantDiffChanges := map[string][2]string{
		"constraint":                {"type:User/maxDepth", "dangerous"},
		"default":                   {"field:Input.id/default", "behavior-only"},
		"effect":                    {"operation:query.user/effect", "dangerous"},
		"nullability-tightening":    {"field:User.name/nullable", "breaking"},
		"openness":                  {"type:Role/open", "breaking"},
		"removal":                   {"type:Removed", "breaking"},
		"required-input-tightening": {"field:Input.scope", "breaking"},
	}
	seenDiffChanges := make(map[string]bool, len(fixture.SemanticDiffCoverage))
	for _, change := range fixture.SemanticDiffCoverage {
		want, found := wantDiffChanges[change.Change]
		if !found || change.Path != want[0] || change.Classification != want[1] || seenDiffChanges[change.Change] {
			t.Errorf("semantic diff evidence %#v, want path/classification %#v", change, want)
		}
		seenDiffChanges[change.Change] = true
	}
	if len(seenDiffChanges) != len(wantDiffChanges) {
		t.Fatalf("semantic diff coverage = %v, want %v", seenDiffChanges, wantDiffChanges)
	}

	wantCodes := []string{
		transporthttp.CodeDiscoveryBadRequest, transporthttp.CodeDiscoveryBusy, transporthttp.CodeDiscoveryCancelled,
		transporthttp.CodeDiscoveryDisabled, transporthttp.CodeDiscoveryForbidden, transporthttp.CodeDiscoveryInternal,
		transporthttp.CodeDiscoveryLimit, transporthttp.CodeDiscoveryMethod, transporthttp.CodeDiscoveryNotFound,
		transporthttp.CodeDiscoveryRevisionConflict, transporthttp.CodeDiscoveryRevisionMissing, transporthttp.CodeDiscoveryUnauthenticated,
	}
	slices.Sort(wantCodes)
	gotCodes := slices.Clone(fixture.StableFailureCodes)
	slices.Sort(gotCodes)
	if !slices.Equal(gotCodes, wantCodes) {
		t.Fatalf("schema discovery failure codes = %v, want %v", gotCodes, wantCodes)
	}
	wantStatuses := map[string]int{
		transporthttp.CodeDiscoveryBadRequest:      400,
		transporthttp.CodeDiscoveryBusy:            503,
		transporthttp.CodeDiscoveryCancelled:       408,
		transporthttp.CodeDiscoveryDisabled:        404,
		transporthttp.CodeDiscoveryForbidden:       403,
		transporthttp.CodeDiscoveryInternal:        500,
		transporthttp.CodeDiscoveryLimit:           500,
		transporthttp.CodeDiscoveryMethod:          405,
		transporthttp.CodeDiscoveryNotFound:        404,
		transporthttp.CodeDiscoveryRevisionMissing: 404,
		transporthttp.CodeDiscoveryUnauthenticated: 401,
	}
	for _, failure := range fixture.FailureStatuses {
		if failure.Code == transporthttp.CodeDiscoveryRevisionConflict {
			if failure.HTTPStatus != nil || failure.Surface != "revision-store" {
				t.Errorf("revision conflict surface = %#v", failure)
			}
			continue
		}
		want, found := wantStatuses[failure.Code]
		if !found || failure.HTTPStatus == nil || *failure.HTTPStatus != want || failure.Surface != "http" {
			t.Errorf("failure status %#v, want HTTP %d", failure, want)
		}
		delete(wantStatuses, failure.Code)
	}
	if len(wantStatuses) != 0 || len(fixture.FailureStatuses) != len(wantCodes) {
		t.Fatalf("schema discovery failure status coverage missing = %v", wantStatuses)
	}
	if len(fixture.Unsupported) != 19 || !slices.Contains(fixture.Unsupported, "unfiltered-discovery") ||
		!slices.Contains(fixture.Unsupported, "conditional-if-none-match-304") ||
		!slices.Contains(fixture.Unsupported, "accept-content-negotiation") ||
		!slices.Contains(fixture.Unsupported, "native-non-go-runtime-adapters") ||
		!slices.Contains(fixture.Unsupported, "os-architecture-proxy-orchestrator-certification") {
		t.Fatalf("schema discovery unsupported boundary = %v", fixture.Unsupported)
	}
	if len(fixture.Commands) != 3 || !strings.Contains(fixture.Commands[1], "go test -race") {
		t.Fatalf("schema discovery commands = %v", fixture.Commands)
	}

	root := filepath.Join("..", "..")
	for _, dependency := range fixture.Dependencies {
		if dependency.Profile == "" || dependency.Path == "" || len(dependency.SHA256) != 64 {
			t.Fatalf("invalid dependency revision %#v", dependency)
		}
		if got := conformanceFileDigest(t, filepath.Join(root, filepath.FromSlash(dependency.Path))); got != dependency.SHA256 {
			t.Errorf("dependency %s digest = %s, want %s", dependency.Path, got, dependency.SHA256)
		}
	}
	for _, file := range fixture.Implementation.Files {
		if file.Path == "" || len(file.SHA256) != 64 {
			t.Fatalf("invalid implementation revision %#v", file)
		}
		if got := conformanceFileDigest(t, filepath.Join(root, filepath.FromSlash(file.Path))); got != file.SHA256 {
			t.Errorf("implementation %s digest = %s, want %s", file.Path, got, file.SHA256)
		}
	}
}
