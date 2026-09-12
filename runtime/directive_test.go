package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestCustomDirectiveCoercesArgumentsAndComposesInSourceOrder(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	var events []string
	var mutex sync.Mutex
	definition := runtime.DirectiveDefinition{
		Descriptor: schema.DirectiveDescriptor{
			ID: "vendor.audit", Name: "audit", Version: "1", Capability: "vendor.audit-1",
			Repeatable: true, Locations: []protocol.SelectionKind{protocol.CallSelection},
			Arguments: []schema.DirectiveArgumentDescriptor{{
				ID: "vendor.audit.level", Name: "level", Type: schema.TypeID(schema.Int32), Required: true,
			}},
			Phases: []schema.DirectivePhase{schema.DirectiveValidation, schema.DirectivePlanning, schema.DirectiveExecution},
			Effect: string(runtime.ReadEffect), Cost: 1, Deterministic: true, Compatibility: schema.ChangeDangerous,
		},
		Planner: runtime.DirectivePlannerFunc(func(input runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
			level, _ := input.Arguments.Value("level")
			mutex.Lock()
			events = append(events, "plan:"+fmt.Sprint(level.(int32)))
			mutex.Unlock()
			return runtime.DirectivePlanDecision{}, nil
		}),
		Wrapper: runtime.DirectiveExecutionWrapperFunc(func(_ context.Context, input runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
			level, _ := input.Arguments.Value("level")
			mutex.Lock()
			events = append(events, "before:"+fmt.Sprint(level.(int32)))
			mutex.Unlock()
			value, err := next()
			mutex.Lock()
			events = append(events, "after:"+fmt.Sprint(level.(int32)))
			mutex.Unlock()
			return value, err
		}),
	}
	if err := registry.RegisterDirective(definition); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		mutex.Lock()
		events = append(events, "handler")
		mutex.Unlock()
		return "ok", nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := frozenRegistry(t, registry)
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.audit-1"],"document":{"requires":["vendor.audit-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"audit","arguments":{"level":{"$literal":1}}},{"name":"audit","arguments":{"level":{"$literal":2}}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.audit-1": true}})
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	description := plan.Description()
	described := description.Nodes[0].Directives
	if len(described) != 2 || described[0].ID != "vendor.audit" || described[0].Version != "1" ||
		described[0].Capability != "vendor.audit-1" || described[0].DeclaredCost != 1 || described[0].AdditionalCost != 0 {
		t.Fatalf("described directives = %#v", described)
	}
	if len(description.Result.Fields) != 1 || !description.Result.Fields[0].Required {
		t.Fatalf("custom non-skipping directive made result optional: %#v", description.Result)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute: %#v", outcome.Errors)
	}
	mutex.Lock()
	defer mutex.Unlock()
	want := []string{"plan:1", "plan:2", "before:1", "before:2", "handler", "after:2", "after:1"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestDirectiveResponseAnnotationsAreOrderedAndVersioned(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	definition := runtime.DirectiveDefinition{
		Descriptor: schema.DirectiveDescriptor{
			ID: "vendor.note", Name: "note", Version: "2", Capability: "vendor.note-2",
			Repeatable: true, Locations: []protocol.SelectionKind{protocol.CallSelection},
			Arguments: []schema.DirectiveArgumentDescriptor{{
				ID: "vendor.note.text", Name: "text", Type: schema.TypeID(schema.String), Required: true,
			}},
			Phases: []schema.DirectivePhase{schema.DirectiveValidation, schema.DirectiveResponse},
			Effect: string(runtime.ReadEffect), Deterministic: true, Compatibility: schema.ChangeBehaviorOnly,
		},
		Annotator: runtime.DirectiveResponseAnnotatorFunc(func(input runtime.DirectiveResponseContext) (json.RawMessage, error) {
			value, _ := input.Arguments.Value("text")
			return json.Marshal(map[string]any{"text": value, "selection": input.Selection.Name})
		}),
	}
	if err := registry.RegisterDirective(definition); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "ok", nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot := frozenRegistry(t, registry)
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.note-2"],"document":{"requires":["vendor.note-2"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","as":"z","directives":[{"name":"note","arguments":{"text":{"$literal":"first"}}},{"name":"note","arguments":{"text":{"$literal":"second"}}}]}},{"$call":{"name":"text","as":"a","directives":[{"name":"note","arguments":{"text":{"$literal":"third"}}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.note-2": true}})
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute: %#v", outcome.Errors)
	}
	if len(outcome.Annotations) != 3 {
		t.Fatalf("annotations = %#v", outcome.Annotations)
	}
	if !slices.Equal(outcome.Annotations[0].Path, []any{"a"}) || string(outcome.Annotations[0].Value) != `{"selection":"text","text":"third"}` ||
		outcome.Annotations[1].ID != "vendor.note" || outcome.Annotations[1].Version != "2" || !slices.Equal(outcome.Annotations[1].Path, []any{"z"}) ||
		string(outcome.Annotations[1].Value) != `{"selection":"text","text":"first"}` || string(outcome.Annotations[2].Value) != `{"selection":"text","text":"second"}` {
		t.Fatalf("annotations = %#v", outcome.Annotations)
	}
}

func TestDirectiveArgumentsAreDetachedAcrossCallbacks(t *testing.T) {
	t.Parallel()
	descriptor := testDirectiveDescriptor("detached", "vendor.detached-1")
	descriptor.Arguments[0].Nullable = true
	descriptor.Phases = append(descriptor.Phases, schema.DirectiveExecution, schema.DirectiveResponse)
	definition := runtime.DirectiveDefinition{
		Descriptor: descriptor,
		Wrapper: runtime.DirectiveExecutionWrapperFunc(func(_ context.Context, input runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
			value, _ := input.Arguments.Value("level")
			*value.(*int32) = 99
			return next()
		}),
		Annotator: runtime.DirectiveResponseAnnotatorFunc(func(input runtime.DirectiveResponseContext) (json.RawMessage, error) {
			value, _ := input.Arguments.Value("level")
			return json.Marshal(*value.(*int32))
		}),
	}
	snapshot := directiveTestSnapshot(t, definition)
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.detached-1"],"document":{"requires":["vendor.detached-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"detached","arguments":{"level":{"$literal":7}}}],"select":[{"$field":{"name":"name"}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.detached-1": true}})
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || len(outcome.Annotations) != 1 || string(outcome.Annotations[0].Value) != "7" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestWriteDirectiveElevatesAuthorizationAndEffectAccounting(t *testing.T) {
	t.Parallel()
	descriptor := testDirectiveDescriptor("writes", "vendor.writes-1")
	descriptor.Arguments = nil
	descriptor.Effect = string(runtime.WriteEffect)
	descriptor.Phases = append(descriptor.Phases, schema.DirectiveExecution)
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.RegisterDirective(runtime.DirectiveDefinition{
		Descriptor: descriptor, Wrapper: runtime.DirectiveExecutionWrapperFunc(passthroughDirective),
	}); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "value", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "ok", nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var planningEffect, executionEffect runtime.Effect
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		PlanningAuthorizer: runtime.PlanningAuthorizerFunc(func(request runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
			planningEffect = request.Descriptor.Metadata.Effect
			return runtime.AuthorizationDecision{Allowed: planningEffect == runtime.WriteEffect}, nil
		}),
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			executionEffect = request.Descriptor.Metadata.Effect
			return runtime.AuthorizationDecision{Allowed: executionEffect == runtime.WriteEffect}, nil
		}),
	}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.writes-1"],"document":{"requires":["vendor.writes-1"],"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"value","directives":[{"name":"writes"}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.writes-1": true}})
	plan, err := runtime.Prepare(frozenRegistry(t, registry), request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || planningEffect != runtime.WriteEffect || executionEffect != runtime.WriteEffect || outcome.Effects != runtime.EffectApplied {
		t.Fatalf("outcome=%#v planning=%s execution=%s", outcome, planningEffect, executionEffect)
	}
}

func TestDirectiveAnnotatorCannotMaskCancellation(t *testing.T) {
	t.Parallel()
	var cancel context.CancelFunc
	plan := directiveAnnotatorPlan(t, runtime.DirectiveResponseAnnotatorFunc(func(runtime.DirectiveResponseContext) (json.RawMessage, error) {
		cancel()
		return json.RawMessage(`{"forged":true}`), nil
	}))
	ctx, currentCancel := context.WithCancel(context.Background())
	cancel = currentCancel
	outcome := plan.Execute(ctx)
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeCancelled || len(outcome.Annotations) != 0 {
		t.Fatalf("annotator masked cancellation: %#v", outcome)
	}
}

func TestDirectiveAnnotatorCancellationIsBounded(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	plan := directiveAnnotatorPlan(t, runtime.DirectiveResponseAnnotatorFunc(func(runtime.DirectiveResponseContext) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"late":true}`), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan runtime.Outcome, 1)
	go func() { returned <- plan.ExecuteWith(ctx, runtime.ExecuteOptions{AbandonGrace: -1}) }()
	<-started
	cancel()
	select {
	case outcome := <-returned:
		close(release)
		if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeCancelled || len(outcome.Annotations) != 0 {
			t.Fatalf("unbounded annotator outcome: %#v", outcome)
		}
	case <-time.After(250 * time.Millisecond):
		close(release)
		<-returned
		t.Fatal("cancelled annotator blocked execution")
	}
}

func TestDirectiveAnnotatorFailuresAreClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		annotator runtime.DirectiveResponseAnnotator
	}{
		{
			name: "panic",
			annotator: runtime.DirectiveResponseAnnotatorFunc(func(runtime.DirectiveResponseContext) (json.RawMessage, error) {
				panic("hostile annotator")
			}),
		},
		{
			name: "invalid JSON",
			annotator: runtime.DirectiveResponseAnnotatorFunc(func(runtime.DirectiveResponseContext) (json.RawMessage, error) {
				return json.RawMessage(`{"unterminated":`), nil
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := directiveAnnotatorPlan(t, test.annotator).Execute(context.Background())
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeInternal || len(outcome.Annotations) != 0 {
				t.Fatalf("annotator failure escaped closed handling: %#v", outcome)
			}
		})
	}
}

func directiveAnnotatorPlan(t *testing.T, annotator runtime.DirectiveResponseAnnotator) *runtime.Plan {
	t.Helper()
	descriptor := testDirectiveDescriptor("cancelNote", "vendor.cancel-note-1")
	descriptor.Arguments = nil
	descriptor.Phases = append(descriptor.Phases, schema.DirectiveResponse)
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.RegisterDirective(runtime.DirectiveDefinition{Descriptor: descriptor, Annotator: annotator}); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "ok", nil })); err != nil {
		t.Fatalf("Register: %v", err)
	}
	plan, err := runtime.Prepare(frozenRegistry(t, registry), decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.cancel-note-1"],"document":{"requires":["vendor.cancel-note-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"cancelNote"}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.cancel-note-1": true}}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

func TestDirectiveRegistrationRejectsUnsafeContracts(t *testing.T) {
	t.Parallel()
	base := testDirectiveDescriptor("audit", "vendor.audit-1")
	tests := []struct {
		name   string
		mutate func(*schema.DirectiveDescriptor, *runtime.DirectiveDefinition)
	}{
		{name: "reserved id", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.ID = "naatre.audit"
		}},
		{name: "reserved capability", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Capability = "core.audit-1"
		}},
		{name: "schema identity collision", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.ID = "String"
		}},
		{name: "unknown location", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Locations = []protocol.SelectionKind{"unknown"}
		}},
		{name: "unknown argument type", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Arguments[0].Type = "Missing"
		}},
		{name: "excess declared cost", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Cost = runtime.MaxDirectivePlanCost + 1
		}},
		{name: "duplicate additional capability", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Capabilities = []string{"vendor.extra-1", "vendor.extra-1"}
		}},
		{name: "invalid trait", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Traits = []schema.TraitDescriptor{{ID: "vendor.bad-1", Semantics: schema.TraitDocumentation, Value: json.RawMessage(`{`)}}
		}},
		{name: "invalid deprecation", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Deprecation = &schema.Deprecation{}
		}},
		{name: "invalid source", mutate: func(descriptor *schema.DirectiveDescriptor, _ *runtime.DirectiveDefinition) {
			descriptor.Source = &schema.SourceMetadata{Line: -1}
		}},
		{name: "planner without phase", mutate: func(_ *schema.DirectiveDescriptor, definition *runtime.DirectiveDefinition) {
			definition.Planner = runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
				return runtime.DirectivePlanDecision{}, nil
			})
		}},
		{name: "nondeterministic planner", mutate: func(descriptor *schema.DirectiveDescriptor, definition *runtime.DirectiveDefinition) {
			descriptor.Deterministic = false
			descriptor.Phases = append(descriptor.Phases, schema.DirectivePlanning)
			definition.Planner = runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
				return runtime.DirectivePlanDecision{}, nil
			})
		}},
		{name: "missing validation phase", mutate: func(descriptor *schema.DirectiveDescriptor, definition *runtime.DirectiveDefinition) {
			descriptor.Phases = []schema.DirectivePhase{schema.DirectiveExecution}
			definition.Wrapper = runtime.DirectiveExecutionWrapperFunc(passthroughDirective)
		}},
		{name: "wrapper on structural location", mutate: func(descriptor *schema.DirectiveDescriptor, definition *runtime.DirectiveDefinition) {
			descriptor.Locations = []protocol.SelectionKind{protocol.ParallelSelection}
			descriptor.Phases = append(descriptor.Phases, schema.DirectiveExecution)
			definition.Wrapper = runtime.DirectiveExecutionWrapperFunc(func(_ context.Context, _ runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
				return next()
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := base
			descriptor.Locations = slices.Clone(base.Locations)
			descriptor.Arguments = slices.Clone(base.Arguments)
			descriptor.Phases = slices.Clone(base.Phases)
			definition := runtime.DirectiveDefinition{Descriptor: descriptor}
			test.mutate(&definition.Descriptor, &definition)
			if err := runtime.NewRegistry(coreTypes(t)).RegisterDirective(definition); err == nil {
				t.Fatal("RegisterDirective succeeded")
			}
		})
	}
}

func TestDirectiveRegistrationCanonicalizesArgumentDefaults(t *testing.T) {
	t.Parallel()
	descriptor := testDirectiveDescriptor("defaulted", "vendor.defaulted-1")
	descriptor.Arguments[0].Type = schema.TypeID(schema.Float64)
	descriptor.Arguments[0].Required = false
	descriptor.Arguments[0].Default = json.RawMessage(`1.0`)
	registry := runtime.NewRegistry(coreTypes(t))
	if err := registry.RegisterDirective(runtime.DirectiveDefinition{Descriptor: descriptor}); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	snapshot := frozenRegistry(t, registry)
	descriptors := snapshot.DirectiveDescriptors()
	defaulted := descriptors[len(descriptors)-1]
	if len(defaulted.Arguments) != 1 || string(defaulted.Arguments[0].Default) != "1" {
		t.Fatalf("canonical default = %s", defaulted.Arguments[0].Default)
	}
}

func TestPrepareValidatesDirectiveContractAndNormalCoercion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		location   protocol.SelectionKind
		repeatable bool
		invocation string
		wantCode   string
	}{
		{name: "invalid location", location: protocol.FieldSelection, invocation: `[{"name":"audit","arguments":{"level":{"$literal":1}}}]`, wantCode: "INVALID_DIRECTIVE_LOCATION"},
		{name: "duplicate non-repeatable", location: protocol.CallSelection, invocation: `[{"name":"audit","arguments":{"level":{"$literal":1}}},{"name":"audit","arguments":{"level":{"$literal":2}}}]`, wantCode: "DUPLICATE_DIRECTIVE"},
		{name: "missing argument", location: protocol.CallSelection, invocation: `[{"name":"audit"}]`, wantCode: "MISSING_ARGUMENT"},
		{name: "unknown argument", location: protocol.CallSelection, invocation: `[{"name":"audit","arguments":{"level":{"$literal":1},"extra":{"$literal":true}}}]`, wantCode: "UNKNOWN_ARGUMENT"},
		{name: "wrong argument type", location: protocol.CallSelection, invocation: `[{"name":"audit","arguments":{"level":{"$literal":"high"}}}]`, wantCode: "TYPE_MISMATCH"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := testDirectiveDescriptor("audit", "vendor.audit-1")
			descriptor.Locations = []protocol.SelectionKind{test.location}
			descriptor.Repeatable = test.repeatable
			snapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{Descriptor: descriptor})
			request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.audit-1"],"document":{"requires":["vendor.audit-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":`+test.invocation+`}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.audit-1": true}})
			if codes := validationCodes(t, prepareError(snapshot, request)); !slices.Contains(codes, test.wantCode) {
				t.Fatalf("codes = %v, want %s", codes, test.wantCode)
			}
		})
	}

	descriptor := testDirectiveDescriptor("audit", "vendor.audit-1")
	snapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{Descriptor: descriptor})
	missingPin := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.audit-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"audit","arguments":{"level":{"$literal":1}}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.audit-1": true}})
	if codes := validationCodes(t, prepareError(snapshot, missingPin)); !slices.Contains(codes, "MISSING_DIRECTIVE_CAPABILITY") {
		t.Fatalf("missing pin codes = %v", codes)
	}
}

func TestDirectivePlannerCannotHideInvalidStructureEffectsOrExcessCost(t *testing.T) {
	t.Parallel()
	descriptor := testDirectiveDescriptor("gate", "vendor.gate-1")
	descriptor.Phases = append(descriptor.Phases, schema.DirectivePlanning)
	planner := runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
		return runtime.DirectivePlanDecision{Skip: true}, nil
	})
	snapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{Descriptor: descriptor, Planner: planner})
	options := protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.gate-1": true}}
	tests := []struct {
		name     string
		selects  string
		wantCode string
	}{
		{name: "unknown field", selects: `[{"$call":{"name":"text","directives":[{"name":"gate","arguments":{"level":{"$literal":1}}}],"select":[{"$field":{"name":"missing"}}]}}]`, wantCode: "UNKNOWN_FIELD"},
		{name: "alias collision", selects: `[{"$call":{"name":"text","as":"same","directives":[{"name":"gate","arguments":{"level":{"$literal":1}}}]}},{"$call":{"name":"text","as":"same"}}]`, wantCode: "DUPLICATE_RESPONSE_NAME"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.gate-1"],"document":{"requires":["vendor.gate-1"],"operations":[{"name":"Q","kind":"query","select":`+test.selects+`}]}}`, options)
			if codes := validationCodes(t, prepareError(snapshot, request)); !slices.Contains(codes, test.wantCode) {
				t.Fatalf("codes = %v, want %s", codes, test.wantCode)
			}
		})
	}

	writeDescriptor := descriptor
	writeDescriptor.ID, writeDescriptor.Name, writeDescriptor.Capability = "vendor.writeGate", "writeGate", "vendor.write-gate-1"
	writeDescriptor.Effect = string(runtime.WriteEffect)
	writeDescriptor.Phases = append(writeDescriptor.Phases, schema.DirectiveExecution)
	writeSnapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{
		Descriptor: writeDescriptor, Planner: planner, Wrapper: runtime.DirectiveExecutionWrapperFunc(passthroughDirective),
	})
	writeRequest := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.write-gate-1"],"document":{"requires":["vendor.write-gate-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"writeGate","arguments":{"level":{"$literal":1}}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.write-gate-1": true}})
	if codes := validationCodes(t, prepareError(writeSnapshot, writeRequest)); !slices.Contains(codes, "EFFECT_NOT_ALLOWED") {
		t.Fatalf("write directive codes = %v", codes)
	}

	costDescriptor := testDirectiveDescriptor("costly", "vendor.costly-1")
	costDescriptor.Phases = append(costDescriptor.Phases, schema.DirectivePlanning)
	costPlanner := runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
		return runtime.DirectivePlanDecision{AdditionalCost: runtime.MaxDirectivePlanCost}, nil
	})
	costSnapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{Descriptor: costDescriptor, Planner: costPlanner})
	costRequest := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.costly-1"],"document":{"requires":["vendor.costly-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"costly","arguments":{"level":{"$literal":1}}}],"select":[{"$field":{"name":"name"}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.costly-1": true}})
	if codes := validationCodes(t, prepareError(costSnapshot, costRequest)); !slices.Contains(codes, "DIRECTIVE_COST_EXCEEDED") {
		t.Fatalf("cost directive codes = %v", codes)
	}

	var plannerCalls atomic.Int64
	lateDescriptor := testDirectiveDescriptor("late", "vendor.late-1")
	lateDescriptor.Phases = append(lateDescriptor.Phases, schema.DirectivePlanning)
	lateSnapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{
		Descriptor: lateDescriptor,
		Planner: runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
			plannerCalls.Add(1)
			return runtime.DirectivePlanDecision{}, nil
		}),
	})
	lateRequest := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.late-1"],"document":{"requires":["vendor.late-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"late","arguments":{"level":{"$literal":1}}}],"select":[{"$field":{"name":"missing"}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.late-1": true}})
	if codes := validationCodes(t, prepareError(lateSnapshot, lateRequest)); !slices.Contains(codes, "UNKNOWN_FIELD") {
		t.Fatalf("late validation codes = %v", codes)
	}
	if plannerCalls.Load() != 0 {
		t.Fatalf("planner ran %d times before validation completed", plannerCalls.Load())
	}
}

func TestDirectivePlannerPanicFailsPreparationClosed(t *testing.T) {
	t.Parallel()
	descriptor := testDirectiveDescriptor("panicPlan", "vendor.panic-plan-1")
	descriptor.Phases = append(descriptor.Phases, schema.DirectivePlanning)
	snapshot := directiveTestSnapshot(t, runtime.DirectiveDefinition{
		Descriptor: descriptor,
		Planner: runtime.DirectivePlannerFunc(func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
			panic("hostile planner")
		}),
	})
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.panic-plan-1"],"document":{"requires":["vendor.panic-plan-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"panicPlan","arguments":{"level":{"$literal":1}}}],"select":[{"$field":{"name":"name"}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.panic-plan-1": true}})
	if codes := validationCodes(t, prepareError(snapshot, request)); !slices.Contains(codes, "DIRECTIVE_PLANNING") {
		t.Fatalf("planner panic codes = %v", codes)
	}
}

type directiveCounters struct {
	wrappers atomic.Int64
	handlers atomic.Int64
}

func hostileDirectiveSnapshot(t *testing.T, wrapper runtime.DirectiveExecutionWrapper, deny bool) (runtime.Snapshot, *directiveCounters) {
	t.Helper()
	counts := &directiveCounters{}
	descriptor := testDirectiveDescriptor("hostile", "vendor.hostile-1")
	descriptor.Phases = append(descriptor.Phases, schema.DirectiveExecution)
	descriptor.Arguments = nil
	registry := runtime.NewRegistry(coreTypes(t))
	wrapped := runtime.DirectiveExecutionWrapperFunc(func(ctx context.Context, input runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
		counts.wrappers.Add(1)
		return wrapper.WrapDirective(ctx, input, next)
	})
	if err := registry.RegisterDirective(runtime.DirectiveDefinition{Descriptor: descriptor, Wrapper: wrapped}); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		counts.handlers.Add(1)
		return "ok", nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if deny {
		if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault}); err != nil {
			t.Fatalf("ConfigureAuthorization: %v", err)
		}
	}
	return frozenRegistry(t, registry), counts
}

func hostileDirectiveRequest(t *testing.T) *protocol.Request {
	t.Helper()
	return decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["vendor.hostile-1"],"document":{"requires":["vendor.hostile-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"hostile"}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"vendor.hostile-1": true}})
}

func passthroughDirective(_ context.Context, _ runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
	return next()
}

func TestDirectiveWrapperRunsAfterAuthorization(t *testing.T) {
	t.Parallel()
	snapshot, counts := hostileDirectiveSnapshot(t, runtime.DirectiveExecutionWrapperFunc(passthroughDirective), true)
	plan, err := runtime.Prepare(snapshot, hostileDirectiveRequest(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if counts.wrappers.Load() != 0 || counts.handlers.Load() != 0 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeUnauthorized {
		t.Fatalf("outcome=%#v wrappers=%d handlers=%d", outcome, counts.wrappers.Load(), counts.handlers.Load())
	}
}

func TestDirectiveWrapperDoesNotRunAfterCancellation(t *testing.T) {
	t.Parallel()
	snapshot, counts := hostileDirectiveSnapshot(t, runtime.DirectiveExecutionWrapperFunc(passthroughDirective), false)
	plan, err := runtime.Prepare(snapshot, hostileDirectiveRequest(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := plan.Execute(ctx)
	if counts.wrappers.Load() != 0 || counts.handlers.Load() != 0 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeCancelled {
		t.Fatalf("outcome=%#v wrappers=%d handlers=%d", outcome, counts.wrappers.Load(), counts.handlers.Load())
	}
}

func TestDirectiveWrapperContinuationIsOneShot(t *testing.T) {
	t.Parallel()
	doubleNext := runtime.DirectiveExecutionWrapperFunc(func(_ context.Context, _ runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
		value, err := next()
		if err != nil {
			return nil, err
		}
		_, _ = next()
		return value, nil
	})
	snapshot, counts := hostileDirectiveSnapshot(t, doubleNext, false)
	plan, err := runtime.Prepare(snapshot, hostileDirectiveRequest(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if counts.wrappers.Load() != 1 || counts.handlers.Load() != 1 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeInternal ||
		!errors.Is(outcome.Errors[0], runtime.ErrDirectiveNextCalled) {
		t.Fatalf("outcome=%#v wrappers=%d handlers=%d", outcome, counts.wrappers.Load(), counts.handlers.Load())
	}
}

func TestDirectiveWrapperCannotRetainContinuation(t *testing.T) {
	t.Parallel()
	var retained runtime.DirectiveNext
	retainNext := runtime.DirectiveExecutionWrapperFunc(func(_ context.Context, _ runtime.DirectiveExecutionContext, next runtime.DirectiveNext) (any, error) {
		retained = next
		return "forged", nil
	})
	snapshot, counts := hostileDirectiveSnapshot(t, retainNext, false)
	plan, err := runtime.Prepare(snapshot, hostileDirectiveRequest(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	_ = plan.Execute(context.Background())
	if retained == nil {
		t.Fatal("wrapper did not retain continuation")
	}
	if _, err := retained(); !errors.Is(err, runtime.ErrDirectiveNextCalled) {
		t.Fatalf("retained continuation error = %v", err)
	}
	if counts.handlers.Load() != 0 {
		t.Fatalf("retained continuation ran handler %d times", counts.handlers.Load())
	}
}

func TestDirectiveWrapperCannotMaskCancellation(t *testing.T) {
	t.Parallel()
	var cancel context.CancelFunc
	forged := runtime.DirectiveExecutionWrapperFunc(func(_ context.Context, _ runtime.DirectiveExecutionContext, _ runtime.DirectiveNext) (any, error) {
		cancel()
		return "forged", nil
	})
	snapshot, counts := hostileDirectiveSnapshot(t, forged, false)
	plan, err := runtime.Prepare(snapshot, hostileDirectiveRequest(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for range 64 {
		ctx, currentCancel := context.WithCancel(context.Background())
		cancel = currentCancel
		outcome := plan.Execute(ctx)
		if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeCancelled {
			t.Fatalf("wrapper masked cancellation: %#v", outcome)
		}
	}
	if counts.handlers.Load() != 0 {
		t.Fatalf("handler ran %d times", counts.handlers.Load())
	}
}

func testDirectiveDescriptor(name, capability string) schema.DirectiveDescriptor {
	return schema.DirectiveDescriptor{
		ID: "vendor." + name, Name: name, Version: "1", Capability: capability,
		Locations: []protocol.SelectionKind{protocol.CallSelection},
		Arguments: []schema.DirectiveArgumentDescriptor{{
			ID: "vendor." + name + ".level", Name: "level", Type: schema.TypeID(schema.Int32), Required: true,
		}},
		Phases: []schema.DirectivePhase{schema.DirectiveValidation}, Effect: string(runtime.ReadEffect),
		Cost: 1, Deterministic: true, Compatibility: schema.ChangeDangerous,
	}
}

func directiveTestSnapshot(t *testing.T, directive runtime.DirectiveDefinition) runtime.Snapshot {
	t.Helper()
	registry := runtime.NewRegistry(compositionTypes(t))
	if err := registry.RegisterDirective(directive); err != nil {
		t.Fatalf("RegisterDirective: %v", err)
	}
	if err := registry.Register(runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return map[string]any{"name": "ok"}, nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(runtime.BindField[map[string]any, string](runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (string, error) { return source["name"].(string), nil })); err != nil {
		t.Fatalf("Register field: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snapshot
}
