package conformance_test

import (
	"slices"
	"testing"
)

type mutationUpdateFixture struct {
	Profile              string                    `json:"profile"`
	Capabilities         []string                  `json:"capabilities"`
	EditActions          []string                  `json:"editActions"`
	PreconditionCodes    []string                  `json:"preconditionCodes"`
	UpdateCases          []mutationUpdateCase      `json:"updateCases"`
	ConcurrencyCases     []mutationConcurrencyCase `json:"concurrencyCases"`
	ReadConsistencyCases []mutationReadCase        `json:"readConsistencyCases"`
	CommitBoundaryCases  []mutationBoundaryCase    `json:"commitBoundaryCases"`
	Ownership            struct {
		PortableContract               int  `json:"portableContract"`
		CompleteSDKMatrix              int  `json:"completeSdkMatrix"`
		SDKEvidenceRequiredPerLanguage bool `json:"sdkEvidenceRequiredPerLanguage"`
	} `json:"ownership"`
}

type mutationUpdateCase struct {
	Name             string         `json:"name"`
	Current          map[string]any `json:"current"`
	Input            map[string]any `json:"input"`
	Expected         map[string]any `json:"expected"`
	ExpectedAbsent   []string       `json:"expectedAbsent"`
	ExpectedRevision string         `json:"expectedRevision"`
	Edits            []any          `json:"edits"`
	Patch            []any          `json:"patch"`
	Code             string         `json:"code"`
	Path             []any          `json:"path"`
	Polarity         string         `json:"polarity"`
}

type mutationConcurrencyCase struct {
	Name                     string   `json:"name"`
	MaximumSuccessfulCommits int      `json:"maximumSuccessfulCommits"`
	ProtectedWrites          *int     `json:"protectedWrites"`
	ForbiddenDetails         []string `json:"forbiddenDetails"`
	AdditionalWrites         *int     `json:"additionalWrites"`
	Requests                 int      `json:"requests"`
	SharedExpectedRevision   bool     `json:"sharedExpectedRevision"`
	LoserCode                string   `json:"loserCode"`
	Code                     string   `json:"code"`
	FirstResult              string   `json:"firstResult"`
	CurrentRevision          string   `json:"currentRevision"`
	ReplayResult             string   `json:"replayResult"`
}

type mutationReadCase struct {
	Name                         string `json:"name"`
	Requested                    string `json:"requested"`
	Effective                    string `json:"effective"`
	FirstRevision                string `json:"firstRevision"`
	ConcurrentCommit             string `json:"concurrentCommit"`
	SecondRevision               string `json:"secondRevision"`
	AfterRevision                string `json:"afterRevision"`
	ObservesAfterRevisionOrLater bool   `json:"observesAfterRevisionOrLater"`
	Code                         string `json:"code"`
}

type mutationBoundaryCase struct {
	Name                   string `json:"name"`
	PriorRevision          string `json:"priorRevision"`
	AttemptedRevision      string `json:"attemptedRevision"`
	CommittedRevision      string `json:"committedRevision"`
	Fault                  string `json:"fault"`
	Effect                 string `json:"effect"`
	PublishedRevision      string `json:"publishedRevision"`
	PublishesTentativeData bool   `json:"publishesTentativeData"`
}

func TestPortableMutationUpdateContract(t *testing.T) {
	t.Parallel()
	var fixture mutationUpdateFixture
	readFixture(t, "mutation-updates.json", &fixture)
	assertMutationFixtureHeader(t, fixture)
	assertMutationUpdateCases(t, fixture.UpdateCases)
	assertMutationConcurrencyCases(t, fixture.ConcurrencyCases)
	assertMutationReadCases(t, fixture.ReadConsistencyCases)
	assertMutationBoundaryCases(t, fixture.CommitBoundaryCases)
	if fixture.Ownership.PortableContract != 52 || fixture.Ownership.CompleteSDKMatrix != 69 || !fixture.Ownership.SDKEvidenceRequiredPerLanguage {
		t.Fatal("mutation update evidence ownership is incomplete")
	}
}

func assertMutationFixtureHeader(t *testing.T, fixture mutationUpdateFixture) {
	t.Helper()
	if fixture.Profile != "mutation.update-1" {
		t.Fatalf("profile = %q", fixture.Profile)
	}
	assertExactStrings(t, "mutation capabilities", fixture.Capabilities, []string{"mutation.typed-update-1", "mutation.json-patch-1", "mutation.read-consistency-1"})
	assertExactStrings(t, "mutation edit actions", fixture.EditActions, []string{"set", "remove", "list-append", "list-insert", "list-replace", "list-remove", "map-set", "map-remove"})
	assertExactStrings(t, "mutation precondition codes", fixture.PreconditionCodes, []string{"PRECONDITION_REQUIRED", "REVISION_CONFLICT", "ENTITY_NOT_FOUND", "PRECONDITION_UNSUPPORTED", "UNAUTHORIZED"})
}

func assertMutationUpdateCases(t *testing.T, cases []mutationUpdateCase) {
	t.Helper()
	updates := make(map[string]mutationUpdateCase, len(cases))
	for _, test := range cases {
		if test.Name == "" || updates[test.Name].Name != "" || (test.Polarity != "positive" && test.Polarity != "negative") {
			t.Fatalf("invalid or duplicate update case %q", test.Name)
		}
		updates[test.Name] = test
	}
	for _, name := range []string{"omitted-field-unchanged", "explicit-null-is-value", "removal-is-absence", "forbidden-property", "invalid-patch-path", "list-index-conflict", "duplicate-typed-target", "patch-test-precondition"} {
		if updates[name].Name == "" {
			t.Errorf("missing update case %q", name)
		}
	}
	assertMutationUpdateSemantics(t, updates)
}

func assertMutationUpdateSemantics(t *testing.T, updates map[string]mutationUpdateCase) {
	t.Helper()
	if updates["omitted-field-unchanged"].Expected["nickname"] != "nick" {
		t.Error("omitted update field is not preserved")
	}
	if value, exists := updates["explicit-null-is-value"].Expected["nickname"]; !exists || value != nil {
		t.Error("explicit null is not represented as a present null")
	}
	if !slices.Equal(updates["removal-is-absence"].ExpectedAbsent, []string{"nickname"}) {
		t.Error("removal is not represented as field absence")
	}
	if updates["forbidden-property"].Code != "FORBIDDEN_PROPERTY" || updates["invalid-patch-path"].Code != "INVALID_PATCH_PATH" || updates["list-index-conflict"].Code != "LIST_INDEX_CONFLICT" {
		t.Error("typed update negative matrix has unstable codes")
	}
	indexConflict := updates["list-index-conflict"]
	tags, tagsOK := indexConflict.Current["tags"].([]any)
	if indexConflict.ExpectedRevision == "" || !tagsOK || !slices.Equal(tags, []any{"only"}) {
		t.Error("list index conflict does not bind an out-of-range edit to a current revision")
	}
	if !slices.Equal(updates["forbidden-property"].Path, []any{"input", "administrator"}) || !slices.Equal(updates["invalid-patch-path"].Path, []any{"patch", float64(0)}) {
		t.Error("typed update negative matrix has unstable error paths")
	}
}

func assertMutationConcurrencyCases(t *testing.T, cases []mutationConcurrencyCase) {
	t.Helper()
	concurrency := make(map[string]mutationConcurrencyCase, len(cases))
	for _, test := range cases {
		concurrency[test.Name] = test
	}
	race := concurrency["same-revision-at-most-one-commit"]
	if race.Requests != 2 || !race.SharedExpectedRevision || race.MaximumSuccessfulCommits != 1 || race.LoserCode != "REVISION_CONFLICT" {
		t.Error("concurrent revision fixture permits more than one commit")
	}
	noWrite := concurrency["precondition-failure-no-write-no-leak"]
	if noWrite.Code != "REVISION_CONFLICT" || noWrite.ProtectedWrites == nil || *noWrite.ProtectedWrites != 0 || !slices.Equal(noWrite.ForbiddenDetails, []string{"currentValue", "currentRevision", "providerKey"}) {
		t.Error("precondition failure fixture does not enforce no-write/no-leak")
	}
	replay := concurrency["idempotency-replay-before-revision-check"]
	if replay.FirstResult != "committed-r2" || replay.CurrentRevision != "committed-r3" || replay.ReplayResult != replay.FirstResult || replay.AdditionalWrites == nil || *replay.AdditionalWrites != 0 {
		t.Error("idempotency replay fixture permits a repeated write")
	}
}

func assertMutationReadCases(t *testing.T, cases []mutationReadCase) {
	t.Helper()
	reads := make(map[string]mutationReadCase, len(cases))
	for _, test := range cases {
		reads[test.Name] = test
	}
	for _, name := range []string{"best-effort-default", "snapshot-remains-stable", "read-your-writes-observes-commit", "unsupported-guarantee-is-explicit"} {
		if reads[name].Name == "" {
			t.Errorf("missing read consistency case %q", name)
		}
	}
	if reads["unsupported-guarantee-is-explicit"].Code != "READ_CONSISTENCY_UNSUPPORTED" {
		t.Error("unsupported read consistency lacks stable code")
	}
	snapshot := reads["snapshot-remains-stable"]
	if snapshot.FirstRevision != "committed-r1" || snapshot.ConcurrentCommit != "committed-r2" || snapshot.SecondRevision != snapshot.FirstRevision {
		t.Error("snapshot read fixture does not preserve its declared revision")
	}
	readYourWrites := reads["read-your-writes-observes-commit"]
	if readYourWrites.AfterRevision == "" || !readYourWrites.ObservesAfterRevisionOrLater {
		t.Error("read-your-writes fixture does not require the committed revision")
	}
}

func assertMutationBoundaryCases(t *testing.T, cases []mutationBoundaryCase) {
	t.Helper()
	boundaries := make(map[string]mutationBoundaryCase, len(cases))
	for _, test := range cases {
		boundaries[test.Name] = test
		if test.PublishesTentativeData {
			t.Errorf("boundary case %q publishes tentative data", test.Name)
		}
	}
	for _, name := range []string{"rollback-preserves-prior-revision", "cache-invalidation-after-commit", "stream-truncation-after-commit", "unknown-commit-withholds-revision"} {
		if boundaries[name].Name == "" {
			t.Errorf("missing commit boundary case %q", name)
		}
	}
	rollback := boundaries["rollback-preserves-prior-revision"]
	if rollback.PriorRevision == "" || rollback.PublishedRevision != rollback.PriorRevision {
		t.Error("rollback fixture does not preserve the prior committed revision")
	}
	for _, name := range []string{"cache-invalidation-after-commit", "stream-truncation-after-commit"} {
		boundary := boundaries[name]
		if boundary.CommittedRevision == "" || boundary.PublishedRevision != boundary.CommittedRevision {
			t.Errorf("boundary case %q does not preserve the committed revision", name)
		}
	}
	unknown := boundaries["unknown-commit-withholds-revision"]
	if unknown.Effect != "indeterminate" || unknown.PublishedRevision != "" {
		t.Error("unknown commit fixture publishes a tentative revision")
	}
}
