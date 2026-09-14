package runtime_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestFederationPlannerBuildsDeterministicDependencyPlan(t *testing.T) {
	t.Parallel()
	composition := federationEntityComposition(t, false)
	planner, err := runtime.NewFederationPlanner(runtime.FederationPlannerConfig{
		Composition: composition,
		Limits:      runtime.FederationLimits{MaxCalls: 2, MaxCost: 10, MaxConcurrency: 2, MaxAttempts: 2},
	})
	planner = mustFederationValue(t, planner, err)
	input := map[string]any{"id": "user-1"}
	fetches := []runtime.FederationEntityFetch{
		{ResponseKey: "user", ServiceID: "users", Type: "User", Input: input, Path: []any{"viewer"}, MaxAttempts: 2, Dependencies: []string{"order"}},
		{ResponseKey: "order", ServiceID: "orders", Type: "Order", Input: map[string]any{"id": "order-1"}, Path: []any{"viewer", "order"}, MaxAttempts: 1},
	}
	plan, err := planner.PlanEntityFetches(composition.Schema().Revision(), fetches)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaRevision != composition.Schema().Revision() || len(plan.Calls) != 2 ||
		plan.Calls[0].ResponseKey != "order" || plan.Calls[1].ResponseKey != "user" ||
		!reflect.DeepEqual(plan.Calls[1].DependsOn, []string{"order"}) || plan.Calls[1].OperationID != "query.user" {
		t.Fatalf("entity plan = %#v", plan)
	}
	input["id"] = "mutated"
	fetches[0].Dependencies[0] = "mutated"
	if plan.Calls[1].Input.(map[string]any)["id"] != "user-1" || plan.Calls[1].DependsOn[0] != "order" {
		t.Fatalf("entity plan retained caller-owned data: %#v", plan.Calls[1])
	}

	reversed := []runtime.FederationEntityFetch{fetches[1], {
		ResponseKey: "user", ServiceID: "users", Type: "User", Input: map[string]any{"id": "user-1"},
		Path: []any{"viewer"}, MaxAttempts: 2, Dependencies: []string{"order"},
	}}
	again, err := planner.PlanEntityFetches(composition.Schema().Revision(), reversed)
	if err != nil || !reflect.DeepEqual(plan, again) {
		t.Fatalf("reordered entity plan = %#v, %v; want %#v", again, err, plan)
	}
}

func TestFederationPlannerRejectsInvalidAndUnboundedEntityPlans(t *testing.T) {
	t.Parallel()
	composition := federationEntityComposition(t, false)
	validOrder := runtime.FederationEntityFetch{ResponseKey: "order", ServiceID: "orders", Type: "Order", Path: []any{"order"}, MaxAttempts: 1}
	validUser := runtime.FederationEntityFetch{ResponseKey: "user", ServiceID: "users", Type: "User", Path: []any{"user"}, MaxAttempts: 1, Dependencies: []string{"order"}}
	tests := []struct {
		name     string
		limits   runtime.FederationLimits
		revision string
		fetches  []runtime.FederationEntityFetch
		code     string
	}{
		{name: "schema revision", limits: federationPlannerLimits(), revision: "federation-r2", fetches: []runtime.FederationEntityFetch{validOrder}, code: runtime.CodeFederationSchemaMismatch},
		{name: "missing dependency", limits: federationPlannerLimits(), revision: "federation-r1", fetches: []runtime.FederationEntityFetch{validUser}, code: runtime.CodeFederationPlanInvalid},
		{name: "wrong dependency", limits: federationPlannerLimits(), revision: "federation-r1", fetches: []runtime.FederationEntityFetch{validOrder, {ResponseKey: "user", ServiceID: "users", Type: "User", Path: []any{"user"}, MaxAttempts: 1, Dependencies: []string{"user"}}}, code: runtime.CodeFederationPlanInvalid},
		{name: "unknown route", limits: federationPlannerLimits(), revision: "federation-r1", fetches: []runtime.FederationEntityFetch{{ResponseKey: "missing", ServiceID: "missing", Type: "Missing", Input: "credential-secret", Path: []any{"missing"}, MaxAttempts: 1}}, code: runtime.CodeFederationPlanInvalid},
		{name: "duplicate response", limits: federationPlannerLimits(), revision: "federation-r1", fetches: []runtime.FederationEntityFetch{validOrder, validOrder}, code: runtime.CodeFederationPlanInvalid},
		{name: "fan out", limits: runtime.FederationLimits{MaxCalls: 1, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 1}, revision: "federation-r1", fetches: []runtime.FederationEntityFetch{validOrder, validUser}, code: runtime.CodeResourceExhausted},
		{name: "retry multiplication", limits: runtime.FederationLimits{MaxCalls: 2, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 2}, revision: "federation-r1", fetches: []runtime.FederationEntityFetch{validOrder, {ResponseKey: "user", ServiceID: "users", Type: "User", Path: []any{"user"}, MaxAttempts: 2, Dependencies: []string{"order"}}}, code: runtime.CodeResourceExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			planner, err := runtime.NewFederationPlanner(runtime.FederationPlannerConfig{Composition: composition, Limits: test.limits})
			planner = mustFederationValue(t, planner, err)
			_, err = planner.PlanEntityFetches(test.revision, test.fetches)
			var failure *runtime.ExecutionError
			if !errors.As(err, &failure) || failure.Code != test.code {
				t.Fatalf("planning error = %#v (%v), want %s", failure, err, test.code)
			}
			if strings.Contains(failure.Error(), "credential-secret") {
				t.Fatalf("planning error exposed protected input or source: %#v", failure)
			}
		})
	}
}

func TestFederationCoordinatorExecutesDependenciesWithDelegationAndTrace(t *testing.T) {
	t.Parallel()
	composition := federationEntityComposition(t, false)
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	issuer := federationDelegationIssuerAt(t, now)
	traceParent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	var mu sync.Mutex
	invoked := make([]string, 0, 2)
	bindings := make([]runtime.FederationEndpointBinding, 0, 2)
	for _, serviceID := range []string{"orders", "users"} {
		service, _ := composition.Service(serviceID)
		bindings = append(bindings, runtime.FederationEndpointBinding{
			Reference: service.EndpointReference,
			Invoker: runtime.FederationInvokerFunc(func(_ context.Context, invocation runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
				if invocation.TraceContext.TraceParent != traceParent {
					t.Fatalf("trace context = %#v", invocation.TraceContext)
				}
				verifier := federationDelegationVerifierAt(t, service.Audience, now)
				delegation, err := verifier.Verify(invocation.Delegation, runtime.FederationDelegationExpectation{
					RequestID: invocation.RequestID, OperationID: invocation.OperationID, SchemaRevision: composition.Schema().Revision(),
					ServiceSchemaRevision: service.SchemaRevision, ServiceSchemaDigest: service.SchemaDigest,
				})
				if err != nil || delegation.Subject != "user-1" || delegation.AuthorizationRevision != "policy-r1" {
					t.Fatalf("delegation = %#v, %v", delegation, err)
				}
				mu.Lock()
				invoked = append(invoked, service.ID)
				mu.Unlock()
				return runtime.FederationRemoteResult{Data: map[string]any{"service": service.ID}, SchemaRevision: service.SchemaRevision}, nil
			}),
		})
	}
	coordinator, err := runtime.NewFederationCoordinator(runtime.FederationCoordinatorConfig{
		Composition: composition, Delegations: issuer, Endpoints: bindings,
		TraceContext: runtime.FederationTraceContextProviderFunc(func(context.Context) (runtime.FederationTraceContext, error) {
			return runtime.FederationTraceContext{TraceParent: traceParent}, nil
		}),
		Limits: federationPlannerLimits(), MaximumDuration: time.Second,
	})
	coordinator = mustFederationValue(t, coordinator, err)
	ctx, cancel := context.WithDeadline(runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", AuthorizationRevision: "policy-r1",
	}), time.Now().Add(time.Minute))
	defer cancel()
	outcome := coordinator.ExecuteEntityFetches(ctx, "request-1", composition.Schema().Revision(), []runtime.FederationEntityFetch{
		{ResponseKey: "user", ServiceID: "users", Type: "User", Path: []any{"user"}, MaxAttempts: 1, Dependencies: []string{"order"}},
		{ResponseKey: "order", ServiceID: "orders", Type: "Order", Path: []any{"order"}, MaxAttempts: 1},
	})
	if len(outcome.Errors) != 0 || len(outcome.Data) != 2 {
		t.Fatalf("coordinator outcome = %#v", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(invoked, []string{"orders", "users"}) {
		t.Fatalf("invocation order = %v", invoked)
	}
}

func TestFederationCoordinatorPreservesIndependentDataAndBlocksFailedDependencies(t *testing.T) {
	t.Parallel()
	composition := federationEntityComposition(t, true)
	issuer := federationIssuer(t)
	var userInvocations atomic.Int64
	bindings := federationBindings(t, composition, map[string]runtime.FederationInvoker{
		"orders": runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
			return runtime.FederationRemoteResult{}, errors.New("transport credential-secret")
		}),
		"users": runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
			userInvocations.Add(1)
			return runtime.FederationRemoteResult{}, nil
		}),
		"inventory": runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
			return runtime.FederationRemoteResult{Data: "available", SchemaRevision: "inventory-r1"}, nil
		}),
	})
	coordinator, err := runtime.NewFederationCoordinator(runtime.FederationCoordinatorConfig{
		Composition: composition, Delegations: issuer, Endpoints: bindings,
		Limits: runtime.FederationLimits{MaxCalls: 3, MaxCost: 10, MaxConcurrency: 3, MaxAttempts: 1}, MaximumDuration: time.Second,
	})
	coordinator = mustFederationValue(t, coordinator, err)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	outcome := coordinator.ExecuteEntityFetches(ctx, "request-1", composition.Schema().Revision(), []runtime.FederationEntityFetch{
		{ResponseKey: "user", ServiceID: "users", Type: "User", Path: []any{"user"}, MaxAttempts: 1, Dependencies: []string{"order"}},
		{ResponseKey: "order", ServiceID: "orders", Type: "Order", Path: []any{"order"}, MaxAttempts: 1},
		{ResponseKey: "inventory", ServiceID: "inventory", Type: "Inventory", Path: []any{"inventory"}, MaxAttempts: 1},
	})
	if outcome.Data["inventory"] != "available" || userInvocations.Load() != 0 || len(outcome.Errors) != 2 {
		t.Fatalf("partial outcome = %#v, user invocations = %d", outcome, userInvocations.Load())
	}
	for _, failure := range outcome.Errors {
		if failure.Code != runtime.CodeFederationUnavailable || strings.Contains(failure.Message, "credential-secret") {
			t.Fatalf("unsafe public failure = %#v", failure)
		}
	}
}

func TestFederationCoordinatorRejectsUnsafeTraceAndEndpointBindings(t *testing.T) {
	t.Parallel()
	composition := federationEntityComposition(t, false)
	issuer := federationIssuer(t)
	bindings := federationBindings(t, composition, map[string]runtime.FederationInvoker{
		"orders": runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
			t.Fatal("invoker ran with an invalid trace context")
			return runtime.FederationRemoteResult{}, nil
		}),
		"users": runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
			return runtime.FederationRemoteResult{}, nil
		}),
	})
	if _, err := runtime.NewFederationCoordinator(runtime.FederationCoordinatorConfig{
		Composition: composition, Delegations: issuer, Endpoints: bindings[:1], Limits: federationPlannerLimits(), MaximumDuration: time.Second,
	}); err == nil {
		t.Fatal("coordinator accepted incomplete endpoint bindings")
	}
	coordinator, err := runtime.NewFederationCoordinator(runtime.FederationCoordinatorConfig{
		Composition: composition, Delegations: issuer, Endpoints: bindings,
		TraceContext: runtime.FederationTraceContextProviderFunc(func(context.Context) (runtime.FederationTraceContext, error) {
			return runtime.FederationTraceContext{TraceParent: "credential-secret\r\nforged"}, nil
		}),
		Limits: federationPlannerLimits(), MaximumDuration: time.Second,
	})
	coordinator = mustFederationValue(t, coordinator, err)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	outcome := coordinator.ExecuteEntityFetches(ctx, "request-1", composition.Schema().Revision(), []runtime.FederationEntityFetch{
		{ResponseKey: "order", ServiceID: "orders", Type: "Order", Path: []any{"order"}, MaxAttempts: 1},
	})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationUnavailable ||
		strings.Contains(outcome.Errors[0].Message, "credential-secret") {
		t.Fatalf("unsafe trace outcome = %#v", outcome)
	}
}

func TestReferenceFederationCoordinatorRejectsDependencyCyclesBeforeInvocation(t *testing.T) {
	t.Parallel()
	composition := federationEntityComposition(t, false)
	var invocations atomic.Int64
	coordinator := newFederationCoordinator(t, composition, federationIssuer(t), runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		invocations.Add(1)
		return runtime.FederationRemoteResult{}, nil
	}), runtime.FederationLimits{MaxCalls: 2, MaxCost: 10, MaxConcurrency: 2, MaxAttempts: 1})
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{SchemaRevision: composition.Schema().Revision(), Calls: []runtime.FederationCall{
		{ResponseKey: "left", ServiceID: "orders", OperationID: "query.order", Path: []any{"left"}, MaxAttempts: 1, DependsOn: []string{"right"}},
		{ResponseKey: "right", ServiceID: "users", OperationID: "query.user", Path: []any{"right"}, MaxAttempts: 1, DependsOn: []string{"left"}},
	}})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationPlanInvalid || invocations.Load() != 0 {
		t.Fatalf("cyclic plan outcome = %#v, invocations = %d", outcome, invocations.Load())
	}
}

func federationEntityComposition(t *testing.T, includeInventory bool) schema.FederationComposition {
	t.Helper()
	orders := runtimeFederationManifest(t, "orders", "orders-api", "orders-r1", "Order", "query.order")
	orders.EntityFetches = []schema.EntityFetch{{Type: "Order", OperationID: "query.order", Keys: []string{"id"}}}
	users := runtimeFederationManifest(t, "users", "users-api", "users-r1", "User", "query.user")
	users.EntityFetches = []schema.EntityFetch{{
		Type: "User", OperationID: "query.user", Keys: []string{"id"},
		Requires: []schema.EntityFetchDependency{{ServiceID: "orders", Type: "Order"}},
	}}
	manifests := []schema.ServiceManifest{users, orders}
	trust := map[string]schema.ServiceTrust{"orders": runtimeFederationTrust(orders), "users": runtimeFederationTrust(users)}
	if includeInventory {
		inventory := runtimeFederationManifest(t, "inventory", "inventory-api", "inventory-r1", "Inventory", "query.inventory")
		inventory.EntityFetches = []schema.EntityFetch{{Type: "Inventory", OperationID: "query.inventory", Keys: []string{"id"}}}
		manifests = append(manifests, inventory)
		trust["inventory"] = runtimeFederationTrust(inventory)
	}
	composition, err := schema.ComposeFederation(manifests, schema.FederationOptions{Revision: "federation-r1", Services: trust})
	if err != nil {
		t.Fatal(err)
	}
	return composition
}

func federationPlannerLimits() runtime.FederationLimits {
	return runtime.FederationLimits{MaxCalls: 2, MaxCost: 10, MaxConcurrency: 2, MaxAttempts: 2}
}

func federationBindings(t *testing.T, composition schema.FederationComposition, invokers map[string]runtime.FederationInvoker) []runtime.FederationEndpointBinding {
	t.Helper()
	bindings := make([]runtime.FederationEndpointBinding, 0, len(invokers))
	for serviceID, invoker := range invokers {
		service, ok := composition.Service(serviceID)
		if !ok {
			t.Fatalf("missing service %q", serviceID)
		}
		bindings = append(bindings, runtime.FederationEndpointBinding{Reference: service.EndpointReference, Invoker: invoker})
	}
	return bindings
}
