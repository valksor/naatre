package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
)

func TestProcessAdmissionBoundsAggregateResourcesAndCleansQueue(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 1
	config.Limits.MaxQueuedBytes = 2
	config.Limits.MaxReservedBytes = 4
	controller := newStartedProcessController(t, config)

	active, err := controller.Admit(context.Background(), processRequest("tenant-a", runtime.WorkQuery, 3))
	if err != nil {
		t.Fatal(err)
	}
	queued := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })

	_, err = controller.Admit(context.Background(), processRequest("tenant-c", runtime.WorkQuery, 1))
	assertAdmissionCode(t, err, runtime.CodeOverloaded)
	active.Release()
	second := receiveAdmission(t, queued)
	if stats := controller.Stats(); stats.InFlight != 1 || stats.Queued != 0 || stats.ReservedBytes != 1 {
		t.Fatalf("stats after queued admission = %#v", stats)
	}
	second.Release()
	if stats := controller.Stats(); stats.InFlight != 0 || stats.ReservedBytes != 0 {
		t.Fatalf("released stats = %#v", stats)
	}
}

func TestManySmallAdmissionsCannotBypassMemoryOrResultBounds(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 8
	config.Limits.MaxReservedBytes = 3
	config.Limits.MaxResultBufferBytes = 3
	controller := newStartedProcessController(t, config)
	leases := make([]*runtime.WorkLease, 0, 3)
	for index := 0; index < 3; index++ {
		leases = append(leases, mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1)))
	}
	blocked := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	assertAdmissionBlocked(t, blocked)
	leases[0].Release()
	receiveAdmission(t, blocked).Release()
	for _, lease := range leases[1:] {
		lease.Release()
	}
}

func TestZeroResultWeightsCannotBypassAggregateBufferLimit(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 8
	config.Limits.MaxResultBufferBytes = 2
	controller := newStartedProcessController(t, config)
	request := processRequest("tenant-a", runtime.WorkQuery, 1)
	request.ResultBufferBytes = 0
	first := mustAdmit(t, controller, request)
	second := mustAdmit(t, controller, request)
	third := admitAsync(controller, request)
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	if stats := controller.Stats(); stats.ResultBufferBytes != 2 {
		t.Fatalf("zero-weight result accounting = %#v", stats)
	}
	first.Release()
	receiveAdmission(t, third).Release()
	second.Release()
}

func TestProcessAdmissionIsFairAcrossTenants(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 4
	controller := newStartedProcessController(t, config)
	active, err := controller.Admit(context.Background(), processRequest("tenant-a", runtime.WorkQuery, 1))
	if err != nil {
		t.Fatal(err)
	}
	aFirst := admitAsync(controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	aSecond := admitAsync(controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 2 })
	bFirst := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 3 })

	active.Release()
	bLease := receiveAdmission(t, bFirst)
	assertAdmissionBlocked(t, aFirst)
	assertAdmissionBlocked(t, aSecond)
	bLease.Release()
	aLease := receiveAdmission(t, aFirst)
	aLease.Release()
	receiveAdmission(t, aSecond).Release()
}

func TestProcessAdmissionIsFairAcrossPrincipalsWithinTenant(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 3
	config.Limits.MaxTenantInFlight = 2
	config.Limits.MaxPrincipalInFlight = 1
	controller := newStartedProcessController(t, config)
	request := processRequest("tenant-a", runtime.WorkQuery, 1)
	request.PrincipalReference = "principal-a"
	active := mustAdmit(t, controller, request)
	aQueued := admitAsync(controller, request)
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	request.PrincipalReference = "principal-b"
	bLease := receiveAdmission(t, admitAsync(controller, request))
	assertAdmissionBlocked(t, aQueued)
	bLease.Release()
	active.Release()
	receiveAdmission(t, aQueued).Release()
}

func TestPartitionLimitsReserveCapacityForOtherCallers(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 4
	config.Limits.MaxTenantInFlight = 4
	config.Limits.MaxPrincipalInFlight = 4
	controller := newStartedProcessController(t, config)
	active := make([]*runtime.WorkLease, 0, 3)
	for _, principal := range []string{"principal-a", "principal-b", "principal-c"} {
		request := processRequest("tenant-a", runtime.WorkQuery, 1)
		request.PrincipalReference = principal
		active = append(active, mustAdmit(t, controller, request))
	}
	aFourthRequest := processRequest("tenant-a", runtime.WorkQuery, 1)
	aFourthRequest.PrincipalReference = "principal-d"
	aFourth := admitAsync(controller, aFourthRequest)
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	bLease := mustAdmit(t, controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	assertAdmissionBlocked(t, aFourth)
	bLease.Release()
	active[0].Release()
	receiveAdmission(t, aFourth).Release()
	for _, lease := range active[1:] {
		lease.Release()
	}
}

func TestPrincipalActiveLimitReservesTenantCapacity(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 4
	config.Limits.MaxTenantInFlight = 4
	config.Limits.MaxPrincipalInFlight = 4
	controller := newStartedProcessController(t, config)
	request := processRequest("tenant-a", runtime.WorkQuery, 1)
	request.PrincipalReference = "principal-a"
	first := mustAdmit(t, controller, request)
	second := mustAdmit(t, controller, request)
	third := admitAsync(controller, request)
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	request.PrincipalReference = "principal-b"
	otherPrincipal := mustAdmit(t, controller, request)
	assertAdmissionBlocked(t, third)
	first.Release()
	receiveAdmission(t, third).Release()
	second.Release()
	otherPrincipal.Release()
}

func TestQueuePartitionLimitsReserveCapacityForOtherCallers(t *testing.T) {
	t.Parallel()
	t.Run("tenant", func(t *testing.T) {
		config := unsafeQueuePartitionConfig()
		controller := newStartedProcessController(t, config)
		active := mustAdmit(t, controller, processRequest("active", runtime.WorkQuery, 1))
		var queued []<-chan admissionResult
		for _, principal := range []string{"principal-a", "principal-b", "principal-c"} {
			request := processRequest("tenant-a", runtime.WorkQuery, 1)
			request.PrincipalReference = principal
			queued = append(queued, admitAsync(controller, request))
		}
		waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 3 })
		request := processRequest("tenant-a", runtime.WorkQuery, 1)
		request.PrincipalReference = "principal-d"
		_, err := controller.Admit(context.Background(), request)
		assertAdmissionCode(t, err, runtime.CodeRateLimited)
		queued = append(queued, admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1)))
		waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 4 })
		drainQueuedAdmissions(t, controller, active, queued)
	})
	t.Run("principal", func(t *testing.T) {
		config := unsafeQueuePartitionConfig()
		controller := newStartedProcessController(t, config)
		active := mustAdmit(t, controller, processRequest("active", runtime.WorkQuery, 1))
		request := processRequest("tenant-a", runtime.WorkQuery, 1)
		request.PrincipalReference = "principal-a"
		queued := []<-chan admissionResult{admitAsync(controller, request), admitAsync(controller, request)}
		waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 2 })
		_, err := controller.Admit(context.Background(), request)
		assertAdmissionCode(t, err, runtime.CodeRateLimited)
		request.PrincipalReference = "principal-b"
		queued = append(queued, admitAsync(controller, request))
		queued = append(queued, admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1)))
		waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 4 })
		drainQueuedAdmissions(t, controller, active, queued)
	})
}

func unsafeQueuePartitionConfig() runtime.ProcessConfig {
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 4
	config.Limits.MaxTenantQueued = 4
	config.Limits.MaxPrincipalQueued = 4
	return config
}

func drainQueuedAdmissions(t testing.TB, controller *runtime.ProcessController, active *runtime.WorkLease, queued []<-chan admissionResult) {
	t.Helper()
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	for _, result := range queued {
		got := <-result
		if got.lease != nil || got.err == nil {
			t.Fatalf("drained queued admission = %#v", got)
		}
	}
	active.Release()
	if outcome := receiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
}

func TestNestedFanOutSharesAggregateCapacityAndTenantFairness(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 4
	config.Limits.MaxTenantInFlight = 4
	controller := newStartedProcessController(t, config)
	other := mustAdmit(t, controller, processRequest("tenant-c", runtime.WorkQuery, 1))
	parentRequest := processRequest("tenant-a", runtime.WorkQuery, 1)
	parentRequest.PrincipalReference = "parent"
	parent := mustAdmit(t, controller, parentRequest)
	childOneRequest := processRequest("tenant-a", runtime.WorkQuery, 1)
	childOneRequest.PrincipalReference = "child-one"
	childOne := mustAdmit(t, controller, childOneRequest)
	childTwoRequest := processRequest("tenant-a", runtime.WorkQuery, 1)
	childTwoRequest.PrincipalReference = "child-two"
	childTwo := mustAdmit(t, controller, childTwoRequest)
	unrelated := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	nestedRequest := processRequest("tenant-a", runtime.WorkQuery, 1)
	nestedRequest.PrincipalReference = "nested"
	nested := admitAsync(controller, nestedRequest)
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.InFlight == 4 && stats.Queued == 2 })
	childOne.Release()
	unrelatedLease := receiveAdmission(t, unrelated)
	assertAdmissionBlocked(t, nested)
	unrelatedLease.Release()
	childTwo.Release()
	receiveAdmission(t, nested).Release()
	parent.Release()
	other.Release()
}

func TestNoisyTenantCannotFillEveryQueuePartition(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxTenantQueued = 1
	controller := newStartedProcessController(t, config)
	active := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	aQueued := admitAsync(controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	_, err := controller.Admit(context.Background(), processRequest("tenant-a", runtime.WorkQuery, 1))
	assertAdmissionCode(t, err, runtime.CodeRateLimited)
	bQueued := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 2 })
	active.Release()
	bLease := receiveAdmission(t, bQueued)
	bLease.Release()
	receiveAdmission(t, aQueued).Release()
}

func TestStreamAndRemoteWorkUseDedicatedProcessLimits(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 4
	config.Limits.MaxStreams = 1
	config.Limits.MaxRemoteConnections = 1
	controller := newStartedProcessController(t, config)
	stream := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkStream, 1))
	remote := mustAdmit(t, controller, processRequest("tenant-b", runtime.WorkRemote, 1))
	secondStream := admitAsync(controller, processRequest("tenant-c", runtime.WorkStream, 1))
	secondRemote := admitAsync(controller, processRequest("tenant-d", runtime.WorkRemote, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 2 })
	if stats := controller.Stats(); stats.Streams != 1 || stats.RemoteConnections != 1 {
		t.Fatalf("dedicated stats = %#v", stats)
	}
	stream.Release()
	remote.Release()
	receiveAdmission(t, secondStream).Release()
	receiveAdmission(t, secondRemote).Release()
}

func TestQueuedAdmissionCancellationReleasesEveryReservation(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	controller := newStartedProcessController(t, config)
	active, err := controller.Admit(context.Background(), processRequest("tenant-a", runtime.WorkQuery, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := admitAsyncContext(controller, ctx, processRequest("tenant-b", runtime.WorkQuery, 2))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.QueuedBytes == 2 })
	cancel()
	if got := <-result; !errors.Is(got.err, context.Canceled) || got.lease != nil {
		t.Fatalf("cancelled admission = %#v", got)
	}
	if stats := controller.Stats(); stats.Queued != 0 || stats.QueuedBytes != 0 {
		t.Fatalf("cancelled queue stats = %#v", stats)
	}
	active.Release()
}

func TestQueuedAdmissionHasOneTerminalOutcomeDuringDrainRace(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var stages []runtime.ProcessEventStage
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Hooks.Observe = func(event runtime.ProcessEvent) error {
		mu.Lock()
		stages = append(stages, event.Stage)
		mu.Unlock()
		return nil
	}
	controller := newStartedProcessController(t, config)
	active := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	ctx, cancel := context.WithCancel(context.Background())
	queued := admitAsyncContext(controller, ctx, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	start := make(chan struct{})
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { <-start; cancel() }()
	go func() { <-start; drained <- controller.Drain(context.Background()) }()
	close(start)
	result := <-queued
	if result.lease != nil || result.err == nil {
		t.Fatalf("raced queued admission = %#v", result)
	}
	active.Release()
	if outcome := receiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
	waitForProcessEvents(t, &mu, &stages, 7)
	mu.Lock()
	defer mu.Unlock()
	terminal := 0
	for _, stage := range stages {
		if stage == runtime.ProcessEventAdmissionRejected || stage == runtime.ProcessEventAdmissionCancelled {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("queued admission terminal events = %d in %v, want 1", terminal, stages)
	}
}

func TestProcessPartitionKeysAreBoundedTenantScopedAndCollisionSafe(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 3
	config.Limits.MaxPrincipalInFlight = 1
	controller := newStartedProcessController(t, config)
	request := processRequest("tenant-a", runtime.WorkQuery, 1)
	request.PrincipalReference = "shared-subject"
	first := mustAdmit(t, controller, request)
	otherTenant := processRequest("tenant-b", runtime.WorkQuery, 1)
	otherTenant.PrincipalReference = "shared-subject"
	second := mustAdmit(t, controller, otherTenant)
	samePartition := admitAsync(controller, request)
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	second.Release()
	assertAdmissionBlocked(t, samePartition)
	first.Release()
	receiveAdmission(t, samePartition).Release()

	empty := mustAdmit(t, controller, processRequest("", runtime.WorkQuery, 1))
	nul := mustAdmit(t, controller, processRequest("\x00", runtime.WorkQuery, 1))
	empty.Release()
	nul.Release()
	oversized := processRequest(strings.Repeat("x", runtime.ProcessPartitionReferenceMaxBytes+1), runtime.WorkQuery, 1)
	if lease, err := controller.Admit(context.Background(), oversized); err == nil || lease != nil {
		t.Fatalf("oversized partition reference = lease %#v, error %v", lease, err)
	}
	if lease, err := controller.Admit(context.Background(), runtime.AdmissionRequest{
		TenantReference: string([]byte{0xff}), Kind: runtime.WorkQuery,
	}); err == nil || lease != nil {
		t.Fatalf("invalid UTF-8 partition reference = lease %#v, error %v", lease, err)
	}
}

func TestCancelledContextCannotAcquireProcessResources(t *testing.T) {
	t.Parallel()
	controller := newStartedProcessController(t, runtime.DefaultProcessConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease, err := controller.Admit(ctx, processRequest("tenant-a", runtime.WorkQuery, 1))
	if lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled direct admission = lease %#v, error %v", lease, err)
	}
	if stats := controller.Stats(); stats.InFlight != 0 || stats.Queued != 0 {
		t.Fatalf("cancelled direct admission retained resources: %#v", stats)
	}
}

func TestCancelledUncooperativeWorkRetainsItsProcessSlot(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	controller := newStartedProcessController(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	active, err := controller.Admit(ctx, processRequest("tenant-a", runtime.WorkQuery, 1))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-active.Context().Done()
	next := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool {
		return stats.InFlight == 1 && stats.Queued == 1
	})
	assertAdmissionBlocked(t, next)
	active.Release()
	receiveAdmission(t, next).Release()
}

func TestProcessDrainCancelsConfiguredWorkAndFinishesOwnedWork(t *testing.T) {
	t.Parallel()
	controller := newStartedProcessController(t, runtime.DefaultProcessConfig())
	query := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	mutation := mustAdmit(t, controller, processRequest("tenant-b", runtime.WorkMutation, 1))
	stream := mustAdmit(t, controller, processRequest("tenant-c", runtime.WorkStream, 1))
	remote := mustAdmit(t, controller, processRequest("tenant-d", runtime.WorkRemote, 1))
	durable := mustAdmit(t, controller, processRequest("tenant-e", runtime.WorkDurable, 1))
	if err := mutation.SetPhase(runtime.WorkCommitting); err != nil {
		t.Fatal(err)
	}
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	waitForProcessState(t, controller, runtime.ProcessDraining)
	assertContextCancelled(t, query.Context())
	assertContextCancelled(t, stream.Context())
	assertContextCancelled(t, remote.Context())
	assertContextActive(t, mutation.Context())
	assertContextActive(t, durable.Context())
	query.Release()
	stream.Release()
	remote.Release()
	assertDrainBlocked(t, drained)
	mutation.Release()
	durable.Release()
	if outcome := receiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
	if health := controller.Health(); health.State != runtime.ProcessStopped || health.Ready || health.Live {
		t.Fatalf("stopped health = %#v", health)
	}
}

func TestProcessDrainRejectsQueuedAndNewAdmission(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	controller := newStartedProcessController(t, config)
	active := mustAdmit(t, controller, processRequest("tenant-active", runtime.WorkQuery, 1))
	queued := admitAsync(controller, processRequest("tenant-queued", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })

	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	waitForProcessState(t, controller, runtime.ProcessDraining)
	got := <-queued
	assertAdmissionCode(t, got.err, runtime.CodeOverloaded)
	if got.lease != nil || !errors.Is(got.err, runtime.ErrProcessDraining) {
		t.Fatalf("queued drain admission = %#v", got)
	}
	lease, err := controller.Admit(context.Background(), processRequest("tenant-new", runtime.WorkQuery, 1))
	if lease != nil {
		lease.Release()
	}
	assertAdmissionCode(t, err, runtime.CodeOverloaded)
	if !errors.Is(err, runtime.ErrProcessDraining) {
		t.Fatalf("new drain admission cause = %v", err)
	}
	active.Release()
	if outcome := receiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
}

func TestForcedDrainCancelsButDoesNotReleaseDurableOwnership(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller := newStartedProcessController(t, config)
	durable := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkDurable, 1))
	if outcome := controller.Drain(context.Background()); outcome != runtime.DrainForced {
		t.Fatalf("drain outcome = %q", outcome)
	}
	assertContextCancelled(t, durable.Context())
	if stats := controller.Stats(); stats.InFlight != 1 {
		t.Fatalf("forced drain released durable ownership: %#v", stats)
	}
	if health := controller.Health(); health.State != runtime.ProcessForced || health.Live {
		t.Fatalf("forced health = %#v", health)
	}
	durable.Release()
}

func TestDependencyFailureSeparatesReadinessFromLiveness(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	config := runtime.DefaultProcessConfig()
	config.Clock = func() time.Time { return now }
	config.DependencyFailureGrace = time.Minute
	config.Dependencies = []runtime.DependencyConfig{{Name: "primary-store", Essential: true, Ready: true}}
	controller := newStartedProcessController(t, config)
	if health := controller.Health(); !health.Ready || !health.Live {
		t.Fatalf("initial health = %#v", health)
	}
	if err := controller.SetDependency("primary-store", false); err != nil {
		t.Fatal(err)
	}
	if health := controller.Health(); health.Ready || !health.Live || health.EssentialUnavailable != 1 {
		t.Fatalf("transient dependency health = %#v", health)
	}
	now = now.Add(time.Minute)
	if health := controller.Health(); health.Ready || health.Live {
		t.Fatalf("sustained dependency health = %#v", health)
	}
	if err := controller.SetDependency("primary-store", true); err != nil {
		t.Fatal(err)
	}
	if health := controller.Health(); !health.Ready || !health.Live {
		t.Fatalf("recovered dependency health = %#v", health)
	}
}

func TestEssentialDependencyFailureStopsNewAdmission(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Dependencies = []runtime.DependencyConfig{{Name: "primary-store", Essential: true}}
	controller := newStartedProcessController(t, config)
	lease, err := controller.Admit(context.Background(), processRequest("tenant-a", runtime.WorkQuery, 1))
	if lease != nil {
		lease.Release()
	}
	assertAdmissionCode(t, err, runtime.CodeOverloaded)
}

func TestDependencyOutageHoldsQueuedAdmissionUntilRecovery(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Dependencies = []runtime.DependencyConfig{{Name: "primary-store", Essential: true, Ready: true}}
	controller := newStartedProcessController(t, config)
	active := mustAdmit(t, controller, processRequest("tenant-active", runtime.WorkQuery, 1))
	queued := admitAsync(controller, processRequest("tenant-queued", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })

	if err := controller.SetDependency("primary-store", false); err != nil {
		t.Fatal(err)
	}
	active.Release()
	assertAdmissionBlocked(t, queued)
	if err := controller.SetDependency("primary-store", true); err != nil {
		t.Fatal(err)
	}
	result := <-queued
	if result.err != nil || result.lease == nil {
		t.Fatalf("recovered queued admission = %#v", result)
	}
	result.lease.Release()
}

func TestProcessRevisionSwapPinsEveryAdmissionToOneSnapshot(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	controller, err := runtime.NewProcessController(config, processRevision("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	first := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	if err := controller.InstallRevision(processRevision("r2")); err != nil {
		t.Fatal(err)
	}
	second := mustAdmit(t, controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	if err := controller.RollbackRevision(processRevision("r1")); err != nil {
		t.Fatal(err)
	}
	third := mustAdmit(t, controller, processRequest("tenant-c", runtime.WorkQuery, 1))
	if got := []string{first.Revision().Revision, second.Revision().Revision, third.Revision().Revision}; !slices.Equal(got, []string{"r1", "r2", "r1"}) {
		t.Fatalf("pinned revisions = %v", got)
	}
	first.Release()
	second.Release()
	third.Release()
}

func TestProcessCleanupContextsAreIndependentAndBounded(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Cleanup = runtime.CleanupTimeouts{
		Rollback: 20 * time.Millisecond, LeaseRelease: 30 * time.Millisecond, SourceClose: 40 * time.Millisecond,
	}
	controller := newStartedProcessController(t, config)
	parent, cancelParent := context.WithCancel(context.Background())
	lease, err := controller.Admit(parent, processRequest("tenant-a", runtime.WorkMutation, 1))
	if err != nil {
		t.Fatal(err)
	}
	cancelParent()
	for cleanup, want := range map[runtime.CleanupKind]time.Duration{
		runtime.CleanupRollback:     config.Cleanup.Rollback,
		runtime.CleanupLeaseRelease: config.Cleanup.LeaseRelease,
		runtime.CleanupSourceClose:  config.Cleanup.SourceClose,
	} {
		ctx, cancel, err := lease.CleanupContext(cleanup)
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > want || time.Until(deadline) < want-10*time.Millisecond {
			t.Fatalf("%s cleanup deadline = %v, want about %v", cleanup, deadline, want)
		}
		if ctx.Err() != nil {
			t.Fatalf("%s cleanup inherited cancellation: %v", cleanup, ctx.Err())
		}
		cancel()
	}
	lease.Release()
}

func TestConnectionRotationIsFiniteStaggeredAndExpiryBounded(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.MaxConnectionLifetime = time.Hour
	config.ReconnectStagger = 10 * time.Minute
	controller := newStartedProcessController(t, config)
	established := time.Unix(1_800_000_000, 0)
	first := controller.ConnectionDeadline("connection-a", established, time.Time{})
	second := controller.ConnectionDeadline("connection-b", established, time.Time{})
	minimum := established.Add(50 * time.Minute)
	maximum := established.Add(time.Hour)
	if first.Before(minimum) || first.After(maximum) || second.Before(minimum) || second.After(maximum) || first.Equal(second) {
		t.Fatalf("staggered deadlines = %v and %v, bounds %v..%v", first, second, minimum, maximum)
	}
	expires := established.Add(5 * time.Minute)
	if got := controller.ConnectionDeadline("connection-a", established, expires); !got.Equal(expires) {
		t.Fatalf("identity-bounded deadline = %v, want %v", got, expires)
	}
}

func TestProcessLifecycleHookFailuresAreContained(t *testing.T) {
	t.Parallel()
	var failures atomic.Int64
	config := runtime.DefaultProcessConfig()
	config.Hooks = runtime.ProcessHooks{
		Observe: func(runtime.ProcessEvent) error { panic("sensitive hook panic") },
		Failure: func(runtime.ProcessHookFailure) { failures.Add(1) },
	}
	controller, err := runtime.NewProcessController(config, processRevision("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	lease := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	lease.Release()
	waitForAtomicCount(t, &failures, 3)
	if failures.Load() != 3 {
		t.Fatalf("contained lifecycle hook failures = %d, want start/admit/release", failures.Load())
	}
}

func TestProcessLifecycleHookReturnedErrorsAreContained(t *testing.T) {
	t.Parallel()
	var failures atomic.Int64
	var panics atomic.Int64
	config := runtime.DefaultProcessConfig()
	config.Hooks = runtime.ProcessHooks{
		Observe: func(runtime.ProcessEvent) error { return errors.New("private exporter failure") },
		Failure: func(failure runtime.ProcessHookFailure) {
			failures.Add(1)
			if failure.Panicked {
				panics.Add(1)
			}
		},
	}
	controller := newStartedProcessController(t, config)
	lease := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkQuery, 1))
	lease.Release()
	waitForAtomicCount(t, &failures, 3)
	if failures.Load() != 3 || panics.Load() != 0 {
		t.Fatalf("returned hook failures = %d with %d panics, want 3 with 0", failures.Load(), panics.Load())
	}
}

func TestProcessLifecycleHooksCannotBlockDrainOrDeadlockReentry(t *testing.T) {
	t.Parallel()
	blocked := make(chan struct{})
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	config.Dependencies = []runtime.DependencyConfig{{Name: "store", Essential: true, Ready: true}}
	var controller *runtime.ProcessController
	config.Hooks.Observe = func(event runtime.ProcessEvent) error {
		if event.Stage == runtime.ProcessEventStarted {
			return controller.SetDependency("store", true)
		}
		if event.Stage == runtime.ProcessEventDrainStarted {
			<-blocked
		}
		return nil
	}
	var err error
	controller, err = runtime.NewProcessController(config, processRevision("r1"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- controller.Start() }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(blocked)
		t.Fatal("process hook reentry deadlocked Start")
	}
	lease := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkDurable, 1))
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	if outcome := receiveDrain(t, drained); outcome != runtime.DrainForced {
		close(blocked)
		t.Fatalf("blocked-hook drain outcome = %q", outcome)
	}
	close(blocked)
	lease.Release()
}

func TestProcessLifecycleEventsAreOrderedAcrossHealthRevisionAndDrain(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var stages []runtime.ProcessEventStage
	config := runtime.DefaultProcessConfig()
	config.Dependencies = []runtime.DependencyConfig{{Name: "store", Essential: true, Ready: true}}
	config.Hooks.Observe = func(event runtime.ProcessEvent) error {
		mu.Lock()
		stages = append(stages, event.Stage)
		mu.Unlock()
		return nil
	}
	controller := newStartedProcessController(t, config)
	lease := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkMutation, 1))
	if err := controller.InstallRevision(processRevision("r2")); err != nil {
		t.Fatal(err)
	}
	if health := controller.Health(); !health.Ready || !health.Live {
		t.Fatalf("health during installed revision = %#v", health)
	}
	if err := controller.RollbackRevision(processRevision("r1")); err != nil {
		t.Fatal(err)
	}
	if health := controller.Health(); !health.Ready || !health.Live {
		t.Fatalf("health after revision rollback = %#v", health)
	}
	if err := controller.SetDependency("store", false); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetDependency("store", true); err != nil {
		t.Fatal(err)
	}
	drained := make(chan runtime.DrainOutcome, 1)
	go func() { drained <- controller.Drain(context.Background()) }()
	waitForProcessState(t, controller, runtime.ProcessDraining)
	lease.Release()
	if outcome := receiveDrain(t, drained); outcome != runtime.DrainCompleted {
		t.Fatalf("drain outcome = %q", outcome)
	}
	want := []runtime.ProcessEventStage{
		runtime.ProcessEventStarted, runtime.ProcessEventAdmissionGranted,
		runtime.ProcessEventRevisionInstalled, runtime.ProcessEventRevisionRolledBack,
		runtime.ProcessEventDependencyUnavailable, runtime.ProcessEventDependencyRecovered,
		runtime.ProcessEventDrainStarted, runtime.ProcessEventWorkReleased, runtime.ProcessEventDrainCompleted,
	}
	waitForProcessEvents(t, &mu, &stages, len(want))
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(stages, want) {
		t.Fatalf("process lifecycle stages = %v, want %v", stages, want)
	}
}

func TestProcessEventsDoNotSerializePartitionOrRevisionReferences(t *testing.T) {
	t.Parallel()
	const tenant = "private-tenant-reference"
	const principal = "private-principal-reference"
	const revision = "private-revision-reference"
	var mu sync.Mutex
	var encoded [][]byte
	config := runtime.DefaultProcessConfig()
	config.Hooks.Observe = func(event runtime.ProcessEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		mu.Lock()
		encoded = append(encoded, payload)
		mu.Unlock()
		return nil
	}
	controller, err := runtime.NewProcessController(config, runtime.ProcessRevision{
		Revision: revision, SchemaRevision: revision + "-schema", ConfigurationRevision: revision + "-config",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	lease, err := controller.Admit(context.Background(), runtime.AdmissionRequest{
		TenantReference: tenant, PrincipalReference: principal, Kind: runtime.WorkQuery,
		ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		complete := len(encoded) >= 3
		mu.Unlock()
		if complete {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(encoded) < 3 {
		t.Fatalf("process events = %d, want start/admit/release", len(encoded))
	}
	for _, payload := range encoded {
		for _, secret := range []string{tenant, principal, revision} {
			if strings.Contains(string(payload), secret) {
				t.Fatalf("process event leaked %q: %s", secret, payload)
			}
		}
	}
}

func TestProcessConfigurationRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*runtime.ProcessConfig){
		"negative-retry":    func(config *runtime.ProcessConfig) { config.RetryAfter = -time.Second },
		"negative-cleanup":  func(config *runtime.ProcessConfig) { config.Cleanup.Rollback = -time.Second },
		"invalid-drain":     func(config *runtime.ProcessConfig) { config.Drain.Query = "maybe" },
		"negative-lifetime": func(config *runtime.ProcessConfig) { config.MaxConnectionLifetime = -time.Second },
		"oversized-stagger": func(config *runtime.ProcessConfig) { config.ReconnectStagger = config.MaxConnectionLifetime },
		"unsafe-dependency": func(config *runtime.ProcessConfig) {
			config.Dependencies = []runtime.DependencyConfig{{Name: " unsafe"}}
		},
		"duplicate-dependency": func(config *runtime.ProcessConfig) {
			config.Dependencies = []runtime.DependencyConfig{{Name: "store"}, {Name: "store"}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := runtime.DefaultProcessConfig()
			mutate(&config)
			if controller, err := runtime.NewProcessController(config, processRevision("r1")); err == nil || controller != nil {
				t.Fatalf("NewProcessController accepted invalid config: controller=%#v err=%v", controller, err)
			}
		})
	}
	if controller, err := runtime.NewProcessController(runtime.DefaultProcessConfig(), runtime.ProcessRevision{}); err == nil || controller != nil {
		t.Fatalf("NewProcessController accepted invalid revision: controller=%#v err=%v", controller, err)
	}
}

func TestProcessRunRejectsOverloadBeforeBusinessCallback(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 1
	controller := newStartedProcessController(t, config)
	active := mustAdmit(t, controller, processRequest("tenant-active", runtime.WorkQuery, 1))
	queued := admitAsync(controller, processRequest("tenant-queued", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.Queued == 1 })
	var calls atomic.Int64
	err := controller.Run(context.Background(), processRequest("tenant-overloaded", runtime.WorkQuery, 1), func(context.Context, runtime.AdmittedWork) error {
		calls.Add(1)
		return nil
	})
	assertAdmissionCode(t, err, runtime.CodeOverloaded)
	if calls.Load() != 0 {
		t.Fatalf("business callback ran %d times before overload rejection", calls.Load())
	}
	active.Release()
	receiveAdmission(t, queued).Release()
}

func TestProcessRunRetainsSlotUntilBusinessCallbackReturns(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	controller := newStartedProcessController(t, config)
	entered := make(chan struct{})
	finish := make(chan struct{})
	wantErr := errors.New("business stopped")
	runResult := make(chan error, 1)
	go func() {
		runResult <- controller.Run(context.Background(), processRequest("tenant-a", runtime.WorkQuery, 1), func(ctx context.Context, work runtime.AdmittedWork) error {
			if ctx == nil || work.Revision().Revision != "r1" {
				return errors.New("admitted work is incomplete")
			}
			close(entered)
			<-finish
			return wantErr
		})
	}()
	<-entered
	queued := admitAsync(controller, processRequest("tenant-b", runtime.WorkQuery, 1))
	waitForProcessStats(t, controller, func(stats runtime.ProcessStats) bool { return stats.InFlight == 1 && stats.Queued == 1 })
	assertAdmissionBlocked(t, queued)
	close(finish)
	if err := <-runResult; !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want %v", err, wantErr)
	}
	receiveAdmission(t, queued).Release()
}

func TestProcessClockFailuresAreContainedWithoutLockingController(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Clock = func() time.Time { panic("private clock panic") }
	config.Dependencies = []runtime.DependencyConfig{{Name: "store", Essential: true}}
	if controller, err := runtime.NewProcessController(config, processRevision("r1")); err == nil || controller != nil {
		t.Fatalf("panicking startup clock = controller %#v, error %v", controller, err)
	}

	config.Dependencies[0].Ready = true
	controller, err := runtime.NewProcessController(config, processRevision("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	if health := controller.Health(); health.Ready || health.Live {
		t.Fatalf("panicking health clock did not fail closed: %#v", health)
	}
	if err := controller.SetDependency("store", false); err == nil {
		t.Fatal("panicking dependency clock was accepted")
	}
	lease, err := controller.Admit(context.Background(), runtime.AdmissionRequest{Kind: runtime.WorkQuery})
	if lease != nil {
		lease.Release()
		t.Fatal("admission remained open after dependency failure despite clock panic")
	}
	assertAdmissionCode(t, err, runtime.CodeOverloaded)
	if stats := controller.Stats(); stats.InFlight != 0 || stats.Queued != 0 {
		t.Fatalf("clock panic locked or mutated controller: %#v", stats)
	}
}

func TestProcessRuntimeConsumesOperationsFixture(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "conformance", "v1", "operations.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile          string                 `json:"profile"`
		States           []runtime.ProcessState `json:"states"`
		WorkKinds        []runtime.WorkKind     `json:"workKinds"`
		CleanupKinds     []runtime.CleanupKind  `json:"cleanupKinds"`
		RevisionSequence []string               `json:"revisionSequence"`
		Boundaries       map[string]bool        `json:"boundaries"`
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Profile != "operations.lifecycle-1" ||
		!slices.Equal(fixture.States, []runtime.ProcessState{runtime.ProcessStarting, runtime.ProcessReady, runtime.ProcessDraining, runtime.ProcessStopped, runtime.ProcessForced}) ||
		!slices.Equal(fixture.WorkKinds, []runtime.WorkKind{runtime.WorkQuery, runtime.WorkMutation, runtime.WorkStream, runtime.WorkRemote, runtime.WorkDurable}) ||
		!slices.Equal(fixture.CleanupKinds, []runtime.CleanupKind{runtime.CleanupRollback, runtime.CleanupLeaseRelease, runtime.CleanupSourceClose}) ||
		!slices.Equal(fixture.RevisionSequence, []string{"r1", "r2", "r1"}) {
		t.Fatalf("operations fixture header = %#v", fixture)
	}
	for _, boundary := range []string{
		"admissionBeforeBusinessExecution", "activeRevisionImmutable", "queuedCancellationReleasesAccounting", "uncooperativeWorkRetainsSlot",
		"partitionCapacityReserved",
	} {
		if !fixture.Boundaries[boundary] {
			t.Errorf("operations fixture boundary %q is not required", boundary)
		}
	}
	for _, boundary := range []string{"healthExposesProtectedMetadata", "hookEventsExposePartitionReferences"} {
		if fixture.Boundaries[boundary] {
			t.Errorf("operations fixture unsafe boundary %q is enabled", boundary)
		}
	}
}

func TestProcessForcedDrainEmitsOrderedTerminalEvent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var stages []runtime.ProcessEventStage
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = time.Millisecond
	config.Hooks.Observe = func(event runtime.ProcessEvent) error {
		mu.Lock()
		stages = append(stages, event.Stage)
		mu.Unlock()
		return nil
	}
	controller := newStartedProcessController(t, config)
	lease := mustAdmit(t, controller, processRequest("tenant-a", runtime.WorkDurable, 1))
	if outcome := controller.Drain(context.Background()); outcome != runtime.DrainForced {
		t.Fatalf("drain outcome = %q", outcome)
	}
	lease.Release()
	want := []runtime.ProcessEventStage{
		runtime.ProcessEventStarted, runtime.ProcessEventAdmissionGranted,
		runtime.ProcessEventDrainStarted, runtime.ProcessEventDrainForced,
		runtime.ProcessEventWorkReleased,
	}
	waitForProcessEvents(t, &mu, &stages, len(want))
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(stages, want) {
		t.Fatalf("forced lifecycle stages = %v, want %v", stages, want)
	}
}

type admissionResult struct {
	lease *runtime.WorkLease
	err   error
}

func newStartedProcessController(t testing.TB, config runtime.ProcessConfig) *runtime.ProcessController {
	t.Helper()
	controller, err := runtime.NewProcessController(config, processRevision("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	return controller
}

func processRevision(name string) runtime.ProcessRevision {
	return runtime.ProcessRevision{Revision: name, SchemaRevision: "schema-" + name, ConfigurationRevision: "config-" + name}
}

func processRequest(tenant string, kind runtime.WorkKind, bytes uint64) runtime.AdmissionRequest {
	return runtime.AdmissionRequest{
		TenantReference: tenant, PrincipalReference: tenant + "-principal", Kind: kind,
		ReservedBytes: bytes, QueueBytes: bytes, ResultBufferBytes: 1,
	}
}

func admitAsync(controller *runtime.ProcessController, request runtime.AdmissionRequest) <-chan admissionResult {
	return admitAsyncContext(controller, context.Background(), request)
}

func admitAsyncContext(controller *runtime.ProcessController, ctx context.Context, request runtime.AdmissionRequest) <-chan admissionResult {
	result := make(chan admissionResult, 1)
	go func() {
		lease, err := controller.Admit(ctx, request)
		result <- admissionResult{lease: lease, err: err}
	}()
	return result
}

func mustAdmit(t testing.TB, controller *runtime.ProcessController, request runtime.AdmissionRequest) *runtime.WorkLease {
	t.Helper()
	lease, err := controller.Admit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func receiveAdmission(t testing.TB, result <-chan admissionResult) *runtime.WorkLease {
	t.Helper()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.lease
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for admission")
		return nil
	}
}

func assertAdmissionBlocked(t testing.TB, result <-chan admissionResult) {
	t.Helper()
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case got := <-result:
		t.Fatalf("admission unexpectedly completed: %#v", got)
	case <-timer.C:
	}
}

func assertAdmissionCode(t testing.TB, err error, code string) {
	t.Helper()
	var admission *runtime.AdmissionError
	if !errors.As(err, &admission) || admission.Code != code || admission.RetryAfter <= 0 {
		t.Fatalf("admission error = %#v, want %q with retry hint", err, code)
	}
}

func waitForProcessStats(t testing.TB, controller *runtime.ProcessController, ready func(runtime.ProcessStats) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready(controller.Stats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process stats did not reach expected state: %#v", controller.Stats())
}

func waitForProcessState(t testing.TB, controller *runtime.ProcessController, state runtime.ProcessState) {
	t.Helper()
	waitForProcessStats(t, controller, func(runtime.ProcessStats) bool { return controller.Health().State == state })
}

func waitForProcessEvents(t testing.TB, mu *sync.Mutex, stages *[]runtime.ProcessEventStage, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		complete := len(*stages) >= count
		mu.Unlock()
		if complete {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("process events = %v, want at least %d", *stages, count)
}

func waitForAtomicCount(t testing.TB, count *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("atomic count = %d, want at least %d", count.Load(), want)
}

func assertContextCancelled(t testing.TB, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled")
	}
}

func assertContextActive(t testing.TB, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("context unexpectedly cancelled: %v", context.Cause(ctx))
	default:
	}
}

func assertDrainBlocked(t testing.TB, result <-chan runtime.DrainOutcome) {
	t.Helper()
	select {
	case outcome := <-result:
		t.Fatalf("drain unexpectedly completed: %q", outcome)
	default:
	}
}

func receiveDrain(t testing.TB, result <-chan runtime.DrainOutcome) runtime.DrainOutcome {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for drain")
		return ""
	}
}
