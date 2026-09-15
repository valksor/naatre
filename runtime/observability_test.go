package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestTelemetryEmitsSafeCausalLifecycleAndContainsHookFailures(t *testing.T) {
	t.Parallel()
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		return "raw-result-must-not-appear", nil
	})
	var mu sync.Mutex
	var traces []runtime.TelemetryEvent
	var metrics []runtime.MetricEvent
	var failures []runtime.TelemetryFailure
	options := runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		RequestID: "request-private-123", OperationID: "operation-private-456",
		SchemaRevision: "schema-7", PersistedHash: "sha256:approved", PrincipalReference: "principal-ref-9",
		Hooks: runtime.TelemetryHooks{
			Trace: func(event runtime.TelemetryEvent) error {
				mu.Lock()
				defer mu.Unlock()
				traces = append(traces, event)
				if event.Kind == runtime.TelemetryHandler && event.Stage == runtime.TelemetryStarted {
					panic("trace hook panic failure-secret")
				}
				return nil
			},
			Metric: func(event runtime.MetricEvent) error {
				mu.Lock()
				defer mu.Unlock()
				metrics = append(metrics, event)
				return errors.New("metric exporter unavailable failure-secret")
			},
			Failure: func(failure runtime.TelemetryFailure) {
				mu.Lock()
				defer mu.Unlock()
				failures = append(failures, failure)
			},
		},
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "raw-principal-must-not-appear"})
	outcome := plan.ExecuteWith(ctx, options)
	if len(outcome.Errors) != 0 || outcome.Data["read"] != "raw-result-must-not-appear" {
		t.Fatalf("telemetry changed outcome: %#v", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	wantLifecycle := observabilityFixtureEvents(t, "serial-operation-lifecycle")
	gotLifecycle := make([]string, 0, len(traces))
	for _, event := range traces {
		gotLifecycle = append(gotLifecycle, string(event.Kind)+"."+string(event.Stage))
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"raw-result-must-not-appear", "raw-principal-must-not-appear"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("telemetry event leaked %q: %s", secret, encoded)
			}
		}
	}
	if !slices.Equal(gotLifecycle, wantLifecycle) {
		t.Fatalf("lifecycle = %v, want %v", gotLifecycle, wantLifecycle)
	}
	if traces[1].ParentID != traces[0].ID || traces[2].ParentID != traces[1].ID || traces[3].ID != traces[2].ID {
		t.Fatalf("causal lifecycle is not linked: %#v", traces)
	}
	if traces[2].MemberKind != runtime.CallMember {
		t.Fatalf("operation/handler identity is incomplete: %#v", traces)
	}
	if len(metrics) != len(wantLifecycle) {
		t.Fatalf("metric events = %d, want %d", len(metrics), len(wantLifecycle))
	}
	for _, metric := range metrics {
		encoded, err := json.Marshal(metric)
		if err != nil {
			t.Fatal(err)
		}
		for _, unbounded := range []string{"request-private-123", "operation-private-456", "principal-ref-9", "Q", "read"} {
			if strings.Contains(string(encoded), unbounded) {
				t.Fatalf("metric labels contain unbounded value %q: %s", unbounded, encoded)
			}
		}
	}
	if len(failures) != len(metrics)+1 {
		t.Fatalf("telemetry failures = %d, want %d", len(failures), len(metrics)+1)
	}
	for _, failure := range failures {
		if strings.Contains(failure.Message, "failure-secret") {
			t.Fatalf("telemetry failure leaked hook payload: %#v", failure)
		}
	}
}

func TestPrepareTelemetryReportsPlanningOutcome(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	descriptor := runtime.Descriptor{
		Name: "read", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) { return "ok", nil })); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"read"}}]}]}}`)
	var events []runtime.TelemetryEvent
	plan, err := runtime.PrepareWithOptions(snapshot, request, runtime.PrepareOptions{Telemetry: runtime.TelemetryOptions{
		Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
			events = append(events, event)
			return nil
		}},
	}})
	if err != nil || plan == nil {
		t.Fatalf("PrepareWithOptions: plan=%v err=%v", plan, err)
	}
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, string(event.Kind)+"."+string(event.Stage))
	}
	if want := observabilityFixtureEvents(t, "planning-lifecycle"); !slices.Equal(got, want) {
		t.Fatalf("planning lifecycle = %v, want %v", got, want)
	}
	if events[0].ID == "" || events[0].ID != events[1].ID || events[0].ParentID != "" || events[1].ParentID != "" {
		t.Fatalf("standalone planning lifecycle is not self-correlated: %#v", events)
	}
	encoded, marshalErr := json.Marshal(events[1])
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !strings.Contains(string(encoded), `"cost":`) {
		t.Fatalf("planning completion omitted static cost: %s", encoded)
	}
}

type denyingAuthorizer struct{}

func (denyingAuthorizer) Authorize(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
	return runtime.AuthorizationDecision{Allowed: false}, nil
}

func TestDeniedMutationAuditCarriesSafeCorrelation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.WriteEffect)
	metadata.Transaction = runtime.TransactionRequired
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "changed", nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault, Authorizer: denyingAuthorizer{}}); err != nil {
		t.Fatal(err)
	}
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{rollback: func(context.Context) error { return nil }}, nil
	}}
	var audits []runtime.MutationAuditEvent
	if err := registry.ConfigureTransactions(runtime.TransactionConfig{Provider: provider, Audit: func(_ context.Context, event runtime.MutationAuditEvent) error {
		audits = append(audits, event)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"only","select":[{"$call":{"name":"write"}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "raw-subject"})
	var traces []runtime.TelemetryEvent
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		RequestID: "req-1", OperationID: "op-1", PrincipalReference: "principal-ref-1",
		Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
			traces = append(traces, event)
			return nil
		}},
	}})
	if len(outcome.Errors) == 0 || outcome.Errors[0].Code != runtime.CodeUnauthorized || calls.Load() != 0 {
		t.Fatalf("denied outcome = %#v, handler calls = %d", outcome, calls.Load())
	}
	stages := make([]runtime.MutationAuditStage, 0, len(audits))
	for _, event := range audits {
		stages = append(stages, event.Stage)
		if event.Group != "only" {
			t.Fatalf("audit group = %q, want only", event.Group)
		}
		if event.RequestID != "req-1" || event.OperationID != "op-1" || event.PrincipalReference != "principal-ref-1" {
			t.Fatalf("audit correlation = %#v", event)
		}
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(encoded), "raw-subject") {
			t.Fatalf("audit leaked principal: %s", encoded)
		}
	}
	if want := []runtime.MutationAuditStage{runtime.MutationAttempted, runtime.MutationDenied, runtime.MutationRolledBack}; !slices.Equal(stages, want) {
		t.Fatalf("audit stages = %v, want %v", stages, want)
	}
	var transactionEvents []runtime.TelemetryEvent
	for _, event := range traces {
		if event.Kind == runtime.TelemetryTransaction {
			transactionEvents = append(transactionEvents, event)
		}
	}
	if len(transactionEvents) != 3 {
		t.Fatalf("transaction telemetry = %#v", transactionEvents)
	}
	gotLifecycle := make([]string, 0, len(transactionEvents))
	for _, event := range transactionEvents {
		gotLifecycle = append(gotLifecycle, string(event.Kind)+"."+string(event.Stage))
	}
	if want := observabilityFixtureEvents(t, "mutation-denied-audit"); !slices.Equal(gotLifecycle, want) {
		t.Fatalf("denied lifecycle = %v, want %v", gotLifecycle, want)
	}
	for _, event := range transactionEvents[1:] {
		if event.ID != transactionEvents[0].ID || event.ParentID != transactionEvents[0].ParentID {
			t.Fatalf("transaction audit telemetry is not correlated: %#v", transactionEvents)
		}
	}
}

func TestCommittedMutationSurvivesTelemetryAndAuditHookFailures(t *testing.T) {
	t.Parallel()
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{commit: func(context.Context) (runtime.CommitOutcome, error) {
			return runtime.CommitApplied, nil
		}}, nil
	}}
	var auditCalls atomic.Int64
	plan := transactionPlan(t, provider, func(_ context.Context, event runtime.MutationAuditEvent) error {
		auditCalls.Add(1)
		if event.Stage == runtime.MutationCommitted {
			panic("audit panic secret")
		}
		return errors.New("audit write secret")
	}, operationAtomicDocument(), func(context.Context) (string, error) {
		return "committed", nil
	})
	var failures []runtime.TelemetryFailure
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{
		Trace: func(runtime.TelemetryEvent) error { return errors.New("trace secret") },
		Metric: func(runtime.MetricEvent) error {
			panic("metric secret")
		},
		Log: func(runtime.TelemetryEvent) error { return errors.New("log secret") },
		Failure: func(failure runtime.TelemetryFailure) {
			failures = append(failures, failure)
		},
	}}})
	if len(outcome.Errors) != 0 || outcome.Data["write"] != "committed" || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("hook failure changed committed outcome: %#v", outcome)
	}
	if auditCalls.Load() != 2 {
		t.Fatalf("audit calls = %d, want 2", auditCalls.Load())
	}
	var auditFailures, auditPanics int
	for _, failure := range failures {
		if failure.Pillar == "audit" {
			auditFailures++
			if failure.Panicked {
				auditPanics++
			}
		}
		if strings.Contains(failure.Message, "secret") {
			t.Fatalf("failure leaked hook payload: %#v", failure)
		}
	}
	if auditFailures != 2 || auditPanics != 1 {
		t.Fatalf("audit failures = %d with %d panic, want 2 with 1 panic; all failures = %#v", auditFailures, auditPanics, failures)
	}
}

func TestSubscriptionTelemetryUsesExplicitSafeReferences(t *testing.T) {
	t.Parallel()
	var events []runtime.TelemetryEvent
	var metrics []runtime.MetricEvent
	options := runtime.TelemetryOptions{
		RequestID: "req-stream", OperationID: "op-stream",
		Hooks: runtime.TelemetryHooks{
			Trace: func(event runtime.TelemetryEvent) error {
				events = append(events, event)
				return nil
			},
			Metric: func(event runtime.MetricEvent) error {
				metrics = append(metrics, event)
				return nil
			},
		},
	}
	runtime.EmitSubscriptionTelemetry(options, runtime.SubscriptionTelemetry{
		Stage: runtime.SubscriptionHistoryLost, StreamReference: "stream-ref", ConnectionAttempt: 2, ReplayAttempt: 1,
		SignalReference: "signal-2", ParentReference: "signal-1", Links: []string{"remote-root"},
		Outcome: runtime.TelemetryHistoryUnavailable, Duration: 3 * time.Second, Cost: 5, BatchSize: 7, Attempt: 11,
		ActiveStreams: 13, ReplayEvents: 17, ReplayBytes: 19, ScannedCandidates: 23, DrainRemaining: 29,
	})
	if len(events) != 1 || events[0].Kind != runtime.TelemetrySubscription || events[0].Stage != runtime.TelemetryStage(runtime.SubscriptionHistoryLost) {
		t.Fatalf("subscription telemetry = %#v", events)
	}
	if events[0].ID != "signal-2" || events[0].ParentID != "signal-1" || !slices.Equal(events[0].Links, []string{"remote-root"}) {
		t.Fatalf("subscription causal references = %#v", events[0])
	}
	encoded, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"raw-handle", "raw-cursor", "raw-topic"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("subscription event leaked %q: %s", raw, encoded)
		}
	}
	if len(metrics) != 1 {
		t.Fatalf("metric events = %d, want 1", len(metrics))
	}
	metric := metrics[0]
	if metric.Duration != 3*time.Second || metric.Cost != 5 || metric.BatchSize != 7 || metric.Attempt != 11 ||
		metric.ConnectionAttempt != 2 || metric.ReplayAttempt != 1 || metric.ActiveStreams != 13 ||
		metric.ReplayEvents != 17 || metric.ReplayBytes != 19 || metric.ScannedCandidates != 23 || metric.DrainRemaining != 29 {
		t.Fatalf("metric measurements = %#v", metric)
	}
	encodedMetric, err := json.Marshal(metric)
	if err != nil {
		t.Fatal(err)
	}
	for _, unbounded := range []string{"req-stream", "op-stream", "stream-ref", "signal-2", "signal-1", "remote-root"} {
		if strings.Contains(string(encodedMetric), unbounded) {
			t.Fatalf("metric contains unbounded reference %q: %s", unbounded, encodedMetric)
		}
	}
}

func TestTelemetryHooksReceiveIndependentLinkSlices(t *testing.T) {
	t.Parallel()
	var logged runtime.TelemetryEvent
	options := runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{
		Trace: func(event runtime.TelemetryEvent) error {
			event.Links[0] = "trace-mutated"
			return nil
		},
		Log: func(event runtime.TelemetryEvent) error {
			logged = event
			return nil
		},
	}}
	runtime.EmitSubscriptionTelemetry(options, runtime.SubscriptionTelemetry{
		Stage: runtime.SubscriptionEstablished, SignalReference: "signal", Links: []string{"remote-root"},
		Outcome: runtime.TelemetryActive,
	})
	if !slices.Equal(logged.Links, []string{"remote-root"}) {
		t.Fatalf("log hook links = %v, want isolated original", logged.Links)
	}
}

func TestSubscriptionTelemetryBoundsCallerControlledVocabulary(t *testing.T) {
	t.Parallel()
	var trace runtime.TelemetryEvent
	var metric runtime.MetricEvent
	options := runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{
		Trace: func(event runtime.TelemetryEvent) error {
			trace = event
			return nil
		},
		Metric: func(event runtime.MetricEvent) error {
			metric = event
			return nil
		},
	}}
	runtime.EmitSubscriptionTelemetry(options, runtime.SubscriptionTelemetry{
		Stage:     runtime.SubscriptionTelemetryStage("tenant-stage"),
		Outcome:   runtime.TelemetryOutcome("principal-outcome"),
		ErrorCode: "customer-secret-code",
	})
	if trace.Stage != runtime.TelemetryFailed || trace.Outcome != runtime.TelemetryFailedOutcome || trace.ErrorCode != runtime.CodeInternal {
		t.Fatalf("trace vocabulary was not normalized: %#v", trace)
	}
	if metric.Stage != runtime.TelemetryFailed || metric.Outcome != runtime.TelemetryFailedOutcome || metric.ErrorCode != runtime.CodeInternal {
		t.Fatalf("metric vocabulary was not normalized: %#v", metric)
	}
	runtime.EmitSubscriptionTelemetry(options, runtime.SubscriptionTelemetry{
		Stage: runtime.SubscriptionTelemetryStage(runtime.MutationCommitted), Outcome: runtime.TelemetryActive,
	})
	if trace.Stage != runtime.TelemetryFailed || trace.ErrorCode != runtime.CodeInternal {
		t.Fatalf("cross-kind subscription stage was not normalized: %#v", trace)
	}
}

func TestTelemetryIdentitiesAreUniqueAcrossExecutions(t *testing.T) {
	t.Parallel()
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		return "ok", nil
	})
	collect := func() []runtime.TelemetryEvent {
		var events []runtime.TelemetryEvent
		outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
			RequestID: "reused-request-reference", OperationID: "reused-operation-reference",
			Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
				events = append(events, event)
				return nil
			}},
		}})
		if len(outcome.Errors) != 0 {
			t.Fatalf("outcome = %#v", outcome)
		}
		return events
	}
	first, second := collect(), collect()
	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("event counts = %d and %d", len(first), len(second))
	}
	for index := range first {
		if first[index].ID == second[index].ID {
			t.Fatalf("event %d reused identity %q across executions", index, first[index].ID)
		}
	}
}

func TestBatchTelemetryPreservesDispatchLifecycle(t *testing.T) {
	t.Parallel()
	plan, _ := batchBenchmarkPlan(t, 0, true)
	var mu sync.Mutex
	var events []runtime.TelemetryEvent
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{
		Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
			if event.Kind == runtime.TelemetryBatch {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
			return nil
		}},
	}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("batch outcome errors = %#v", outcome.Errors)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Stage != runtime.TelemetryStarted || events[1].Stage != runtime.TelemetryCompleted {
		t.Fatalf("batch lifecycle = %#v", events)
	}
	gotLifecycle := make([]string, 0, len(events))
	for _, event := range events {
		gotLifecycle = append(gotLifecycle, string(event.Kind)+"."+string(event.Stage))
	}
	if want := observabilityFixtureEvents(t, "batched-handler-lifecycle"); !slices.Equal(gotLifecycle, want) {
		t.Fatalf("batch fixture lifecycle = %v, want %v", gotLifecycle, want)
	}
	if events[0].ID == "" || events[0].ID != events[1].ID || events[0].ParentID == "" || events[0].BatchSize != 64 {
		t.Fatalf("batch correlation = %#v", events)
	}
}

func TestParallelTelemetryMatchesCausalFixture(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(compositionTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var started atomic.Int64
	for _, name := range []string{"left", "right"} {
		registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
			Name: name, Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
		}, func(context.Context, runtime.Invocation) (string, error) {
			if started.Add(1) == 2 {
				close(release)
			}
			<-release
			return name, nil
		}))
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"left"}},{"$call":{"name":"right"}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []runtime.TelemetryEvent
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Telemetry: runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
		if event.Kind == runtime.TelemetryOperation || event.Kind == runtime.TelemetryHandler {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}
		return nil
	}}}})
	if len(outcome.Errors) != 0 {
		t.Fatalf("parallel outcome = %#v", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, string(event.Kind)+"."+string(event.Stage))
	}
	if want := observabilityFixtureEvents(t, "parallel-causal-links"); !slices.Equal(got, want) {
		t.Fatalf("parallel lifecycle = %v, want %v", got, want)
	}
	starts := map[string]bool{}
	for _, event := range events {
		if event.Kind == runtime.TelemetryHandler && event.Stage == runtime.TelemetryStarted {
			if event.ParentID != events[0].ID {
				t.Fatalf("handler parent = %q, want %q", event.ParentID, events[0].ID)
			}
			starts[event.ID] = true
		}
		if event.Kind == runtime.TelemetryHandler && event.Stage == runtime.TelemetryCompleted && !starts[event.ID] {
			t.Fatalf("handler completion has no matching start: %#v", event)
		}
	}
}

func TestSubscriptionTelemetryExpressesFixtureCausalLinks(t *testing.T) {
	t.Parallel()
	fixture := loadObservabilityFixture(t)
	for _, vector := range fixture.Vectors {
		if len(vector.Events) == 0 || !strings.HasPrefix(vector.Events[0], "subscription.") {
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			var events []runtime.TelemetryEvent
			options := runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
				events = append(events, event)
				return nil
			}}}
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
			got := make([]string, 0, len(events))
			for _, event := range events {
				got = append(got, string(event.Kind)+"."+string(event.Stage))
			}
			if !slices.Equal(got, vector.Events) {
				t.Fatalf("subscription lifecycle = %v, want %v", got, vector.Events)
			}
			for _, link := range vector.Links {
				if events[link.To].ParentID != events[link.From].ID && !slices.Contains(events[link.To].Links, events[link.From].ID) {
					t.Fatalf("link %#v not expressed by %#v", link, events)
				}
			}
		})
	}
}

func TestRetryTelemetryLinksEveryAttempt(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
		}
		return "ok", nil
	})
	var allEvents []runtime.TelemetryEvent
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{
		Retry: runtime.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		Telemetry: runtime.TelemetryOptions{Hooks: runtime.TelemetryHooks{Trace: func(event runtime.TelemetryEvent) error {
			allEvents = append(allEvents, event)
			return nil
		}}},
	})
	if len(outcome.Errors) != 0 || outcome.Data["read"] != "ok" {
		t.Fatalf("retry outcome = %#v", outcome)
	}
	var events, handlerStarts []runtime.TelemetryEvent
	var gotLifecycle []string
	for _, event := range allEvents {
		if event.Kind == runtime.TelemetryRetry {
			events = append(events, event)
		}
		if event.Kind == runtime.TelemetryHandler && event.Stage == runtime.TelemetryStarted {
			handlerStarts = append(handlerStarts, event)
		}
		if event.Kind == runtime.TelemetryRetry || event.Kind == runtime.TelemetryHandler {
			gotLifecycle = append(gotLifecycle, string(event.Kind)+"."+string(event.Stage))
		}
	}
	want := []string{"1.started", "1.failed", "2.started", "2.completed"}
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, fmt.Sprintf("%d.%s", event.Attempt, event.Stage))
		if event.ID == "" || event.ParentID == "" {
			t.Fatalf("retry event lacks causal identity: %#v", event)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("retry lifecycle = %v, want %v", got, want)
	}
	if fixtureWant := observabilityFixtureEvents(t, "retried-attempt-lifecycle"); !slices.Equal(gotLifecycle, fixtureWant) {
		t.Fatalf("retry fixture lifecycle = %v, want %v", gotLifecycle, fixtureWant)
	}
	if len(handlerStarts) != 2 || handlerStarts[0].ParentID != events[0].ID || handlerStarts[1].ParentID != events[2].ID {
		t.Fatalf("handler attempts are not linked to retries: retries=%#v handlers=%#v", events, handlerStarts)
	}
}
