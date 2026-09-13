package conformance_test

import (
	"slices"
	"testing"
)

type operationsFixture struct {
	Profile            string               `json:"profile"`
	States             []string             `json:"states"`
	WorkKinds          []string             `json:"workKinds"`
	EventStages        []string             `json:"eventStages"`
	PublicCodes        []string             `json:"publicCodes"`
	Accounting         operationsAccounting `json:"accounting"`
	FairnessVectors    []operationsFairness `json:"fairnessVectors"`
	HealthVectors      []operationsHealth   `json:"healthVectors"`
	DrainVectors       []operationsDrain    `json:"drainVectors"`
	CleanupKinds       []string             `json:"cleanupKinds"`
	RevisionSequence   []string             `json:"revisionSequence"`
	ConnectionRotation operationsRotation   `json:"connectionRotation"`
	Boundaries         map[string]bool      `json:"boundaries"`
}

type operationsAccounting struct {
	Active                     []string `json:"active"`
	Queued                     []string `json:"queued"`
	MinimumRequestWeight       uint64   `json:"minimumRequestWeight"`
	PartitionReferenceMaxBytes uint64   `json:"partitionReferenceMaxBytes"`
	ReleaseOnCancellation      bool     `json:"releaseOnCancellation"`
}

type operationsFairness struct {
	Name          string   `json:"name"`
	ActiveTenant  string   `json:"activeTenant"`
	QueuedTenants []string `json:"queuedTenants"`
	NextTenant    string   `json:"nextTenant"`
}

type operationsHealth struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Ready bool   `json:"ready"`
	Live  bool   `json:"live"`
}

type operationsDrain struct {
	Name             string   `json:"name"`
	Cancel           []string `json:"cancel"`
	Finish           []string `json:"finish"`
	Outcome          string   `json:"outcome"`
	ReleaseOwnership bool     `json:"releaseOwnership"`
}

type operationsRotation struct {
	Finite                          bool `json:"finite"`
	DeterministicStagger            bool `json:"deterministicStagger"`
	IdentityExpiryShortens          bool `json:"identityExpiryShortens"`
	StaggerStrictlyLessThanLifetime bool `json:"staggerStrictlyLessThanLifetime"`
}

func TestOperationsLifecycleFixture(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	assertOperationsHeader(t, *fixture)
	assertOperationsBehavior(t, *fixture)
}

func assertOperationsHeader(t *testing.T, fixture operationsFixture) {
	t.Helper()
	assertExactStrings(t, "states", fixture.States, []string{"starting", "ready", "draining", "stopped", "forced"})
	assertExactStrings(t, "work kinds", fixture.WorkKinds, []string{"query", "mutation", "stream", "remote", "durable"})
	assertExactStrings(t, "event stages", fixture.EventStages, []string{
		"started", "dependency-unavailable", "dependency-recovered", "admission-queued", "admission-granted", "admission-rejected",
		"admission-cancelled", "work-released", "drain-started", "drain-completed", "drain-forced", "revision-installed", "revision-rolled-back",
	})
	assertExactStrings(t, "public codes", fixture.PublicCodes, []string{"OVERLOADED", "RATE_LIMITED", "CANCELLED", "RESOURCE_EXHAUSTED"})
	assertExactStrings(t, "active accounting", fixture.Accounting.Active, []string{"inFlight", "reservedBytes", "resultBufferBytes", "streams", "remoteConnections"})
	assertExactStrings(t, "queued accounting", fixture.Accounting.Queued, []string{"queued", "queuedBytes"})
	assertExactStrings(t, "cleanup kinds", fixture.CleanupKinds, []string{"rollback", "lease-release", "source-close"})
	assertExactStrings(t, "revision sequence", fixture.RevisionSequence, []string{"r1", "r2", "r1"})
	if fixture.Profile != "operations.lifecycle-1" || fixture.Accounting.MinimumRequestWeight != 1 ||
		fixture.Accounting.PartitionReferenceMaxBytes != 256 || fixture.Accounting.ReleaseOnCancellation {
		t.Fatalf("operations fixture header/accounting = %#v", fixture)
	}
}

func assertOperationsBehavior(t *testing.T, fixture operationsFixture) {
	t.Helper()
	if len(fixture.FairnessVectors) != 1 || fixture.FairnessVectors[0].Name != "tenant-round-robin" ||
		fixture.FairnessVectors[0].ActiveTenant != "tenant-a" ||
		!slices.Equal(fixture.FairnessVectors[0].QueuedTenants, []string{"tenant-a", "tenant-a", "tenant-b"}) || fixture.FairnessVectors[0].NextTenant != "tenant-b" {
		t.Fatalf("fairness vectors = %#v", fixture.FairnessVectors)
	}
	wantHealth := []operationsHealth{
		{Name: "startup", State: "starting", Live: true},
		{Name: "ready", State: "ready", Ready: true, Live: true},
		{Name: "transient-dependency-failure", State: "ready", Live: true},
		{Name: "sustained-dependency-failure", State: "ready"},
		{Name: "draining", State: "draining", Live: true},
		{Name: "stopped", State: "stopped"},
		{Name: "forced", State: "forced"},
	}
	if !slices.Equal(fixture.HealthVectors, wantHealth) {
		t.Fatalf("health vectors = %#v, want %#v", fixture.HealthVectors, wantHealth)
	}
	wantDrain := []operationsDrain{
		{Name: "default-drain", Cancel: []string{"query", "stream", "remote"}, Finish: []string{"mutation-committing", "durable"}, Outcome: "completed", ReleaseOwnership: true},
		{Name: "forced-drain", Cancel: []string{"query", "mutation-committing", "stream", "remote", "durable"}, Finish: []string{}, Outcome: "forced"},
	}
	if !slices.EqualFunc(fixture.DrainVectors, wantDrain, func(left, right operationsDrain) bool {
		return left.Name == right.Name && slices.Equal(left.Cancel, right.Cancel) && slices.Equal(left.Finish, right.Finish) &&
			left.Outcome == right.Outcome && left.ReleaseOwnership == right.ReleaseOwnership
	}) {
		t.Fatalf("drain vectors = %#v, want %#v", fixture.DrainVectors, wantDrain)
	}
	if !fixture.ConnectionRotation.Finite || !fixture.ConnectionRotation.DeterministicStagger ||
		!fixture.ConnectionRotation.IdentityExpiryShortens || !fixture.ConnectionRotation.StaggerStrictlyLessThanLifetime {
		t.Fatalf("connection rotation = %#v", fixture.ConnectionRotation)
	}
	wantBoundaries := map[string]bool{
		"admissionBeforeBusinessExecution": true, "activeRevisionImmutable": true, "queuedCancellationReleasesAccounting": true,
		"uncooperativeWorkRetainsSlot": true, "partitionCapacityReserved": true,
		"healthExposesProtectedMetadata": false, "hookEventsExposePartitionReferences": false,
	}
	if len(fixture.Boundaries) != len(wantBoundaries) {
		t.Fatalf("boundary inventory = %#v", fixture.Boundaries)
	}
	for name, want := range wantBoundaries {
		if got, ok := fixture.Boundaries[name]; !ok || got != want {
			t.Errorf("boundary %q = %v,%v want %v", name, got, ok, want)
		}
	}
}
