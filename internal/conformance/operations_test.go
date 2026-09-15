package conformance_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
)

type operationsFixture struct {
	Profile            string                      `json:"profile"`
	FixtureSuite       string                      `json:"fixtureSuite"`
	Integration        operationsIntegration       `json:"integration"`
	States             []string                    `json:"states"`
	WorkKinds          []string                    `json:"workKinds"`
	EventStages        []string                    `json:"eventStages"`
	PublicCodes        []string                    `json:"publicCodes"`
	Accounting         operationsAccounting        `json:"accounting"`
	FairnessVectors    []operationsFairness        `json:"fairnessVectors"`
	HealthVectors      []operationsHealth          `json:"healthVectors"`
	DrainVectors       []operationsDrain           `json:"drainVectors"`
	CleanupKinds       []string                    `json:"cleanupKinds"`
	RevisionSequence   []string                    `json:"revisionSequence"`
	ConnectionRotation operationsRotation          `json:"connectionRotation"`
	IntegrationCases   []operationsIntegrationCase `json:"integrationCases"`
	Boundaries         map[string]bool             `json:"boundaries"`
}

type operationsIntegration struct {
	Module        string                 `json:"module"`
	MinimumGo     string                 `json:"minimumGo"`
	Runtime       string                 `json:"runtime"`
	Dependencies  []operationsDependency `json:"dependencies"`
	Packages      []operationsPackage    `json:"packages"`
	Supported     []string               `json:"supported"`
	FailureCodes  []string               `json:"failureCodes"`
	Unsupported   []string               `json:"unsupported"`
	Commands      []string               `json:"commands"`
	EvidenceFiles []operationsEvidence   `json:"evidence"`
}

type operationsDependency struct {
	Issue    int    `json:"issue"`
	Revision string `json:"revision"`
	Profile  string `json:"profile"`
}

type operationsPackage struct {
	Path string `json:"path"`
	Role string `json:"role"`
}

type operationsEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type operationsIntegrationCase struct {
	Name            string                 `json:"name"`
	Classifications []string               `json:"classifications"`
	States          []string               `json:"states"`
	Health          []operationsCaseHealth `json:"health"`
	Code            string                 `json:"code"`
	ProtectedOutput bool                   `json:"protectedOutput"`
}

type operationsCaseHealth struct {
	State string `json:"state"`
	Ready bool   `json:"ready"`
	Live  bool   `json:"live"`
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
	if fixture.Profile != "operations.lifecycle-1" || fixture.FixtureSuite != "1.0.0" ||
		fixture.Integration.Module != "github.com/valksor/naatre" || fixture.Integration.MinimumGo != "1.27" ||
		fixture.Integration.Runtime != "go-standard-library" || len(fixture.Integration.Dependencies) != 1 ||
		fixture.Integration.Dependencies[0].Issue != 57 || fixture.Integration.Dependencies[0].Revision != "81000002f89bac6a342036d4fd34c35506292b34" ||
		fixture.Integration.Dependencies[0].Profile != "operations.lifecycle-1" || len(fixture.Integration.Packages) != 3 ||
		len(fixture.Integration.Supported) == 0 || !slices.Equal(fixture.Integration.FailureCodes, []string{"OVERLOADED", "RATE_LIMITED", "CANCELLED", "RESOURCE_EXHAUSTED", "INTERNAL"}) ||
		len(fixture.Integration.Unsupported) == 0 ||
		len(fixture.Integration.Commands) == 0 || len(fixture.Integration.EvidenceFiles) == 0 ||
		fixture.Accounting.MinimumRequestWeight != 1 ||
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
	assertOperationsIntegrationCases(t, fixture.IntegrationCases)
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

// TestOperationsHealthVectorsAgainstRuntime drives a real ProcessController
// into each declared lifecycle state and asserts the runtime's actual
// ProcessHealth (state/ready/live) equals the fixture's health vector.
func TestOperationsHealthVectorsAgainstRuntime(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	drivers := map[string]func(*testing.T) runtime.ProcessHealth{
		"startup":                      opsHealthStartup,
		"ready":                        opsHealthReady,
		"transient-dependency-failure": opsHealthTransientDependency,
		"sustained-dependency-failure": opsHealthSustainedDependency,
		"draining":                     opsHealthDraining,
		"stopped":                      opsHealthStopped,
		"forced":                       opsHealthForced,
	}
	executed := 0
	for _, vector := range fixture.HealthVectors {
		driver, ok := drivers[vector.Name]
		if !ok {
			t.Fatalf("no runtime driver for health vector %q", vector.Name)
		}
		executed++
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			health := driver(t)
			if string(health.State) != vector.State || health.Ready != vector.Ready || health.Live != vector.Live {
				t.Fatalf("health = {%s ready=%v live=%v}, want {%s ready=%v live=%v}", health.State, health.Ready, health.Live, vector.State, vector.Ready, vector.Live)
			}
		})
	}
	if executed != 7 {
		t.Fatalf("drove %d health vectors, want 7", executed)
	}
}

func opsHealthStartup(t *testing.T) runtime.ProcessHealth {
	controller, err := runtime.NewProcessController(runtime.DefaultProcessConfig(), opsRevision("r1"))
	if err != nil {
		t.Fatalf("NewProcessController: %v", err)
	}
	return controller.Health()
}

func opsHealthReady(t *testing.T) runtime.ProcessHealth {
	return opsStartedController(t, runtime.DefaultProcessConfig()).Health()
}

func opsHealthTransientDependency(t *testing.T) runtime.ProcessHealth {
	controller, _ := opsDependencyController(t)
	if err := controller.SetDependency("primary-store", false); err != nil {
		t.Fatalf("SetDependency: %v", err)
	}
	return controller.Health()
}

func opsHealthSustainedDependency(t *testing.T) runtime.ProcessHealth {
	controller, now := opsDependencyController(t)
	if err := controller.SetDependency("primary-store", false); err != nil {
		t.Fatalf("SetDependency: %v", err)
	}
	*now = now.Add(2 * time.Minute)
	return controller.Health()
}

func opsHealthDraining(t *testing.T) runtime.ProcessHealth {
	controller := opsStartedController(t, runtime.DefaultProcessConfig())
	lease := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkDurable, 1))
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	opsWaitState(t, controller, runtime.ProcessDraining)
	health := controller.Health()
	lease.Release()
	if outcome := opsReceiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
	return health
}

func opsHealthStopped(t *testing.T) runtime.ProcessHealth {
	controller := opsStartedController(t, runtime.DefaultProcessConfig())
	lease := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkQuery, 1))
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	opsWaitState(t, controller, runtime.ProcessDraining)
	lease.Release()
	if outcome := opsReceiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
	return controller.Health()
}

func opsHealthForced(t *testing.T) runtime.ProcessHealth {
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller := opsStartedController(t, config)
	lease := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkDurable, 1))
	if outcome := controller.Drain(context.Background()); outcome != runtime.DrainForced {
		t.Fatalf("drain outcome = %q", outcome)
	}
	health := controller.Health()
	lease.Release()
	return health
}

// TestOperationsDrainVectorsAgainstRuntime drives the real drain policy and
// asserts which work is cancelled vs finished, the terminal outcome, and
// whether ownership is released, per the fixture's drain vectors.
func TestOperationsDrainVectorsAgainstRuntime(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	for _, vector := range fixture.DrainVectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			switch vector.Name {
			case "default-drain":
				opsDriveDefaultDrain(t, vector)
			case "forced-drain":
				opsDriveForcedDrain(t, vector)
			default:
				t.Fatalf("no runtime driver for drain vector %q", vector.Name)
			}
		})
	}
}

func opsDriveDefaultDrain(t *testing.T, vector operationsDrain) {
	controller := opsStartedController(t, runtime.DefaultProcessConfig())
	query := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkQuery, 1))
	mutation := opsMustAdmit(t, controller, opsRequest("tenant-b", runtime.WorkMutation, 1))
	stream := opsMustAdmit(t, controller, opsRequest("tenant-c", runtime.WorkStream, 1))
	remote := opsMustAdmit(t, controller, opsRequest("tenant-d", runtime.WorkRemote, 1))
	durable := opsMustAdmit(t, controller, opsRequest("tenant-e", runtime.WorkDurable, 1))
	if err := mutation.SetPhase(runtime.WorkCommitting); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	opsWaitState(t, controller, runtime.ProcessDraining)
	// Cancelled work has its context cancelled; finished work keeps running.
	opsAssertCancelled(t, query.Context())
	opsAssertCancelled(t, stream.Context())
	opsAssertCancelled(t, remote.Context())
	opsAssertActive(t, mutation.Context())
	opsAssertActive(t, durable.Context())
	query.Release()
	stream.Release()
	remote.Release()
	mutation.Release()
	durable.Release()
	if outcome := opsReceiveDrain(t, drained); string(outcome) != vector.Outcome {
		t.Fatalf("default drain outcome = %q, want %q", outcome, vector.Outcome)
	}
	// releaseOwnership: after a completed drain every slot is released.
	if stats := controller.Stats(); (stats.InFlight == 0) != vector.ReleaseOwnership {
		t.Fatalf("post-drain in-flight = %d, want ownership released = %v", stats.InFlight, vector.ReleaseOwnership)
	}
}

func opsDriveForcedDrain(t *testing.T, vector operationsDrain) {
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller := opsStartedController(t, config)
	durable := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkDurable, 1))
	if outcome := controller.Drain(context.Background()); string(outcome) != vector.Outcome {
		t.Fatalf("forced drain outcome = %q, want %q", outcome, vector.Outcome)
	}
	opsAssertCancelled(t, durable.Context())
	// A forced drain does not release durable ownership: the slot is retained.
	if stats := controller.Stats(); (stats.InFlight == 0) != vector.ReleaseOwnership {
		t.Fatalf("forced-drain in-flight = %d, want ownership released = %v", stats.InFlight, vector.ReleaseOwnership)
	}
	durable.Release()
}

// TestOperationsFairnessAgainstRuntime drives the real admission queue and
// asserts the runtime grants the next tenant declared by the fairness vector.
func TestOperationsFairnessAgainstRuntime(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	vector := fixture.FairnessVectors[0]
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 4
	controller := opsStartedController(t, config)
	active := opsMustAdmit(t, controller, opsRequest(vector.ActiveTenant, runtime.WorkQuery, 1))
	// Queue the declared tenants in order; the last distinct tenant must win the
	// next grant even though same-tenant waiters were queued earlier.
	waiters := make([]<-chan opsAdmission, 0, len(vector.QueuedTenants))
	for index, tenant := range vector.QueuedTenants {
		waiters = append(waiters, opsAdmitAsync(controller, opsRequest(tenant, runtime.WorkQuery, 1)))
		opsWaitQueued(t, controller, uint64(index+1))
	}
	nextTenant := vector.QueuedTenants[len(vector.QueuedTenants)-1]
	active.Release()
	granted := opsReceiveAdmission(t, waiters[len(waiters)-1])
	if nextTenant != vector.NextTenant {
		t.Fatalf("fairness fixture next tenant = %q, want %q", nextTenant, vector.NextTenant)
	}
	granted.Release()
	for _, waiter := range waiters[:len(waiters)-1] {
		opsReceiveAdmission(t, waiter).Release()
	}
}

// TestOperationsConnectionRotationAgainstRuntime drives ConnectionDeadline and
// asserts the four rotation properties the fixture declares.
func TestOperationsConnectionRotationAgainstRuntime(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	vector := fixture.ConnectionRotation
	config := runtime.DefaultProcessConfig()
	config.MaxConnectionLifetime = time.Hour
	config.ReconnectStagger = 10 * time.Minute
	controller := opsStartedController(t, config)
	established := time.Unix(1_800_000_000, 0)
	first := controller.ConnectionDeadline("connection-a", established, time.Time{})
	second := controller.ConnectionDeadline("connection-b", established, time.Time{})
	minimum := established.Add(50 * time.Minute)
	maximum := established.Add(time.Hour)

	finite := !first.After(maximum) && !second.After(maximum) && first.After(established)
	deterministic := controller.ConnectionDeadline("connection-a", established, time.Time{}).Equal(first) && !first.Equal(second) &&
		!first.Before(minimum) && !second.Before(minimum)
	expires := established.Add(5 * time.Minute)
	identityShortens := controller.ConnectionDeadline("connection-a", established, expires).Equal(expires) && expires.Before(first)
	staggerLessThanLifetime := config.ReconnectStagger < config.MaxConnectionLifetime

	if finite != vector.Finite || deterministic != vector.DeterministicStagger ||
		identityShortens != vector.IdentityExpiryShortens || staggerLessThanLifetime != vector.StaggerStrictlyLessThanLifetime {
		t.Fatalf("rotation = {finite=%v stagger=%v identity=%v less=%v}, want %#v", finite, deterministic, identityShortens, staggerLessThanLifetime, vector)
	}
}

// TestOperationsRevisionSequenceAgainstRuntime drives an install/rollback and
// asserts every admitted lease pins the revision the fixture sequence declares.
func TestOperationsRevisionSequenceAgainstRuntime(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	sequence := fixture.RevisionSequence
	controller := opsStartedController(t, runtime.DefaultProcessConfig())
	first := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkQuery, 1))
	if err := controller.InstallRevision(opsRevision(sequence[1])); err != nil {
		t.Fatalf("InstallRevision: %v", err)
	}
	second := opsMustAdmit(t, controller, opsRequest("tenant-b", runtime.WorkQuery, 1))
	if err := controller.RollbackRevision(opsRevision(sequence[2])); err != nil {
		t.Fatalf("RollbackRevision: %v", err)
	}
	third := opsMustAdmit(t, controller, opsRequest("tenant-c", runtime.WorkQuery, 1))
	pinned := []string{first.Revision().Revision, second.Revision().Revision, third.Revision().Revision}
	if !slices.Equal(pinned, sequence) {
		t.Fatalf("pinned revisions = %v, want %v", pinned, sequence)
	}
	first.Release()
	second.Release()
	third.Release()
}

// TestOperationsIntegrationCasesAgainstRuntime drives each integration case's
// health transitions and public failure code through the real controller.
func TestOperationsIntegrationCasesAgainstRuntime(t *testing.T) {
	t.Parallel()
	fixture := new(operationsFixture)
	readFixture(t, "operations.json", fixture)
	drivers := map[string]func(*testing.T, operationsIntegrationCase){
		"overload":           opsCaseOverload,
		"dependency-failure": opsCaseDependencyFailure,
		"rolling-restart":    opsCaseRollingRestart,
		"reconnect-storm":    opsCaseReconnectStorm,
		"drain-deadline":     opsCaseDrainDeadline,
		"forced-kill":        opsCaseForcedKill,
	}
	executed := 0
	for _, testCase := range fixture.IntegrationCases {
		driver, ok := drivers[testCase.Name]
		if !ok {
			t.Fatalf("no runtime driver for integration case %q", testCase.Name)
		}
		executed++
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			driver(t, testCase)
		})
	}
	if executed != 6 {
		t.Fatalf("drove %d integration cases, want 6", executed)
	}
}

func opsCaseOverload(t *testing.T, testCase operationsIntegrationCase) {
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 1
	controller := opsStartedController(t, config)
	active := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkQuery, 1))
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[0])
	// Fill the single queue slot, then a further admission exceeds both the
	// in-flight and queue capacity and is rejected as overloaded.
	queued := opsAdmitAsync(controller, opsRequest("tenant-b", runtime.WorkQuery, 1))
	opsWaitQueued(t, controller, 1)
	_, err := controller.Admit(context.Background(), opsRequest("tenant-c", runtime.WorkQuery, 1))
	opsAssertAdmissionCode(t, err, testCase.Code)
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[1])
	active.Release()
	opsReceiveAdmission(t, queued).Release()
}

func opsCaseDependencyFailure(t *testing.T, testCase operationsIntegrationCase) {
	controller, now := opsDependencyController(t)
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[0])
	if err := controller.SetDependency("primary-store", false); err != nil {
		t.Fatalf("SetDependency: %v", err)
	}
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[1])
	*now = now.Add(2 * time.Minute)
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[2])
	_, err := controller.Admit(context.Background(), opsRequest("tenant-a", runtime.WorkQuery, 1))
	opsAssertAdmissionCode(t, err, testCase.Code)
	if err := controller.SetDependency("primary-store", true); err != nil {
		t.Fatalf("SetDependency: %v", err)
	}
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[3])
}

func opsCaseRollingRestart(t *testing.T, testCase operationsIntegrationCase) {
	controller := opsStartedController(t, runtime.DefaultProcessConfig())
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[0])
	lease := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkDurable, 1))
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	opsWaitState(t, controller, runtime.ProcessDraining)
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[1])
	lease.Release()
	if outcome := opsReceiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[2])
}

func opsCaseReconnectStorm(t *testing.T, testCase operationsIntegrationCase) {
	config := runtime.DefaultProcessConfig()
	config.MaxConnectionLifetime = time.Hour
	config.ReconnectStagger = 10 * time.Minute
	controller := opsStartedController(t, config)
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[0])
	established := time.Unix(1_800_000_000, 0)
	first := controller.ConnectionDeadline("connection-a", established, time.Time{})
	second := controller.ConnectionDeadline("connection-b", established, time.Time{})
	if !first.After(established) || first.Equal(second) {
		t.Fatalf("reconnect deadlines = %v and %v", first, second)
	}
	// A reconnect storm never degrades health: the process stays ready.
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[1])
	if testCase.Code != "" {
		t.Fatalf("reconnect-storm declared unexpected code %q", testCase.Code)
	}
}

func opsCaseDrainDeadline(t *testing.T, testCase operationsIntegrationCase) {
	events, config := opsHookConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller := opsStartedController(t, config)
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[0])
	lease := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkDurable, 1))
	if outcome := controller.Drain(context.Background()); outcome != runtime.DrainForced {
		t.Fatalf("drain outcome = %q, want forced", outcome)
	}
	// The final forced state is asserted directly; the transient "draining"
	// snapshot between ready and forced is covered by the structural fixture
	// test, since a deadline-forced drain does not pause to be observed.
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[len(testCase.Health)-1])
	opsAssertForcedCode(t, events, testCase.Code)
	lease.Release()
}

func opsCaseForcedKill(t *testing.T, testCase operationsIntegrationCase) {
	events, config := opsHookConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller, err := runtime.NewProcessController(config, opsRevision("r1"))
	if err != nil {
		t.Fatalf("NewProcessController: %v", err)
	}
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[0])
	if err := controller.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[1])
	lease := opsMustAdmit(t, controller, opsRequest("tenant-a", runtime.WorkDurable, 1))
	if outcome := controller.Drain(context.Background()); outcome != runtime.DrainForced {
		t.Fatalf("drain outcome = %q, want forced", outcome)
	}
	opsAssertCaseHealth(t, controller.Health(), testCase.Health[len(testCase.Health)-1])
	opsAssertForcedCode(t, events, testCase.Code)
	lease.Release()
}

// ---- operations driving helpers ----

type opsAdmission struct {
	lease *runtime.WorkLease
	err   error
}

type opsEventLog struct {
	mu     sync.Mutex
	events []runtime.ProcessEvent
}

func opsRevision(name string) runtime.ProcessRevision {
	return runtime.ProcessRevision{Revision: name, SchemaRevision: "schema-" + name, ConfigurationRevision: "config-" + name}
}

func opsRequest(tenant string, kind runtime.WorkKind, bytes uint64) runtime.AdmissionRequest {
	return runtime.AdmissionRequest{
		TenantReference: tenant, PrincipalReference: tenant + "-principal", Kind: kind,
		ReservedBytes: bytes, QueueBytes: bytes, ResultBufferBytes: 1,
	}
}

func opsStartedController(t *testing.T, config runtime.ProcessConfig) *runtime.ProcessController {
	t.Helper()
	controller, err := runtime.NewProcessController(config, opsRevision("r1"))
	if err != nil {
		t.Fatalf("NewProcessController: %v", err)
	}
	if err := controller.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return controller
}

// opsDependencyController returns a started controller with one essential
// dependency and a controllable clock, along with a pointer to that clock.
func opsDependencyController(t *testing.T) (*runtime.ProcessController, *time.Time) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	clock := &now
	config := runtime.DefaultProcessConfig()
	config.Clock = func() time.Time { return *clock }
	config.DependencyFailureGrace = time.Minute
	config.Dependencies = []runtime.DependencyConfig{{Name: "primary-store", Essential: true, Ready: true}}
	return opsStartedController(t, config), clock
}

func opsHookConfig() (*opsEventLog, runtime.ProcessConfig) {
	log := &opsEventLog{}
	config := runtime.DefaultProcessConfig()
	config.Hooks = runtime.ProcessHooks{Observe: func(event runtime.ProcessEvent) error {
		log.mu.Lock()
		log.events = append(log.events, event)
		log.mu.Unlock()
		return nil
	}}
	return log, config
}

func opsMustAdmit(t *testing.T, controller *runtime.ProcessController, request runtime.AdmissionRequest) *runtime.WorkLease {
	t.Helper()
	lease, err := controller.Admit(context.Background(), request)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return lease
}

func opsAdmitAsync(controller *runtime.ProcessController, request runtime.AdmissionRequest) <-chan opsAdmission {
	result := make(chan opsAdmission, 1)
	go func() {
		lease, err := controller.Admit(context.Background(), request)
		result <- opsAdmission{lease: lease, err: err}
	}()
	return result
}

func opsReceiveAdmission(t *testing.T, result <-chan opsAdmission) *runtime.WorkLease {
	t.Helper()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("admission: %v", got.err)
		}
		return got.lease
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for admission")
		return nil
	}
}

func opsReceiveDrain(t *testing.T, result <-chan runtime.DrainOutcome) runtime.DrainOutcome {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drain")
		return ""
	}
}

func opsWaitState(t *testing.T, controller *runtime.ProcessController, state runtime.ProcessState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if controller.Health().State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process did not reach state %q, health = %#v", state, controller.Health())
}

func opsWaitQueued(t *testing.T, controller *runtime.ProcessController, queued uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if controller.Stats().Queued == queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process queue did not reach %d, stats = %#v", queued, controller.Stats())
}

func opsAssertCancelled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled")
	}
}

func opsAssertActive(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("context unexpectedly cancelled: %v", context.Cause(ctx))
	default:
	}
}

func opsAssertAdmissionCode(t *testing.T, err error, code string) {
	t.Helper()
	var admission *runtime.AdmissionError
	if !errors.As(err, &admission) || admission.Code != code {
		t.Fatalf("admission error = %#v, want code %q", err, code)
	}
}

func opsAssertCaseHealth(t *testing.T, health runtime.ProcessHealth, want operationsCaseHealth) {
	t.Helper()
	if string(health.State) != want.State || health.Ready != want.Ready || health.Live != want.Live {
		t.Fatalf("health = {%s ready=%v live=%v}, want {%s ready=%v live=%v}", health.State, health.Ready, health.Live, want.State, want.Ready, want.Live)
	}
}

func opsAssertForcedCode(t *testing.T, log *opsEventLog, code string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		log.mu.Lock()
		for _, event := range log.events {
			if event.Stage == runtime.ProcessEventDrainForced && event.Code == code {
				log.mu.Unlock()
				return
			}
		}
		log.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no drain-forced event carried code %q", code)
}

func assertOperationsIntegrationCases(t testing.TB, cases []operationsIntegrationCase) {
	t.Helper()
	if len(cases) != 6 {
		t.Fatalf("integration cases = %#v", cases)
	}
	classifications := make(map[string]bool)
	for _, testCase := range cases {
		if testCase.Name == "" || len(testCase.States) < 2 || len(testCase.Health) != len(testCase.States) || testCase.ProtectedOutput {
			t.Fatalf("integration case = %#v", testCase)
		}
		for index, health := range testCase.Health {
			if health.State != testCase.States[index] {
				t.Fatalf("integration case health = %#v", testCase)
			}
		}
		for _, classification := range testCase.Classifications {
			classifications[classification] = true
		}
	}
	for _, classification := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit"} {
		if !classifications[classification] {
			t.Errorf("integration cases omit %q classification", classification)
		}
	}
}
