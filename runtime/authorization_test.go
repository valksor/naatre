package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

func TestAuthorizationDenyByDefaultNeverInvokesProtectedHandler(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","as":"safe","select":[{"$field":{"name":"value"}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", Tenant: "tenant-a"}))
	if calls.Load() != 0 {
		t.Fatalf("unauthorized handler ran %d times", calls.Load())
	}
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeUnauthorized || !slices.Equal(outcome.Errors[0].Path, []any{"safe"}) {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	if len(outcome.Data) != 0 || outcome.Errors[0].Message != "access denied" || outcome.Errors[0].Details != nil {
		t.Fatalf("unsafe denial shape: data=%#v error=%#v", outcome.Data, outcome.Errors[0])
	}
}

func TestAuthorizationPolicyDistinguishesOperationsAndReceivesDynamicObject(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var seen []runtime.AuthorizationRequest
	var mu sync.Mutex
	policy := runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
		mu.Lock()
		seen = append(seen, request)
		mu.Unlock()
		return runtime.AuthorizationDecision{Allowed: request.Operation == protocol.Query && request.Principal.Subject == "reader"}, nil
	})
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault, Authorizer: policy}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	query := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, query)
	if err != nil {
		t.Fatalf("Prepare query: %v", err)
	}
	outcome := plan.Execute(runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "reader"}))
	if len(outcome.Errors) != 0 || calls.Load() != 2 {
		t.Fatalf("query outcome=%#v calls=%d", outcome, calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0].Object != nil || seen[1].Descriptor.Name != "value" {
		t.Fatalf("authorization requests = %#v", seen)
	}
	object, ok := seen[1].Object.(map[string]any)
	if !ok || object["value"] != "classified" {
		t.Fatalf("dynamic object = %#v", seen[1].Object)
	}
	if seen[1].Descriptor.Metadata.AuthorizationPolicy != "test" || len(seen[1].Path) != 2 {
		t.Fatalf("safe policy metadata = %#v", seen[1])
	}
}

func TestPlanningAuthorizationRejectsMutationBeforeExecution(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	planning := runtime.PlanningAuthorizerFunc(func(request runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
		return runtime.AuthorizationDecision{Allowed: request.Operation == protocol.Query}, nil
	})
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault, PlanningAuthorizer: planning}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"change"}}]}]}}`)
	_, err = runtime.Prepare(snapshot, request)
	var validation *runtime.ValidationErrors
	if !errors.As(err, &validation) || !slices.ContainsFunc(validation.Issues(), func(issue runtime.ValidationIssue) bool {
		return issue.Diagnostic.Code == "POLICY_DENIED"
	}) {
		t.Fatalf("Prepare error = %#v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("planning policy invoked %d handlers", calls.Load())
	}
}

func TestInterceptorOrderIsDeterministicAndNextIsOneShot(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var events []string
	var mu sync.Mutex
	levels := []runtime.InterceptorLevel{
		runtime.GlobalInterceptor, runtime.OperationInterceptor, runtime.TypeInterceptor,
		runtime.FieldInterceptor, runtime.HandlerInterceptorLevel,
	}
	for _, level := range levels {
		level := level
		interceptor := runtime.HandlerInterceptorFunc(func(_ context.Context, invocation runtime.HandlerInvocation, next runtime.HandlerNext) (any, error) {
			if invocation.Descriptor.Name == "value" {
				mu.Lock()
				events = append(events, "before:"+string(level))
				mu.Unlock()
			}
			value, err := next()
			if invocation.Descriptor.Name == "value" {
				mu.Lock()
				events = append(events, "after:"+string(level))
				mu.Unlock()
			}
			return value, err
		})
		registration := runtime.InterceptorRegistration{Level: level, Interceptor: interceptor}
		switch level {
		case runtime.OperationInterceptor:
			registration.Operation = protocol.Query
		case runtime.TypeInterceptor:
			registration.Owner = "Secret"
		case runtime.FieldInterceptor, runtime.HandlerInterceptorLevel:
			registration.Owner, registration.Member = "Secret", "value"
		case runtime.GlobalInterceptor:
		}
		if err := registry.RegisterInterceptor(registration); err != nil {
			t.Fatalf("RegisterInterceptor(%s): %v", level, err)
		}
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	want := []string{
		"before:global", "before:operation", "before:type", "before:field", "before:handler",
		"after:handler", "after:field", "after:type", "after:operation", "after:global",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}

	registry = authorizationRegistry(t, &calls)
	if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{
		Level: runtime.GlobalInterceptor,
		Interceptor: runtime.HandlerInterceptorFunc(func(_ context.Context, _ runtime.HandlerInvocation, next runtime.HandlerNext) (any, error) {
			if _, err := next(); err != nil {
				return nil, err
			}
			return next()
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = registry.Freeze()
	plan, err = runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare one-shot interceptor plan: %v", err)
	}
	before := calls.Load()
	outcome = plan.Execute(context.Background())
	if calls.Load() != before+1 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeInternal {
		t.Fatalf("one-shot next: calls=%d before=%d errors=%#v", calls.Load(), before, outcome.Errors)
	}
}

func TestPrincipalContextCopiesClaims(t *testing.T) {
	t.Parallel()
	claims := map[string]string{"role": "reader"}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", Claims: claims})
	claims["role"] = "admin"
	principal, ok := runtime.PrincipalFromContext(ctx)
	if !ok || principal.Claims["role"] != "reader" {
		t.Fatalf("principal = %#v, %v", principal, ok)
	}
	principal.Claims["role"] = "mutated"
	again, _ := runtime.PrincipalFromContext(ctx)
	if again.Claims["role"] != "reader" {
		t.Fatalf("stored principal mutated: %#v", again)
	}
}

func TestAuthorizationAcrossAliasesFragmentsParallelGroupsAndNestedCalls(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var paths []string
	var mu sync.Mutex
	policy := runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
		path := securityPath(request.Path)
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		return runtime.AuthorizationDecision{Allowed: path != "hidden"}, nil
	})
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault, Authorizer: policy}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$parallel":{"select":[{"$call":{"name":"secret","as":"visible","select":[{"$fragment":{"name":"Protected"}}]}},{"$call":{"name":"secret","as":"hidden","select":[{"$field":{"name":"value"}}]}}]}}]}],"fragments":[{"name":"Protected","on":"Secret","select":[{"$call":{"name":"detail","as":"nested","args":{}}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "reader"}))
	if calls.Load() != 2 {
		t.Fatalf("handlers ran %d times, want visible root and nested call only: outcome=%#v", calls.Load(), outcome)
	}
	if got := outcome.Data["visible"]; !reflect.DeepEqual(got, map[string]any{"nested": "classified:detail"}) {
		t.Fatalf("visible data = %#v", got)
	}
	if _, exists := outcome.Data["hidden"]; exists {
		t.Fatalf("denied alias leaked data: %#v", outcome.Data)
	}
	if len(outcome.Errors) != 1 || !slices.Equal(outcome.Errors[0].Path, []any{"hidden"}) || outcome.Errors[0].Code != runtime.CodeUnauthorized {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	mu.Lock()
	slices.Sort(paths)
	gotPaths := slices.Clone(paths)
	mu.Unlock()
	if !slices.Equal(gotPaths, []string{"hidden", "visible", "visible/nested"}) {
		t.Fatalf("authorization paths = %v", gotPaths)
	}
}

func TestCollectionDenialHidesCountsValuesAndMetadata(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{Allowed: request.Descriptor.Name != "secrets"}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secrets","as":"vault","select":[{"$meta":{"name":"count","as":"total"}},{"$map":{"as":"items","select":[{"$field":{"name":"value"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if calls.Load() != 0 || len(outcome.Data) != 0 {
		t.Fatalf("forbidden collection leaked work or data: calls=%d data=%#v", calls.Load(), outcome.Data)
	}
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeUnauthorized || !slices.Equal(outcome.Errors[0].Path, []any{"vault"}) {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
}

func TestAuthorizationPolicyDistinguishesEveryOperationKind(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var operations []protocol.OperationKind
	var mu sync.Mutex
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			mu.Lock()
			operations = append(operations, request.Operation)
			mu.Unlock()
			return runtime.AuthorizationDecision{Allowed: true}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"change"}}]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"S","kind":"subscription","select":[{"$call":{"name":"watch"}}]}]}}`,
	} {
		plan, prepareErr := runtime.Prepare(snapshot, decodeRuntimeRequest(t, body))
		if prepareErr != nil {
			t.Fatalf("Prepare: %v", prepareErr)
		}
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
			t.Fatalf("Execute: %#v", outcome.Errors)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, kind := range []protocol.OperationKind{protocol.Query, protocol.Mutation, protocol.Subscription} {
		if !slices.Contains(operations, kind) {
			t.Fatalf("policy did not observe %s: %v", kind, operations)
		}
	}
}

func TestAuthorizationDecisionLifecycleFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		principal runtime.Principal
		decision  runtime.AuthorizationDecision
		allowed   bool
	}{
		{name: "expired", principal: runtime.Principal{Subject: "reader"}, decision: runtime.AuthorizationDecision{Allowed: true, ExpiresAt: time.Now().Add(-time.Minute)}},
		{name: "revision-mismatch", principal: runtime.Principal{Subject: "reader", AuthorizationRevision: "new"}, decision: runtime.AuthorizationDecision{Allowed: true, AuthorizationRevision: "old"}},
		{name: "unknown-cache-scope", principal: runtime.Principal{Subject: "reader"}, decision: runtime.AuthorizationDecision{Allowed: true, CacheScope: "unknown"}},
		{name: "principal-scope-without-subject", decision: runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCachePrincipal}},
		{name: "tenant-scope-without-tenant", principal: runtime.Principal{Subject: "reader"}, decision: runtime.AuthorizationDecision{Allowed: true, CacheScope: runtime.AuthorizationCacheTenant}},
		{name: "current-principal-decision", principal: runtime.Principal{Subject: "reader", Tenant: "tenant-a", AuthorizationRevision: "r2"}, decision: runtime.AuthorizationDecision{Allowed: true, AuthorizationRevision: "r2", ExpiresAt: time.Now().Add(time.Hour), CacheScope: runtime.AuthorizationCachePrincipal}, allowed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			registry := authorizationRegistry(t, &calls)
			if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault, Authorizer: runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
				return test.decision, nil
			})}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := registry.Freeze()
			if err != nil {
				t.Fatal(err)
			}
			plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
			if err != nil {
				t.Fatal(err)
			}
			outcome := plan.Execute(runtime.WithPrincipal(context.Background(), test.principal))
			if test.allowed {
				if len(outcome.Errors) != 0 || calls.Load() != 2 {
					t.Fatalf("allowed outcome=%#v calls=%d", outcome, calls.Load())
				}
				return
			}
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeUnauthorized || calls.Load() != 0 {
				t.Fatalf("denied outcome=%#v calls=%d", outcome, calls.Load())
			}
		})
	}
}

func TestAuthorizationCallbacksPanicAndErrorsFailClosedWithoutLeaks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		authorizer runtime.Authorizer
	}{
		{name: "panic", authorizer: runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			panic("policy-secret")
		})},
		{name: "error", authorizer: runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			return runtime.AuthorizationDecision{}, errors.New("policy-secret")
		})},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			registry := authorizationRegistry(t, &calls)
			if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault, Authorizer: test.authorizer}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := registry.Freeze()
			if err != nil {
				t.Fatal(err)
			}
			plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
			if err != nil {
				t.Fatal(err)
			}
			outcome := plan.Execute(context.Background())
			encoded, marshalErr := json.Marshal(outcome)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if calls.Load() != 0 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeUnauthorized || strings.Contains(string(encoded), "policy-secret") {
				t.Fatalf("unsafe fail-closed outcome=%s calls=%d", encoded, calls.Load())
			}
		})
	}

	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault, PlanningAuthorizer: runtime.PlanningAuthorizerFunc(func(runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
		panic("planning-secret")
	})}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
	var validation *runtime.ValidationErrors
	if !errors.As(err, &validation) || len(validation.Issues()) == 0 || strings.Contains(err.Error(), "planning-secret") || calls.Load() != 0 {
		t.Fatalf("planning panic did not fail closed: err=%v calls=%d", err, calls.Load())
	}
}

func TestInterceptorsCannotBypassCancellationOrExposeErrors(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	ctx, cancel := context.WithCancel(context.Background())
	if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{
		Level: runtime.GlobalInterceptor,
		Interceptor: runtime.HandlerInterceptorFunc(func(context.Context, runtime.HandlerInvocation, runtime.HandlerNext) (any, error) {
			cancel()
			return "policy-secret", nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"change"}}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(ctx)
	encoded, marshalErr := json.Marshal(outcome)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if calls.Load() != 0 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeCancelled || strings.Contains(string(encoded), "policy-secret") {
		t.Fatalf("cancellation bypass: outcome=%s calls=%d", encoded, calls.Load())
	}

	var errorCalls atomic.Int64
	errorRegistry := authorizationRegistry(t, &errorCalls)
	if err := errorRegistry.RegisterInterceptor(runtime.InterceptorRegistration{
		Level: runtime.GlobalInterceptor,
		Interceptor: runtime.HandlerInterceptorFunc(func(context.Context, runtime.HandlerInvocation, runtime.HandlerNext) (any, error) {
			return nil, errors.New("interceptor-secret")
		}),
	}); err != nil {
		t.Fatal(err)
	}
	errorSnapshot, err := errorRegistry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	errorPlan, err := runtime.Prepare(errorSnapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"change"}}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	errorOutcome := errorPlan.Execute(context.Background())
	errorJSON, marshalErr := json.Marshal(errorOutcome)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if errorCalls.Load() != 0 || len(errorOutcome.Errors) != 1 || errorOutcome.Errors[0].Code != runtime.CodeHandlerFailed || strings.Contains(string(errorJSON), "interceptor-secret") {
		t.Fatalf("interceptor error leak: outcome=%s calls=%d", errorJSON, errorCalls.Load())
	}
}

func TestAuthorizationRequestsNeverContainRawArguments(t *testing.T) {
	t.Parallel()
	const sensitive = "do-not-disclose-this-argument"
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var observed [][]byte
	var mu sync.Mutex
	record := func(value any) {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Errorf("marshal policy request: %v", err)
			return
		}
		mu.Lock()
		observed = append(observed, encoded)
		mu.Unlock()
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{
		Mode: runtime.AuthorizationDenyByDefault,
		PlanningAuthorizer: runtime.PlanningAuthorizerFunc(func(request runtime.PlanningAuthorizationRequest) (runtime.AuthorizationDecision, error) {
			record(request)
			return runtime.AuthorizationDecision{Allowed: true}, nil
		}),
		Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			record(request)
			return runtime.AuthorizationDecision{Allowed: true}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$call":{"name":"detail","args":{"ignored":{"$literal":"do-not-disclose-this-argument"}}}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
		t.Fatalf("Execute: %#v", outcome.Errors)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) == 0 {
		t.Fatal("policy observed no requests")
	}
	for _, encoded := range observed {
		if strings.Contains(string(encoded), sensitive) || strings.Contains(string(encoded), "Arguments") {
			t.Fatalf("policy request leaked raw arguments: %s", encoded)
		}
	}
}

func TestInterceptorOrderPreservesRegistrationWithinLevel(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var events []string
	for _, name := range []string{"first", "second"} {
		name := name
		if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{
			Level: runtime.GlobalInterceptor,
			Interceptor: runtime.HandlerInterceptorFunc(func(_ context.Context, invocation runtime.HandlerInvocation, next runtime.HandlerNext) (any, error) {
				if invocation.Descriptor.Name == "change" {
					events = append(events, "before:"+name)
				}
				value, err := next()
				if invocation.Descriptor.Name == "change" {
					events = append(events, "after:"+name)
				}
				return value, err
			}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"change"}}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
		t.Fatalf("Execute: %#v", outcome.Errors)
	}
	want := []string{"before:first", "before:second", "after:second", "after:first"}
	if !slices.Equal(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestExactRootHandlerInterceptorIncludesOperationKind(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	var intercepted []protocol.OperationKind
	if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{
		Level: runtime.HandlerInterceptorLevel, Operation: protocol.Mutation, Member: "change",
		Interceptor: runtime.HandlerInterceptorFunc(func(_ context.Context, invocation runtime.HandlerInvocation, next runtime.HandlerNext) (any, error) {
			intercepted = append(intercepted, invocation.Operation)
			return next()
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`,
		`{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"change"}}]}]}}`,
	} {
		plan, prepareErr := runtime.Prepare(snapshot, decodeRuntimeRequest(t, body))
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if outcome := plan.Execute(context.Background()); len(outcome.Errors) != 0 {
			t.Fatalf("Execute: %#v", outcome.Errors)
		}
	}
	if !slices.Equal(intercepted, []protocol.OperationKind{protocol.Mutation}) {
		t.Fatalf("intercepted operations = %v", intercepted)
	}
}

func TestAuthorizationObjectIsIsolatedFromPolicyMutation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault, Authorizer: runtime.AuthorizerFunc(func(_ context.Context, request runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
		if object, ok := request.Object.(map[string]any); ok {
			object["value"] = "tampered"
		}
		return runtime.AuthorizationDecision{Allowed: true}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.Prepare(snapshot, decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"secret","select":[{"$field":{"name":"value"}}]}}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || !reflect.DeepEqual(outcome.Data, map[string]any{"secret": map[string]any{"value": "classified"}}) {
		t.Fatalf("policy mutated handler source: %#v", outcome)
	}
}

func TestInterceptorRegistrationValidationAndFreeze(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	interceptor := runtime.HandlerInterceptorFunc(func(_ context.Context, _ runtime.HandlerInvocation, next runtime.HandlerNext) (any, error) {
		return next()
	})
	invalid := []runtime.InterceptorRegistration{
		{},
		{Level: runtime.GlobalInterceptor, Owner: "Secret", Interceptor: interceptor},
		{Level: runtime.OperationInterceptor, Operation: protocol.Query, Owner: "Secret", Interceptor: interceptor},
		{Level: runtime.TypeInterceptor, Operation: protocol.Query, Owner: "Secret", Interceptor: interceptor},
		{Level: runtime.FieldInterceptor, Member: "value", Interceptor: interceptor},
		{Level: runtime.HandlerInterceptorLevel, Interceptor: interceptor},
		{Level: runtime.HandlerInterceptorLevel, Operation: protocol.Query, Owner: "Secret", Member: "value", Interceptor: interceptor},
	}
	for index, registration := range invalid {
		if err := registry.RegisterInterceptor(registration); err == nil {
			t.Fatalf("invalid registration %d accepted: %#v", index, registration)
		}
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: "unknown"}); err == nil {
		t.Fatal("unknown authorization mode accepted")
	}
	if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{Level: runtime.HandlerInterceptorLevel, Operation: protocol.Query, Member: "secret", Interceptor: interceptor}); err != nil {
		t.Fatalf("valid exact root interceptor: %v", err)
	}
	if _, err := registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{Level: runtime.GlobalInterceptor, Interceptor: interceptor}); err == nil {
		t.Fatal("interceptor registered after freeze")
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{}); err == nil {
		t.Fatal("authorization changed after freeze")
	}
}

func TestFreezeRejectsInterceptorWithoutRegisteredTarget(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	registry := authorizationRegistry(t, &calls)
	if err := registry.RegisterInterceptor(runtime.InterceptorRegistration{
		Level: runtime.FieldInterceptor, Owner: "Secret", Member: "missing",
		Interceptor: runtime.HandlerInterceptorFunc(func(_ context.Context, _ runtime.HandlerInvocation, next runtime.HandlerNext) (any, error) {
			return next()
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Freeze(); err == nil || !strings.Contains(err.Error(), "no registered target") {
		t.Fatalf("Freeze error = %v", err)
	}
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault}); err != nil {
		t.Fatalf("failed freeze must leave registry configurable: %v", err)
	}
}

func TestFreezeRequiresExplicitAuthorizationPosture(t *testing.T) {
	t.Parallel()
	// A registry that never chose an authorization posture must fail closed at
	// readiness rather than silently ship allow-all.
	unconfigured := runtime.NewRegistry(coreTypes(t))
	if _, err := unconfigured.Freeze(); err == nil || !strings.Contains(err.Error(), "authorization posture not configured") {
		t.Fatalf("Freeze without a posture = %v, want readiness failure", err)
	}
	// An unset or unknown mode is rejected rather than silently defaulted.
	if err := unconfigured.ConfigureAuthorization(runtime.AuthorizationConfig{}); err == nil {
		t.Fatal("empty authorization mode accepted")
	}
	if err := unconfigured.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: "unknown"}); err == nil {
		t.Fatal("unknown authorization mode accepted")
	}
	// Explicit allow-by-default is a valid, auditable opt-in that makes the
	// registry ready without changing runtime allow semantics.
	allow := runtime.NewRegistry(coreTypes(t))
	if err := allow.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("allow-by-default opt-in: %v", err)
	}
	if _, err := allow.Freeze(); err != nil {
		t.Fatalf("Freeze after explicit allow-by-default: %v", err)
	}
	// Deny-by-default remains available.
	deny := runtime.NewRegistry(coreTypes(t))
	if err := deny.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationDenyByDefault}); err != nil {
		t.Fatalf("deny-by-default opt-in: %v", err)
	}
	if _, err := deny.Freeze(); err != nil {
		t.Fatalf("Freeze after explicit deny-by-default: %v", err)
	}
}

func securityPath(path any) string {
	return strings.Trim(strings.ReplaceAll(fmt.Sprint(path), " ", "/"), "[]")
}

func authorizationRegistry(t testing.TB, calls *atomic.Int64) *runtime.Registry {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "DetailInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{"ignored": {Type: schema.TypeID(schema.String)}}},
		{ID: "Secret", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{"value": {Type: schema.TypeID(schema.String)}}},
		{ID: "Secrets", Kind: schema.ListType, Output: true, Element: "Secret"},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry(types)
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	metadata := completeMetadata(runtime.ReadEffect)
	registerComposition(t, registry,
		runtime.BindInvocation[map[string]any](runtime.Descriptor{Name: "secret", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Secret", Metadata: metadata}, func(context.Context, runtime.Invocation) (map[string]any, error) {
			calls.Add(1)
			return map[string]any{"value": "classified"}, nil
		}),
		runtime.BindInvocation[string](runtime.Descriptor{Name: "change", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.WriteEffect)}, func(context.Context, runtime.Invocation) (string, error) {
			calls.Add(1)
			return "changed", nil
		}),
		runtime.BindInvocation[string](runtime.Descriptor{Name: "watch", Scope: runtime.RootScope, Kind: protocol.Subscription, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata}, func(context.Context, runtime.Invocation) (string, error) {
			calls.Add(1)
			return "event", nil
		}),
		runtime.BindInvocation[[]map[string]any](runtime.Descriptor{Name: "secrets", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember, Input: schema.TypeID(schema.String), Output: "Secrets", Metadata: metadata}, func(context.Context, runtime.Invocation) ([]map[string]any, error) {
			calls.Add(1)
			return []map[string]any{{"value": "first"}, {"value": "second"}}, nil
		}),
		runtime.BindCall[map[string]any, schema.InputValue, string](runtime.Descriptor{Name: "detail", Scope: runtime.ObjectScope, Owner: "Secret", Member: runtime.CallMember, Input: "DetailInput", Output: schema.TypeID(schema.String), Metadata: metadata}, func(_ context.Context, source map[string]any, _ schema.InputValue) (string, error) {
			calls.Add(1)
			return source["value"].(string) + ":detail", nil
		}),
		runtime.BindField[map[string]any, string](runtime.Descriptor{Name: "value", Scope: runtime.ObjectScope, Owner: "Secret", Member: runtime.FieldMember, Output: schema.TypeID(schema.String), Metadata: metadata}, func(_ context.Context, source map[string]any) (string, error) {
			calls.Add(1)
			return source["value"].(string), nil
		}),
	)
	return registry
}
