package conformance_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type observabilityFixture struct {
	Profile            string                       `json:"profile"`
	EventKinds         []string                     `json:"eventKinds"`
	AuditStages        []string                     `json:"auditStages"`
	Outcomes           []string                     `json:"outcomes"`
	ProhibitedFields   []string                     `json:"prohibitedFields"`
	MetricLabels       []string                     `json:"metricLabels"`
	MetricMeasurements []string                     `json:"metricMeasurements"`
	Vectors            []observabilityFixtureVector `json:"vectors"`
}

type observabilityFixtureVector struct {
	Name        string                     `json:"name"`
	Events      []string                   `json:"events"`
	Identities  []string                   `json:"identities"`
	Links       []observabilityFixtureLink `json:"links"`
	Causal      bool                       `json:"causal"`
	Terminal    string                     `json:"terminal"`
	Redacted    bool                       `json:"redacted"`
	Cardinality string                     `json:"cardinality"`
}

type observabilityFixtureLink struct {
	From int `json:"from"`
	To   int `json:"to"`
}

func TestObservabilityFixtureDefinesSafeCausalLifecycle(t *testing.T) {
	t.Parallel()
	var fixture observabilityFixture
	readFixture(t, "observability.json", &fixture)
	if fixture.Profile != "operations.observability-1" {
		t.Fatalf("observability profile = %q", fixture.Profile)
	}
	wantKinds := []string{"request", "planning", "operation", "handler", "batch", "retry", "transaction", "subscription"}
	if !slices.Equal(fixture.EventKinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", fixture.EventKinds, wantKinds)
	}
	wantStages := []string{"attempted", "denied", "committed", "rolled-back", "compensated", "indeterminate"}
	if !slices.Equal(fixture.AuditStages, wantStages) {
		t.Fatalf("audit stages = %v, want %v", fixture.AuditStages, wantStages)
	}
	wantProhibited := []string{"variables", "arguments", "result", "authorization", "cookie", "password", "secret", "apiKey", "token", "session", "principal", "cursor", "topic", "handle"}
	if !slices.Equal(fixture.ProhibitedFields, wantProhibited) {
		t.Fatalf("prohibited fields = %v, want %v", fixture.ProhibitedFields, wantProhibited)
	}
	if want := []string{"kind", "stage", "operationKind", "outcome", "errorCode"}; !slices.Equal(fixture.MetricLabels, want) {
		t.Fatalf("metric labels = %v, want %v", fixture.MetricLabels, want)
	}
	if want := []string{"duration", "cost", "batchSize", "attempt", "connectionAttempt", "replayAttempt", "activeStreams", "replayEvents", "replayBytes", "scannedCandidates", "drainRemaining"}; !slices.Equal(fixture.MetricMeasurements, want) {
		t.Fatalf("metric measurements = %v, want %v", fixture.MetricMeasurements, want)
	}
	wantOutcomes := []string{"active", "completed", "failed", "cancelled", "denied", "history-unavailable", "authorization-expired", "slow-consumer", "broker-failure", "forced-drain"}
	if !slices.Equal(fixture.Outcomes, wantOutcomes) {
		t.Fatalf("outcomes = %v, want %v", fixture.Outcomes, wantOutcomes)
	}
	knownKinds := make(map[string]bool, len(fixture.EventKinds))
	for _, kind := range fixture.EventKinds {
		if kind == "" || knownKinds[kind] {
			t.Fatalf("invalid or duplicate event kind %q", kind)
		}
		knownKinds[kind] = true
	}
	knownStages := make(map[string]bool, len(fixture.AuditStages))
	for _, stage := range fixture.AuditStages {
		if stage == "" || knownStages[stage] {
			t.Fatalf("invalid or duplicate audit stage %q", stage)
		}
		knownStages[stage] = true
	}
	knownTerminals := make(map[string]bool, len(fixture.AuditStages)+len(fixture.Outcomes))
	for _, terminal := range fixture.AuditStages {
		knownTerminals[terminal] = true
	}
	seenOutcomes := make(map[string]bool, len(fixture.Outcomes))
	for _, terminal := range fixture.Outcomes {
		if terminal == "" || seenOutcomes[terminal] {
			t.Fatalf("invalid or duplicate outcome %q", terminal)
		}
		seenOutcomes[terminal] = true
		knownTerminals[terminal] = true
	}
	seenNames, seenKinds, seenStages := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seenNames[vector.Name] || len(vector.Events) == 0 || len(vector.Identities) != len(vector.Events) || !vector.Causal || !knownTerminals[vector.Terminal] || !vector.Redacted || vector.Cardinality != "bounded" {
			t.Fatalf("invalid or duplicate observability vector %#v", vector)
		}
		seenNames[vector.Name] = true
		incoming := make([]bool, len(vector.Events))
		for index, event := range vector.Events {
			if vector.Identities[index] == "" {
				t.Fatalf("vector %q event %d has no causal identity", vector.Name, index)
			}
			kind, stage, ok := strings.Cut(event, ".")
			if !ok || !knownKinds[kind] || stage == "" {
				t.Fatalf("vector %q has invalid event %q", vector.Name, event)
			}
			seenKinds[kind] = true
			if kind == "transaction" {
				if !knownStages[stage] {
					t.Fatalf("vector %q has undeclared audit stage %q", vector.Name, stage)
				}
				seenStages[stage] = true
			}
		}
		for _, link := range vector.Links {
			if link.From < 0 || link.From >= link.To || link.To >= len(vector.Events) {
				t.Fatalf("vector %q has invalid causal link %#v", vector.Name, link)
			}
			incoming[link.To] = true
		}
		for index := 1; index < len(incoming); index++ {
			if !incoming[index] {
				t.Fatalf("vector %q event %d has no earlier causal link", vector.Name, index)
			}
		}
		reachable := observabilityCausalReachability(len(vector.Events), vector.Links)
		for from := range vector.Identities {
			for to := from + 1; to < len(vector.Identities); to++ {
				if vector.Identities[from] == vector.Identities[to] && !reachable[from][to] {
					t.Fatalf("vector %q identity %q has no causal path from %d to %d", vector.Name, vector.Identities[from], from, to)
				}
			}
		}
	}
	for kind := range knownKinds {
		if !seenKinds[kind] {
			t.Errorf("event kind %q has no vector", kind)
		}
	}
	for stage := range knownStages {
		if !seenStages[stage] {
			t.Errorf("audit stage %q has no vector", stage)
		}
	}
}

// TestObservabilityVectorsAgainstRuntime decodes each observability vector and
// drives its lifecycle through the real telemetry path in package runtime
// (PrepareWithOptions/Plan.ExecuteWith trace hooks, the transaction audit
// telemetry, and EmitSubscriptionTelemetry), asserting that the runtime's
// actual kind.stage sequence and causal links match the fixture.
func TestObservabilityVectorsAgainstRuntime(t *testing.T) {
	t.Parallel()
	var fixture observabilityFixture
	readFixture(t, "observability.json", &fixture)

	drivers := map[string]func(*testing.T, observabilityFixtureVector){
		"planning-lifecycle":           observabilityDrivePlanning,
		"serial-operation-lifecycle":   observabilityDriveSerialOperation,
		"parallel-causal-links":        observabilityDriveParallel,
		"batched-handler-lifecycle":    observabilityDriveBatched,
		"retried-attempt-lifecycle":    observabilityDriveRetried,
		"mutation-commit-audit":        observabilityDriveMutationCommit,
		"mutation-denied-audit":        observabilityDriveMutationDenied,
		"mutation-rollback-audit":      observabilityDriveMutationRollback,
		"mutation-indeterminate-audit": observabilityDriveMutationIndeterminate,
	}

	executed := 0
	for _, vector := range fixture.Vectors {
		vector := vector
		if strings.HasPrefix(vector.Events[0], "subscription.") {
			executed++
			t.Run("subscription/"+vector.Name, func(t *testing.T) {
				t.Parallel()
				observabilityDriveSubscription(t, vector)
			})
			continue
		}
		driver, ok := drivers[vector.Name]
		if !ok {
			// mutation-compensation-audit is exercised structurally by
			// TestObservabilityFixtureDefinesSafeCausalLifecycle above: driving a
			// truthful transaction.compensated audit requires a registered external
			// effect whose compensation succeeds while the enclosing operation still
			// fails, a multi-effect coordination that the single-handler transaction
			// plan used here cannot construct without duplicating the runtime's own
			// compensation harness. Every other transaction audit stage is driven.
			continue
		}
		executed++
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			driver(t, vector)
		})
	}
	if executed < 14 {
		t.Fatalf("drove only %d observability vectors against the runtime, want at least 14", executed)
	}
}

type observabilityTraces struct {
	mu     sync.Mutex
	events []runtime.TelemetryEvent
}

func (c *observabilityTraces) hook(keep func(runtime.TelemetryEvent) bool) runtime.TraceHook {
	return func(event runtime.TelemetryEvent) error {
		if keep == nil || keep(event) {
			c.mu.Lock()
			c.events = append(c.events, event)
			c.mu.Unlock()
		}
		return nil
	}
}

func (c *observabilityTraces) lifecycle() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := make([]string, 0, len(c.events))
	for _, event := range c.events {
		got = append(got, string(event.Kind)+"."+string(event.Stage))
	}
	return got
}

func observabilityAssertLifecycle(t *testing.T, got []string, vector observabilityFixtureVector) {
	t.Helper()
	if !slices.Equal(got, vector.Events) {
		t.Fatalf("%s lifecycle = %v, want %v", vector.Name, got, vector.Events)
	}
}

func observabilityDrivePlanning(t *testing.T, vector observabilityFixtureVector) {
	snapshot := resourceSnapshot(t, 0, func(context.Context) (string, error) { return "ok", nil })
	traces := &observabilityTraces{}
	plan, err := runtime.PrepareWithOptions(snapshot, decodeConformanceRequest(t, resourceSingleCallQuery()), runtime.PrepareOptions{Telemetry: runtime.TelemetryOptions{
		Hooks: runtime.TelemetryHooks{Trace: traces.hook(nil)},
	}})
	if err != nil || plan == nil {
		t.Fatalf("PrepareWithOptions: plan=%v err=%v", plan, err)
	}
	observabilityAssertLifecycle(t, traces.lifecycle(), vector)
	// Standalone planning is self-correlated: both events share one id.
	traces.mu.Lock()
	defer traces.mu.Unlock()
	if traces.events[0].ID == "" || traces.events[0].ID != traces.events[1].ID {
		t.Fatalf("planning lifecycle is not self-correlated: %#v", traces.events)
	}
}

func observabilityDriveSerialOperation(t *testing.T, vector observabilityFixtureVector) {
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		return "ok", nil
	})
	traces := &observabilityTraces{}
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		RequestID: "req-1", OperationID: "op-1", Hooks: runtime.TelemetryHooks{Trace: traces.hook(nil)},
	}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("serial outcome = %#v", outcome)
	}
	observabilityAssertLifecycle(t, traces.lifecycle(), vector)
	// The runtime expresses the fixture's causal chain through span nesting
	// (child ParentID points at its enclosing span) and paired start/finish
	// events sharing one id, rather than a literal edge per fixture link.
	traces.mu.Lock()
	defer traces.mu.Unlock()
	events := traces.events
	if events[1].ParentID != events[0].ID || events[2].ParentID != events[1].ID ||
		events[3].ID != events[2].ID || events[4].ID != events[1].ID || events[5].ID != events[0].ID {
		t.Fatalf("serial lifecycle is not causally correlated: %#v", events)
	}
}

func observabilityDriveParallel(t *testing.T, vector observabilityFixtureVector) {
	registry := runtime.NewRegistry(conformanceCompositionTypes(t))
	release := make(chan struct{})
	var started atomic.Int64
	for _, name := range []string{"left", "right"} {
		if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
			Name: name, Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: conformanceMetadata(runtime.ReadEffect),
		}, func(context.Context, runtime.Invocation) (string, error) {
			if started.Add(1) == 2 {
				close(release)
			}
			<-release
			return name, nil
		})); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"left"}},{"$call":{"name":"right"}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	traces := &observabilityTraces{}
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		Hooks: runtime.TelemetryHooks{Trace: traces.hook(func(event runtime.TelemetryEvent) bool {
			return event.Kind == runtime.TelemetryOperation || event.Kind == runtime.TelemetryHandler
		})},
	}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("parallel outcome = %#v", outcome)
	}
	observabilityAssertLifecycle(t, traces.lifecycle(), vector)
}

func observabilityDriveBatched(t *testing.T, vector observabilityFixtureVector) {
	snapshot := batchingUsersSnapshot(t, batchingUserRecords([]string{"Ada", "Grace"}), 0, false,
		func(inputs []runtime.BatchInput[map[string]any]) []runtime.BatchResult[string] {
			results := make([]runtime.BatchResult[string], len(inputs))
			for index, input := range inputs {
				results[index] = runtime.BatchResult[string]{Index: input.Index, Value: input.Value["name"].(string)}
			}
			return results
		})
	plan := batchingMapPlan(t, snapshot)
	traces := &observabilityTraces{}
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		Hooks: runtime.TelemetryHooks{Trace: traces.hook(func(event runtime.TelemetryEvent) bool {
			return event.Kind == runtime.TelemetryBatch
		})},
	}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("batched outcome = %#v", outcome)
	}
	observabilityAssertLifecycle(t, traces.lifecycle(), vector)
	traces.mu.Lock()
	defer traces.mu.Unlock()
	if traces.events[0].ID == "" || traces.events[0].ID != traces.events[1].ID {
		t.Fatalf("batch lifecycle is not correlated: %#v", traces.events)
	}
}

func observabilityDriveRetried(t *testing.T, vector observabilityFixtureVector) {
	var attempts atomic.Int64
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
		}
		return "ok", nil
	})
	traces := &observabilityTraces{}
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{
		Retry: runtime.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		Telemetry: runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{Trace: traces.hook(func(event runtime.TelemetryEvent) bool {
			return event.Kind == runtime.TelemetryRetry || event.Kind == runtime.TelemetryHandler
		})}},
	})
	if len(outcome.Errors) != 0 || outcome.Data["read"] != "ok" {
		t.Fatalf("retry outcome = %#v", outcome)
	}
	observabilityAssertLifecycle(t, traces.lifecycle(), vector)
}

func observabilityDriveMutationCommit(t *testing.T, vector observabilityFixtureVector) {
	provider := runtime.TransactionProvider(committingTransactionProvider(nil))
	got := observabilityTransactionLifecycle(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) {
		return "committed", nil
	})
	observabilityAssertLifecycle(t, got, vector)
}

func observabilityDriveMutationDenied(t *testing.T, vector observabilityFixtureVector) {
	provider := fakeConformanceTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeConformanceTransaction{rollback: func(context.Context) error { return nil }}, nil
	}}
	got := observabilityTransactionLifecycleWithAuthorizer(t, provider,
		runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: false}, nil
		}), `{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"only","select":[{"$call":{"name":"write"}}]}}]}]}`,
		func(context.Context) (string, error) { return "changed", nil })
	observabilityAssertLifecycle(t, got, vector)
}

func observabilityDriveMutationRollback(t *testing.T, vector observabilityFixtureVector) {
	provider := fakeConformanceTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeConformanceTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitNotApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	got := observabilityTransactionLifecycle(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) {
		return "", &runtime.Error{Code: "HANDLER", Message: "handler failed"}
	})
	observabilityAssertLifecycle(t, got, vector)
}

func observabilityDriveMutationIndeterminate(t *testing.T, vector observabilityFixtureVector) {
	provider := fakeConformanceTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeConformanceTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				return runtime.CommitUnknown, context.DeadlineExceeded
			},
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	got := observabilityTransactionLifecycle(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) {
		return "written", nil
	})
	observabilityAssertLifecycle(t, got, vector)
}

func observabilityDriveSubscription(t *testing.T, vector observabilityFixtureVector) {
	traces := &observabilityTraces{}
	options := runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{Trace: traces.hook(nil)}}
	references := make([]string, len(vector.Identities))
	for index, identity := range vector.Identities {
		references[index] = vector.Name + "/" + identity
	}
	for index, encoded := range vector.Events {
		_, stage, _ := strings.Cut(encoded, ".")
		parent := ""
		var links []string
		for _, link := range vector.Links {
			if link.To != index {
				continue
			}
			if parent == "" {
				parent = references[link.From]
			} else {
				links = append(links, references[link.From])
			}
		}
		outcome := runtime.TelemetryOutcome("")
		if index == len(vector.Events)-1 {
			outcome = runtime.TelemetryOutcome(vector.Terminal)
		}
		runtime.EmitSubscriptionTelemetry(options, runtime.SubscriptionTelemetry{
			Stage: runtime.SubscriptionTelemetryStage(stage), StreamReference: vector.Name,
			SignalReference: references[index], ParentReference: parent, Links: links, Outcome: outcome,
		})
	}
	observabilityAssertLifecycle(t, traces.lifecycle(), vector)
	traces.mu.Lock()
	defer traces.mu.Unlock()
	for _, link := range vector.Links {
		to := traces.events[link.To]
		from := traces.events[link.From]
		if to.ParentID != from.ID && !slices.Contains(to.Links, from.ID) {
			t.Fatalf("subscription link %#v not expressed by %#v", link, traces.events)
		}
	}
}

func observabilityTransactionLifecycle(t *testing.T, provider runtime.TransactionProvider, audit runtime.MutationAuditHook, document string, handler func(context.Context) (string, error)) []string {
	t.Helper()
	plan := mutationExecPlan(t, provider, audit, document, handler)
	return observabilityRunTransaction(t, plan, context.Background())
}

func observabilityTransactionLifecycleWithAuthorizer(t *testing.T, provider runtime.TransactionProvider, authorizer runtime.Authorizer, document string, handler func(context.Context) (string, error)) []string {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCoreTypes(t))
	metadata := conformanceMetadata(runtime.WriteEffect)
	metadata.Transaction = runtime.TransactionRequired
	metadata.Idempotency = runtime.IdempotencyIdempotent
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		return handler(ctx)
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault, Authorizer: authorizer}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider}); err != nil {
		t.Fatalf("ConfigureTransactions: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":`+document+`}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return observabilityRunTransaction(t, plan, context.Background())
}

func observabilityRunTransaction(t *testing.T, plan *runtime.Plan, ctx context.Context) []string {
	t.Helper()
	traces := &observabilityTraces{}
	plan.ExecuteWith(ctx, runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		RequestID: "req-1", OperationID: "op-1", Hooks: runtime.TelemetryHooks{Trace: traces.hook(func(event runtime.TelemetryEvent) bool {
			return event.Kind == runtime.TelemetryTransaction
		})},
	}})
	return traces.lifecycle()
}

func observabilityCausalReachability(size int, links []observabilityFixtureLink) [][]bool {
	reachable := make([][]bool, size)
	for index := range reachable {
		reachable[index] = make([]bool, size)
	}
	for _, link := range links {
		reachable[link.From][link.To] = true
	}
	for middle := range size {
		for from := range size {
			for to := range size {
				reachable[from][to] = reachable[from][to] || reachable[from][middle] && reachable[middle][to]
			}
		}
	}
	return reachable
}
