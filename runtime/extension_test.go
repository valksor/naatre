package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestExtensionRegistryFreezesOrderingDiscoveryAndNegotiation(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	events := make([]string, 0, 2)
	registerExtensionDirective(t, registry, "alphaExt", "com.example.alpha-1", func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
		events = append(events, "alpha")
		return runtime.DirectivePlanDecision{}, nil
	})
	registerExtensionDirective(t, registry, "betaExt", "org.example.beta-1", func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
		events = append(events, "beta")
		return runtime.DirectivePlanDecision{}, nil
	})
	registerExtensionRoot(t, registry)

	alpha := runtimeExtension("com.example.alpha", "1.0.0", "com.example.alpha-1", "com.example.alpha.impl-1", "alphaExt")
	beta := runtimeExtension("org.example.beta", "2.1.0", "org.example.beta-1", "org.example.beta.impl-2", "betaExt")
	beta.Before = []string{alpha.ID}
	if err := registry.RegisterExtension(alpha); err != nil {
		t.Fatalf("RegisterExtension(alpha): %v", err)
	}
	if err := registry.RegisterExtension(beta); err != nil {
		t.Fatalf("RegisterExtension(beta): %v", err)
	}
	alpha.Directives[0] = "mutated"
	beta.Before[0] = "org.example.changed"

	snapshot := frozenRegistry(t, registry)
	descriptors := snapshot.ExtensionDescriptors()
	if len(descriptors) != 2 || descriptors[0].ID != "org.example.beta" || descriptors[1].ID != "com.example.alpha" {
		t.Fatalf("extension order = %#v", descriptors)
	}
	descriptors[0].Before[0] = "org.example.mutated"
	if snapshot.ExtensionDescriptors()[0].Before[0] != "com.example.alpha" {
		t.Fatal("frozen extension descriptor mutated through accessor")
	}

	options, err := snapshot.DecodeOptions(protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeOptions: %v", err)
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["org.example.beta-1","com.example.alpha-1"],"extensions":{"com.example.alpha":{"secret":"not retained by plan"}},"document":{"requires":["com.example.alpha-1","org.example.beta-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"alphaExt"},{"name":"betaExt"}]}}]}]}}`), options)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !slices.Equal(events, []string{"alpha", "beta"}) {
		t.Fatalf("planner order = %v", events)
	}
	description := plan.Description()
	if len(description.Extensions) != 2 || description.Extensions[0].ID != "com.example.alpha" || description.Extensions[1].ID != "org.example.beta" {
		t.Fatalf("plan extensions = %#v", description.Extensions)
	}
	described, _ := json.Marshal(description)
	if strings.Contains(string(described), "not retained") || strings.Contains(string(described), "secret") {
		t.Fatalf("plan retained extension payload: %s", described)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || !slices.Equal(outcome.Capabilities, []string{"com.example.alpha-1", "org.example.beta-1"}) || len(outcome.Extensions) != 2 {
		t.Fatalf("outcome negotiation = %#v", outcome)
	}

	document, err := snapshot.ExportSchema(schema.ExportOptions{Revision: "extensions-r1"})
	if err != nil {
		t.Fatalf("ExportSchema: %v", err)
	}
	exported := document.Extensions()
	if len(exported) != 2 || exported[0].ID != "com.example.alpha" || exported[0].Version != "1.0.0" || exported[0].Capability != "com.example.alpha-1" {
		t.Fatalf("schema extensions = %#v", exported)
	}
}

func TestExtensionRegistryRejectsConflictsCyclesAndUnsafeOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*testing.T, *runtime.Registry)
		want  string
	}{
		{
			name: "conflict",
			setup: func(t *testing.T, registry *runtime.Registry) {
				left := runtimeExtension("com.example.left", "1.0.0", "com.example.left-1", "com.example.left.impl-1")
				right := runtimeExtension("org.example.right", "1.0.0", "org.example.right-1", "org.example.right.impl-1")
				left.Conflicts = []string{right.ID}
				mustRegisterExtension(t, registry, left)
				mustRegisterExtension(t, registry, right)
			},
			want: "conflict",
		},
		{
			name: "ordering cycle",
			setup: func(t *testing.T, registry *runtime.Registry) {
				left := runtimeExtension("com.example.left", "1.0.0", "com.example.left-1", "com.example.left.impl-1")
				right := runtimeExtension("org.example.right", "1.0.0", "org.example.right-1", "org.example.right.impl-1")
				left.Before = []string{right.ID}
				right.Before = []string{left.ID}
				mustRegisterExtension(t, registry, left)
				mustRegisterExtension(t, registry, right)
			},
			want: "cycle",
		},
		{
			name: "missing directive",
			setup: func(t *testing.T, registry *runtime.Registry) {
				mustRegisterExtension(t, registry, runtimeExtension("com.example.missing", "1.0.0", "com.example.missing-1", "com.example.missing.impl-1", "absent"))
			},
			want: "unregistered custom directive",
		},
		{
			name: "capability mismatch",
			setup: func(t *testing.T, registry *runtime.Registry) {
				registerExtensionDirective(t, registry, "auditExt", "com.example.other-1", nil)
				mustRegisterExtension(t, registry, runtimeExtension("com.example.audit", "1.0.0", "com.example.audit-1", "com.example.audit.impl-1", "auditExt"))
			},
			want: "want \"com.example.audit-1\"",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			registry := runtime.NewRegistry(coreTypes(t))
			test.setup(t, registry)
			if _, err := registry.Freeze(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Freeze() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestExtensionPlannerAdditionalCostIsBoundedAcrossInvocations(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	registerExtensionDirective(t, registry, "costExt", "com.example.cost-1", func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
		return runtime.DirectivePlanDecision{AdditionalCost: 2}, nil
	})
	registerExtensionRoot(t, registry)
	descriptor := runtimeExtension("com.example.cost", "1.0.0", "com.example.cost-1", "com.example.cost.impl-1", "costExt")
	descriptor.CostBehavior = schema.ExtensionCostBounded
	descriptor.MaxAdditionalCost = 3
	mustRegisterExtension(t, registry, descriptor)
	snapshot := frozenRegistry(t, registry)
	options, err := snapshot.DecodeOptions(protocol.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["com.example.cost-1"],"document":{"requires":["com.example.cost-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","as":"one","directives":[{"name":"costExt"}]}},{"$call":{"name":"text","as":"two","directives":[{"name":"costExt"}]}}]}]}}`), options)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Prepare(snapshot, request)
	var validation *runtime.ValidationErrors
	if !errors.As(err, &validation) || len(validation.Issues()) == 0 || validation.Issues()[0].Diagnostic.Code != "EXTENSION_COST_EXCEEDED" {
		t.Fatalf("Prepare error = %#v", err)
	}
}

func TestSnapshotDecodeOptionsRejectsIncompatibleCallerPolicy(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	mustRegisterExtension(t, registry, runtimeExtension("com.example.audit", "1.0.0", "com.example.audit-1", "com.example.audit.impl-1"))
	snapshot := frozenRegistry(t, registry)
	_, err := snapshot.DecodeOptions(protocol.DecodeOptions{Extensions: map[string]protocol.ExtensionSupport{
		"com.example.audit": {Version: "2.0.0", Capability: "com.example.audit-2"},
	}})
	if err == nil || !strings.Contains(err.Error(), "incompatible version policy") {
		t.Fatalf("DecodeOptions = %v", err)
	}
}

func TestPrepareRejectsNegotiatedExtensionOutsideFrozenSnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		register *schema.ExtensionDescriptor
	}{
		{name: "absent extension"},
		{name: "same ID version skew", register: func() *schema.ExtensionDescriptor {
			descriptor := runtimeExtension("com.example.audit", "2.0.0", "com.example.audit-1", "com.example.audit.impl-2")
			return &descriptor
		}()},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			registry := runtime.NewRegistry(coreTypes(t))
			registerExtensionRoot(t, registry)
			if test.register != nil {
				mustRegisterExtension(t, registry, *test.register)
			}
			snapshot := frozenRegistry(t, registry)
			request, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["com.example.audit-1"],"extensions":{"com.example.audit":{"level":2}},"document":{"requires":["com.example.audit-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}}`), protocol.DecodeOptions{
				Capabilities: map[string]bool{"com.example.audit-1": true},
				Extensions: map[string]protocol.ExtensionSupport{
					"com.example.audit": {Version: "1.0.0", Capability: "com.example.audit-1"},
				},
			})
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			_, err = runtime.Prepare(snapshot, request)
			var validation *runtime.ValidationErrors
			if !errors.As(err, &validation) || len(validation.Issues()) == 0 || validation.Issues()[0].Diagnostic.Code != "UNSUPPORTED_EXTENSION" {
				t.Fatalf("Prepare error = %#v", err)
			}
		})
	}
}

func TestPrepareRequiresExactTupleForRequestedFrozenExtensionCapability(t *testing.T) {
	t.Parallel()
	registry := runtime.NewRegistry(coreTypes(t))
	plannerCalled := false
	registerExtensionDirective(t, registry, "auditExt", "com.example.audit-1", func(runtime.DirectivePlanContext) (runtime.DirectivePlanDecision, error) {
		plannerCalled = true
		return runtime.DirectivePlanDecision{}, nil
	})
	registerExtensionRoot(t, registry)
	mustRegisterExtension(t, registry, runtimeExtension("com.example.audit", "1.0.0", "com.example.audit-1", "com.example.audit.impl-1", "auditExt"))
	snapshot := frozenRegistry(t, registry)

	request, err := protocol.DecodeRequest([]byte(`{"version":"1","capabilities":["com.example.audit-1"],"document":{"requires":["com.example.audit-1"],"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text","directives":[{"name":"auditExt"}]}}]}]}}`), protocol.DecodeOptions{
		Capabilities: map[string]bool{"com.example.audit-1": true},
	})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	_, err = runtime.Prepare(snapshot, request)
	var validation *runtime.ValidationErrors
	if !errors.As(err, &validation) || len(validation.Issues()) == 0 || validation.Issues()[0].Diagnostic.Code != "UNSUPPORTED_EXTENSION" {
		t.Fatalf("Prepare error = %#v", err)
	}
	if plannerCalled {
		t.Fatal("extension planner ran before exact tuple validation")
	}
}

func runtimeExtension(id, version, capability, implementation string, directives ...string) schema.ExtensionDescriptor {
	return schema.ExtensionDescriptor{
		ExtensionReference: schema.ExtensionReference{ID: id, Version: version, Capability: capability},
		Implementation:     implementation, Points: []schema.ExtensionPoint{schema.ExtensionValidation, schema.ExtensionPlanning},
		Directives: directives, Deterministic: true, SideEffects: "none", CostBehavior: schema.ExtensionCostNone,
		Compatibility: schema.ChangeAdditive, Security: "closed directive view only",
	}
}

func mustRegisterExtension(t *testing.T, registry *runtime.Registry, descriptor schema.ExtensionDescriptor) {
	t.Helper()
	if err := registry.RegisterExtension(descriptor); err != nil {
		t.Fatalf("RegisterExtension: %v", err)
	}
}

func registerExtensionDirective(t *testing.T, registry *runtime.Registry, name, capability string, planner runtime.DirectivePlannerFunc) {
	t.Helper()
	phases := []schema.DirectivePhase{schema.DirectiveValidation}
	if planner != nil {
		phases = append(phases, schema.DirectivePlanning)
	}
	if err := registry.RegisterDirective(runtime.DirectiveDefinition{
		Descriptor: schema.DirectiveDescriptor{
			ID: "vendor." + name, Name: name, Version: "1", Capability: capability,
			Locations: []protocol.SelectionKind{protocol.CallSelection}, Phases: phases,
			Effect: string(runtime.ReadEffect), Deterministic: true, Compatibility: schema.ChangeDangerous,
		},
		Planner: planner,
	}); err != nil {
		t.Fatalf("RegisterDirective(%s): %v", name, err)
	}
}

func registerExtensionRoot(t *testing.T, registry *runtime.Registry) {
	t.Helper()
	if err := registry.Register(runtime.BindInvocation[string](runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) { return "ok", nil })); err != nil {
		t.Fatalf("Register root: %v", err)
	}
}
