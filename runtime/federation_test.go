package runtime_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestFederationDelegationVerifierCannotIssueOrCrossAudience(t *testing.T) {
	t.Parallel()
	seed := []byte("0123456789abcdef0123456789abcdef")
	privateKey := ed25519.NewKeyFromSeed(seed)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	issuer, err := runtime.NewFederationDelegationIssuer(runtime.FederationDelegationIssuerConfig{
		PrivateKey: privateKey, Issuer: "gateway", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := runtime.NewFederationDelegationVerifier(runtime.FederationDelegationVerifierConfig{
		PublicKey: privateKey.Public().(ed25519.PublicKey), Issuer: "gateway", Audience: "naatre:users", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, hasIssueAuthority := any(verifier).(interface {
		Issue(context.Context, runtime.FederationDelegationOptions) (string, error)
	}); hasIssueAuthority {
		t.Fatal("public verifier exposes delegation minting authority")
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	token, err := issuer.Issue(ctx, runtime.FederationDelegationOptions{
		Audience: "naatre:orders", RequestID: "request-1", OperationID: "query.order", SchemaRevision: "federation-r1",
		ServiceSchemaRevision: "orders-r1", ServiceSchemaDigest: "sha256:" + strings.Repeat("a", 64),
		Deadline: now.Add(time.Minute), Cost: 1, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(token, runtime.FederationDelegationExpectation{
		RequestID: "request-1", OperationID: "query.order", SchemaRevision: "federation-r1",
		ServiceSchemaRevision: "orders-r1", ServiceSchemaDigest: "sha256:" + strings.Repeat("a", 64),
	}); !errors.Is(err, runtime.ErrFederationDelegation) {
		t.Fatalf("cross-audience Verify = %v", err)
	}
}

func TestFederationDelegationRejectsForgeryAndWrongAudience(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	issuer := federationDelegationIssuerAt(t, now)
	verifier := federationDelegationVerifierAt(t, "naatre:users", now)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", Tenant: "tenant-1", AuthorizationRevision: "policy-r1",
		Claims: map[string]string{"private": "must-not-be-forwarded"},
	})
	token, err := issuer.Issue(ctx, runtime.FederationDelegationOptions{
		Audience: "naatre:users", RequestID: "request-1", OperationID: "query.user",
		SchemaRevision: "federation-r1", ServiceSchemaRevision: "users-r1",
		ServiceSchemaDigest: "sha256:" + strings.Repeat("a", 64), Deadline: now.Add(time.Minute), Cost: 3, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	delegation, err := verifier.Verify(token, runtime.FederationDelegationExpectation{
		RequestID: "request-1", OperationID: "query.user", SchemaRevision: "federation-r1",
		ServiceSchemaRevision: "users-r1", ServiceSchemaDigest: "sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if delegation.Subject != "user-1" || delegation.Tenant != "tenant-1" || delegation.AuthorizationRevision != "policy-r1" ||
		delegation.ServiceSchemaRevision != "users-r1" || delegation.ServiceSchemaDigest != "sha256:"+strings.Repeat("a", 64) ||
		delegation.Cost != 3 || delegation.Concurrency != 1 {
		t.Fatalf("delegation = %#v", delegation)
	}
	if strings.Contains(token, "must-not-be-forwarded") {
		t.Fatal("delegation forwards arbitrary principal claims")
	}

	payloadPart, signaturePart, found := strings.Cut(token, ".")
	if !found || signaturePart == "" {
		t.Fatal("issued delegation has no signature")
	}
	tamperedSignaturePrefix := byte('A')
	if signaturePart[0] == tamperedSignaturePrefix {
		tamperedSignaturePrefix = 'B'
	}
	tampered := payloadPart + "." + string(tamperedSignaturePrefix) + signaturePart[1:]
	for name, candidate := range map[string]string{
		"forged":         tampered,
		"wrong-audience": token,
	} {
		name, candidate := name, candidate
		t.Run(name, func(t *testing.T) {
			candidateVerifier := verifier
			expectation := runtime.FederationDelegationExpectation{
				RequestID: "request-1", OperationID: "query.user", SchemaRevision: "federation-r1",
				ServiceSchemaRevision: "users-r1", ServiceSchemaDigest: "sha256:" + strings.Repeat("a", 64),
			}
			if name == "wrong-audience" {
				candidateVerifier = federationDelegationVerifierAt(t, "naatre:orders", now)
			}
			if _, err := candidateVerifier.Verify(candidate, expectation); !errors.Is(err, runtime.ErrFederationDelegation) {
				t.Fatalf("Verify = %v", err)
			}
		})
	}
	if _, err := issuer.Issue(context.Background(), runtime.FederationDelegationOptions{
		Audience: "naatre:users", RequestID: "request-1", OperationID: "query.user",
		SchemaRevision: "federation-r1", ServiceSchemaRevision: "users-r1",
		ServiceSchemaDigest: "sha256:" + strings.Repeat("a", 64), Deadline: now.Add(time.Minute), Cost: 1, Concurrency: 1,
	}); !errors.Is(err, runtime.ErrFederationDelegation) {
		t.Fatalf("Issue without principal = %v", err)
	}
}

func TestFederationDelegationRejectsStaleServiceSchemaPin(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	issuer := federationDelegationIssuerAt(t, now)
	verifier := federationDelegationVerifierAt(t, "naatre:users", now)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	digest := "sha256:" + strings.Repeat("a", 64)
	token, err := issuer.Issue(ctx, runtime.FederationDelegationOptions{
		Audience: "naatre:users", RequestID: "request-1", OperationID: "query.user", SchemaRevision: "federation-r1",
		ServiceSchemaRevision: "users-r1", ServiceSchemaDigest: digest, Deadline: now.Add(time.Minute), Cost: 1, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, expectation := range map[string]runtime.FederationDelegationExpectation{
		"revision": {RequestID: "request-1", OperationID: "query.user", SchemaRevision: "federation-r1", ServiceSchemaRevision: "users-r2", ServiceSchemaDigest: digest},
		"digest":   {RequestID: "request-1", OperationID: "query.user", SchemaRevision: "federation-r1", ServiceSchemaRevision: "users-r1", ServiceSchemaDigest: "sha256:" + strings.Repeat("b", 64)},
	} {
		if _, err := verifier.Verify(token, expectation); !errors.Is(err, runtime.ErrFederationDelegation) {
			t.Errorf("%s Verify = %v", name, err)
		}
	}
}

func TestReferenceFederationCoordinatorPropagatesDelegationAndBoundsConcurrency(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	issuer := federationIssuer(t)
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	traceKey := struct{ name string }{"trace-context"}
	var active atomic.Int64
	var maximum atomic.Int64
	invoker := runtime.FederationInvokerFunc(func(ctx context.Context, invocation runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		if ctx.Value(traceKey) != "trace-1" {
			return runtime.FederationRemoteResult{}, errors.New("trace context was not propagated")
		}
		service, ok := composition.Service(invocation.ServiceID)
		if !ok {
			return runtime.FederationRemoteResult{}, errors.New("unknown fixture service")
		}
		verifier := federationDelegationVerifierAt(t, service.Audience, time.Now())
		delegation, err := verifier.Verify(invocation.Delegation, runtime.FederationDelegationExpectation{
			RequestID: "request-1", OperationID: invocation.OperationID,
			SchemaRevision: composition.Schema().Revision(), ServiceSchemaRevision: service.SchemaRevision,
			ServiceSchemaDigest: service.SchemaDigest,
		})
		if err != nil || delegation.Subject != "user-1" || delegation.Cost != invocation.Cost || delegation.Concurrency != 1 {
			return runtime.FederationRemoteResult{}, fmt.Errorf("invalid delegation: %#v: %w", delegation, err)
		}
		current := active.Add(1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		started <- struct{}{}
		select {
		case <-ctx.Done():
			active.Add(-1)
			return runtime.FederationRemoteResult{}, ctx.Err()
		case <-release:
		}
		active.Add(-1)
		return runtime.FederationRemoteResult{Data: invocation.OperationID, SchemaRevision: service.SchemaRevision}, nil
	})
	coordinator := newFederationCoordinator(t, composition, issuer, invoker, runtime.FederationLimits{
		MaxCalls: 3, MaxCost: 10, MaxConcurrency: 2, MaxAttempts: 1,
	})
	requestContext := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", Tenant: "tenant-1", AuthorizationRevision: "policy-r1",
	})
	requestContext = context.WithValue(requestContext, traceKey, "trace-1")
	ctx, cancel := context.WithTimeout(requestContext, time.Second)
	defer cancel()
	done := make(chan runtime.Outcome, 1)
	go func() {
		done <- coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
			SchemaRevision: composition.Schema().Revision(),
			Calls: []runtime.FederationCall{
				{ResponseKey: "first", ServiceID: "orders", OperationID: "query.order", Path: []any{"first"}, MaxAttempts: 1},
				{ResponseKey: "second", ServiceID: "users", OperationID: "query.user", Path: []any{"second"}, MaxAttempts: 1},
				{ResponseKey: "third", ServiceID: "users", OperationID: "query.user", Path: []any{"third"}, MaxAttempts: 1},
			},
		})
	}()
	awaitFederationStart(t, started)
	awaitFederationStart(t, started)
	select {
	case <-started:
		t.Fatal("third downstream started before request-wide concurrency was released")
	default:
	}
	close(release)
	outcome := <-done
	if len(outcome.Errors) != 0 || len(outcome.Data) != 3 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d", maximum.Load())
	}
}

func TestReferenceFederationCoordinatorPreservesPathsAndRejectsSchemaDrift(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	invoker := runtime.FederationInvokerFunc(func(_ context.Context, invocation runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		if invocation.OperationID == "query.order" {
			return runtime.FederationRemoteResult{Errors: []runtime.FederationRemoteError{{
				Code: "ORDER_MISSING", Message: "order unavailable", Path: []any{"record", uint64(0)},
			}}, SchemaRevision: "orders-r1"}, nil
		}
		return runtime.FederationRemoteResult{Data: "user-data", SchemaRevision: "users-r1"}, nil
	})
	coordinator := newFederationCoordinator(t, composition, codec, invoker, runtime.FederationLimits{
		MaxCalls: 2, MaxCost: 10, MaxConcurrency: 2, MaxAttempts: 1,
	})
	ctx, cancel := context.WithTimeout(runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", AuthorizationRevision: "policy-r1",
	}), time.Second)
	defer cancel()
	outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls: []runtime.FederationCall{
			{ResponseKey: "order", ServiceID: "orders", OperationID: "query.order", Path: []any{"viewer", "orders"}, MaxAttempts: 1},
			{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"viewer"}, MaxAttempts: 1},
		},
	})
	if len(outcome.Errors) != 1 || outcome.Data["user"] != "user-data" {
		t.Fatalf("errors = %#v", outcome.Errors)
	}
	if outcome.Errors[0].Code != "ORDER_MISSING" || !slices.Equal(outcome.Errors[0].Path, []any{"viewer", "orders", "record", uint64(0)}) {
		t.Fatalf("remote path = %#v", outcome.Errors[0])
	}
	if outcome.Errors[0].Message == "order unavailable" || outcome.Errors[0].Message != "downstream service reported an error" {
		t.Fatalf("unsafe remote message = %q", outcome.Errors[0].Message)
	}
}

func TestReferenceFederationCoordinatorSanitizesRemoteCodesMessagesAndPaths(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		return runtime.FederationRemoteResult{Errors: []runtime.FederationRemoteError{{
			Code: runtime.CodeUnauthorized, Message: "secret backend detail", Path: []any{map[string]any{"invalid": true}},
		}}, SchemaRevision: "users-r1"}, nil
	})
	coordinator := newFederationCoordinator(t, composition, codec, invoker, runtime.FederationLimits{
		MaxCalls: 1, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 1,
	})
	ctx, cancel := context.WithTimeout(runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", AuthorizationRevision: "policy-r1",
	}), time.Second)
	defer cancel()
	outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls: []runtime.FederationCall{{
			ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"viewer"}, MaxAttempts: 1,
		}},
	})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationUnavailable ||
		outcome.Errors[0].Message != "downstream service reported an error" || !slices.Equal(outcome.Errors[0].Path, []any{"viewer"}) {
		t.Fatalf("sanitized outcome = %#v", outcome)
	}
	if cause := errors.Unwrap(outcome.Errors[0]); cause == nil || !strings.Contains(cause.Error(), "secret backend detail") {
		t.Fatalf("private diagnostic = %v", cause)
	}
}

func TestReferenceFederationCoordinatorRejectsRemoteSchemaDrift(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		return runtime.FederationRemoteResult{Data: "stale", SchemaRevision: "users-r2"}, nil
	})
	coordinator := newFederationCoordinator(t, composition, codec, invoker, runtime.FederationLimits{
		MaxCalls: 1, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 1,
	})
	ctx, cancel := context.WithTimeout(runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", AuthorizationRevision: "policy-r1",
	}), time.Second)
	defer cancel()
	outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"viewer"}, MaxAttempts: 1}},
	})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationSchemaMismatch ||
		!slices.Equal(outcome.Errors[0].Path, []any{"viewer"}) || len(outcome.Data) != 0 {
		t.Fatalf("schema drift outcome = %#v", outcome)
	}
}

func TestReferenceFederationCoordinatorRejectsVersionSkewAndMultiplicativeFanOutBeforeInvocation(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	var calls atomic.Int64
	invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		calls.Add(1)
		return runtime.FederationRemoteResult{}, nil
	})
	coordinator := newFederationCoordinator(t, composition, codec, invoker, runtime.FederationLimits{
		MaxCalls: 2, MaxCost: 3, MaxConcurrency: 2, MaxAttempts: 3,
	})
	ctx, cancel := context.WithTimeout(runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-1", AuthorizationRevision: "policy-r1",
	}), time.Second)
	defer cancel()

	versionSkew := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: "federation-r0",
		Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 1}},
	})
	if len(versionSkew.Errors) != 1 || versionSkew.Errors[0].Code != runtime.CodeFederationSchemaMismatch {
		t.Fatalf("version skew outcome = %#v", versionSkew)
	}

	fanOut := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls: []runtime.FederationCall{
			{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 2},
			{ResponseKey: "order", ServiceID: "orders", OperationID: "query.order", Path: []any{"order"}, MaxAttempts: 2},
		},
	})
	if len(fanOut.Errors) != 1 || fanOut.Errors[0].Code != runtime.CodeResourceExhausted || calls.Load() != 0 {
		t.Fatalf("fan-out outcome = %#v, invocations = %d", fanOut, calls.Load())
	}
}

func TestReferenceFederationCoordinatorPropagatesTimeout(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	invoker := runtime.FederationInvokerFunc(func(ctx context.Context, _ runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		<-ctx.Done()
		return runtime.FederationRemoteResult{}, ctx.Err()
	})
	coordinator, err := runtime.NewReferenceFederationCoordinator(runtime.ReferenceFederationConfig{
		Composition: composition, Delegations: codec, Invoker: invoker,
		Limits:          runtime.FederationLimits{MaxCalls: 1, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 1},
		MaximumDuration: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 1}},
	})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeResourceExhausted {
		t.Fatalf("timeout outcome = %#v", outcome)
	}
}

func TestReferenceFederationCoordinatorReturnsWhenInvokerIgnoresDeadlineAndRetainsAccounting(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	blocked := make(chan struct{})
	var invocations atomic.Int64
	invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		invocations.Add(1)
		<-blocked
		return runtime.FederationRemoteResult{Data: "late", SchemaRevision: "users-r1"}, nil
	})
	coordinator, err := runtime.NewReferenceFederationCoordinator(runtime.ReferenceFederationConfig{
		Composition: composition, Delegations: codec, Invoker: invoker,
		Limits:          runtime.FederationLimits{MaxCalls: 1, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 1},
		MaximumDuration: 25 * time.Millisecond, AbandonGrace: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	plan := runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 1}},
	}
	first := executeFederationBounded(t, coordinator, ctx, plan)
	second := executeFederationBounded(t, coordinator, ctx, plan)
	if len(first.Errors) != 1 || first.Errors[0].Code != runtime.CodeResourceExhausted ||
		len(second.Errors) != 1 || second.Errors[0].Code != runtime.CodeResourceExhausted || invocations.Load() != 1 {
		t.Fatalf("first=%#v second=%#v invocations=%d", first, second, invocations.Load())
	}
	close(blocked)
}

func TestReferenceFederationCoordinatorDistinguishesParentCancellation(t *testing.T) {
	t.Parallel()
	composition := federationComposition(t)
	codec := federationIssuer(t)
	invoker := runtime.FederationInvokerFunc(func(ctx context.Context, _ runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		<-ctx.Done()
		return runtime.FederationRemoteResult{}, ctx.Err()
	})
	coordinator := newFederationCoordinator(t, composition, codec, invoker, runtime.FederationLimits{
		MaxCalls: 1, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 1,
	})
	ctx, cancel := context.WithCancel(runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"}))
	cancel()
	outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 1}},
	})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeCancelled {
		t.Fatalf("cancelled outcome = %#v", outcome)
	}
}

func TestReferenceFederationCoordinatorDerivesCostAndRetrySafetyFromSchema(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		retrySafe bool
		cost      uint64
		wantCode  string
	}{
		{name: "non-retry-safe", retrySafe: false, cost: 1, wantCode: runtime.CodeFederationPlanInvalid},
		{name: "zero-cost-still-charged", retrySafe: true, cost: 0, wantCode: runtime.CodeResourceExhausted},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			manifest := runtimeFederationManifestWithPolicy(t, "users", "users-api", "users-r1", "User", "query.user", test.retrySafe, test.cost)
			composition, err := schema.ComposeFederation([]schema.ServiceManifest{manifest}, schema.FederationOptions{
				Revision: "federation-r1", Services: map[string]schema.ServiceTrust{"users": runtimeFederationTrust(manifest)},
			})
			if err != nil {
				t.Fatal(err)
			}
			codec := federationIssuer(t)
			var invocations atomic.Int64
			invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
				invocations.Add(1)
				return runtime.FederationRemoteResult{}, nil
			})
			coordinator := newFederationCoordinator(t, composition, codec, invoker, runtime.FederationLimits{
				MaxCalls: 1, MaxCost: 1, MaxConcurrency: 1, MaxAttempts: 2,
			})
			ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
			outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
				SchemaRevision: composition.Schema().Revision(),
				Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 2}},
			})
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != test.wantCode || invocations.Load() != 0 {
				t.Fatalf("outcome=%#v invocations=%d", outcome, invocations.Load())
			}
		})
	}
}

func TestReferenceFederationCoordinatorIsolatesInputsRetriesAndRemoteResults(t *testing.T) {
	composition := federationComposition(t)
	sharedInput := map[string]any{
		"nested": map[string]any{"value": "original"},
		"list":   []any{"original"},
	}
	remoteData := map[string]any{"nested": map[string]any{"value": "remote-original"}}
	var invocations atomic.Int64
	invoker := runtime.FederationInvokerFunc(func(_ context.Context, invocation runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		input := invocation.Input.(map[string]any)
		if input["nested"].(map[string]any)["value"] != "original" || input["list"].([]any)[0] != "original" {
			return runtime.FederationRemoteResult{}, errors.New("federation input leaked a sibling or retry mutation")
		}
		input["nested"].(map[string]any)["value"] = "mutated"
		input["list"].([]any)[0] = "mutated"
		switch invocations.Add(1) {
		case 1:
			return runtime.FederationRemoteResult{}, errors.New("retry")
		case 2:
			return runtime.FederationRemoteResult{Data: remoteData, SchemaRevision: "users-r1"}, nil
		default:
			return runtime.FederationRemoteResult{Data: map[string]any{"ok": true}, SchemaRevision: "users-r1"}, nil
		}
	})
	coordinator := newFederationCoordinator(t, composition, federationIssuer(t), invoker, runtime.FederationLimits{
		MaxCalls: 2, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 2,
	})
	plan := runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls: []runtime.FederationCall{
			{ResponseKey: "first", ServiceID: "users", OperationID: "query.user", Input: sharedInput, Path: []any{"first"}, MaxAttempts: 2},
			{ResponseKey: "second", ServiceID: "users", OperationID: "query.user", Input: sharedInput, Path: []any{"second"}, MaxAttempts: 1},
		},
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	outcome := coordinator.Execute(ctx, "request-1", plan)
	if len(outcome.Errors) != 0 || invocations.Load() != 3 {
		t.Fatalf("outcome=%#v invocations=%d", outcome, invocations.Load())
	}
	if sharedInput["nested"].(map[string]any)["value"] != "original" || sharedInput["list"].([]any)[0] != "original" {
		t.Fatalf("plan input mutated: %#v", sharedInput)
	}
	remoteData["nested"].(map[string]any)["value"] = "remote-mutated"
	if outcome.Data["first"].(map[string]any)["nested"].(map[string]any)["value"] != "remote-original" {
		t.Fatalf("outcome retained invoker-owned data: %#v", outcome.Data["first"])
	}

	typedNilInput := map[string]any{
		"map":   map[string]any(nil),
		"slice": []any(nil),
	}
	typedNilInvoker := runtime.FederationInvokerFunc(func(_ context.Context, invocation runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		input := invocation.Input.(map[string]any)
		if value, ok := input["map"].(map[string]any); !ok || value != nil {
			t.Fatalf("typed nil map input = %#v", input["map"])
		}
		if value, ok := input["slice"].([]any); !ok || value != nil {
			t.Fatalf("typed nil slice input = %#v", input["slice"])
		}
		return runtime.FederationRemoteResult{
			Data: map[string]any{"map": map[string]any(nil), "slice": []any(nil)}, SchemaRevision: "users-r1",
		}, nil
	})
	typedNilCoordinator := newFederationCoordinator(t, composition, federationIssuer(t), typedNilInvoker, runtime.FederationLimits{
		MaxCalls: 1, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 1,
	})
	typedNilPlan := runtime.FederationPlan{
		SchemaRevision: composition.Schema().Revision(),
		Calls: []runtime.FederationCall{{
			ResponseKey: "nil", ServiceID: "users", OperationID: "query.user", Input: typedNilInput, Path: []any{"nil"}, MaxAttempts: 1,
		}},
	}
	typedNilOutcome := typedNilCoordinator.Execute(ctx, "request-nil", typedNilPlan)
	if len(typedNilOutcome.Errors) != 0 {
		t.Fatalf("typed nil outcome errors = %#v", typedNilOutcome.Errors)
	}
	typedNilData := typedNilOutcome.Data["nil"].(map[string]any)
	if value, ok := typedNilData["map"].(map[string]any); !ok || value != nil {
		t.Fatalf("typed nil map output = %#v", typedNilData["map"])
	}
	if value, ok := typedNilData["slice"].([]any); !ok || value != nil {
		t.Fatalf("typed nil slice output = %#v", typedNilData["slice"])
	}
}

func TestReferenceFederationCoordinatorRejectsUnsafePlansBeforeInvocation(t *testing.T) {
	queryComposition := federationComposition(t)
	var invocations atomic.Int64
	invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		invocations.Add(1)
		return runtime.FederationRemoteResult{}, nil
	})
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
	cyclicInput := map[string]any{}
	cyclicInput["self"] = cyclicInput
	oversizedPath := make([]any, 129)
	for _, test := range []struct {
		name string
		call runtime.FederationCall
	}{
		{name: "unsupported-input", call: runtime.FederationCall{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Input: make(chan int), Path: []any{"user"}, MaxAttempts: 1}},
		{name: "empty-path", call: runtime.FederationCall{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", MaxAttempts: 1}},
		{name: "invalid-path", call: runtime.FederationCall{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{int64(-1)}, MaxAttempts: 1}},
		{name: "oversized-path", call: runtime.FederationCall{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: oversizedPath, MaxAttempts: 1}},
		{name: "cyclic-input", call: runtime.FederationCall{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Input: cyclicInput, Path: []any{"user"}, MaxAttempts: 1}},
		{name: "deep-input", call: runtime.FederationCall{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Input: deeplyNestedFederationValue(65), Path: []any{"user"}, MaxAttempts: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newFederationCoordinator(t, queryComposition, federationIssuer(t), invoker, runtime.FederationLimits{
				MaxCalls: 1, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 1,
			})
			outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{SchemaRevision: queryComposition.Schema().Revision(), Calls: []runtime.FederationCall{test.call}})
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationPlanInvalid || invocations.Load() != 0 {
				t.Fatalf("outcome=%#v invocations=%d", outcome, invocations.Load())
			}
		})
	}
	boundedCoordinator := newFederationCoordinator(t, queryComposition, federationIssuer(t), invoker, runtime.FederationLimits{
		MaxCalls: 1, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 1,
	})
	tooManyCalls := []runtime.FederationCall{
		{ResponseKey: "first", ServiceID: "users", OperationID: "query.user", Path: []any{"first"}, MaxAttempts: 1},
		{ResponseKey: "second", ServiceID: "users", OperationID: "query.user", Path: []any{"second"}, MaxAttempts: 1},
	}
	outcome := boundedCoordinator.Execute(ctx, "request-1", runtime.FederationPlan{SchemaRevision: queryComposition.Schema().Revision(), Calls: tooManyCalls})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationPlanInvalid || invocations.Load() != 0 {
		t.Fatalf("oversized plan outcome=%#v invocations=%d", outcome, invocations.Load())
	}

	writeManifest := runtimeFederationManifestWithSemantics(t, "users", "users-api", "users-r1", "User", "mutation.user", true, 1, protocol.Mutation, "write")
	writeComposition, err := schema.ComposeFederation([]schema.ServiceManifest{writeManifest}, schema.FederationOptions{
		Revision: "federation-r1", Services: map[string]schema.ServiceTrust{"users": runtimeFederationTrust(writeManifest)},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newFederationCoordinator(t, writeComposition, federationIssuer(t), invoker, runtime.FederationLimits{
		MaxCalls: 1, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 1,
	})
	outcome = coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
		SchemaRevision: writeComposition.Schema().Revision(),
		Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "mutation.user", Path: []any{"user"}, MaxAttempts: 1}},
	})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationPlanInvalid || invocations.Load() != 0 {
		t.Fatalf("write outcome=%#v invocations=%d", outcome, invocations.Load())
	}
}

func TestReferenceFederationCoordinatorContainsUnsafeDownstreamResults(t *testing.T) {
	composition := federationComposition(t)
	cyclicResult := map[string]any{}
	cyclicResult["self"] = cyclicResult
	for _, test := range []struct {
		name   string
		invoke func() runtime.FederationRemoteResult
	}{
		{name: "cyclic-result", invoke: func() runtime.FederationRemoteResult {
			return runtime.FederationRemoteResult{Data: cyclicResult, SchemaRevision: "users-r1"}
		}},
		{name: "deep-result", invoke: func() runtime.FederationRemoteResult {
			return runtime.FederationRemoteResult{Data: deeplyNestedFederationValue(65), SchemaRevision: "users-r1"}
		}},
		{name: "panic", invoke: func() runtime.FederationRemoteResult {
			panic("secret downstream panic")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var invocations atomic.Int64
			invoker := runtime.FederationInvokerFunc(func(context.Context, runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
				invocations.Add(1)
				return test.invoke(), nil
			})
			coordinator := newFederationCoordinator(t, composition, federationIssuer(t), invoker, runtime.FederationLimits{
				MaxCalls: 1, MaxCost: 10, MaxConcurrency: 1, MaxAttempts: 1,
			})
			ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-1", AuthorizationRevision: "policy-r1"})
			outcome := coordinator.Execute(ctx, "request-1", runtime.FederationPlan{
				SchemaRevision: composition.Schema().Revision(),
				Calls:          []runtime.FederationCall{{ResponseKey: "user", ServiceID: "users", OperationID: "query.user", Path: []any{"user"}, MaxAttempts: 1}},
			})
			if invocations.Load() != 1 || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeFederationUnavailable || outcome.Data["user"] != nil {
				t.Fatalf("outcome=%#v invocations=%d", outcome, invocations.Load())
			}
			if strings.Contains(outcome.Errors[0].Message, "secret downstream panic") {
				t.Fatalf("panic detail leaked: %#v", outcome.Errors[0])
			}
		})
	}
}

func deeplyNestedFederationValue(depth int) any {
	value := any("leaf")
	for range depth {
		value = []any{value}
	}
	return value
}

func executeFederationBounded(t *testing.T, coordinator *runtime.ReferenceFederationCoordinator, ctx context.Context, plan runtime.FederationPlan) runtime.Outcome {
	t.Helper()
	done := make(chan runtime.Outcome, 1)
	go func() { done <- coordinator.Execute(ctx, "request-1", plan) }()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(time.Second):
		t.Fatal("federation coordinator did not return within its bound")
		return runtime.Outcome{}
	}
}

func awaitFederationStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream invocation")
	}
}

func federationComposition(t *testing.T) schema.FederationComposition {
	t.Helper()
	users := runtimeFederationManifest(t, "users", "users-api", "users-r1", "User", "query.user")
	orders := runtimeFederationManifest(t, "orders", "orders-api", "orders-r1", "Order", "query.order")
	composition, err := schema.ComposeFederation([]schema.ServiceManifest{users, orders}, schema.FederationOptions{
		Revision: "federation-r1", Services: map[string]schema.ServiceTrust{
			"users":  runtimeFederationTrust(users),
			"orders": runtimeFederationTrust(orders),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return composition
}

func runtimeFederationManifest(t *testing.T, serviceID, endpoint, revision, typeID, operationID string) schema.ServiceManifest {
	return runtimeFederationManifestWithPolicy(t, serviceID, endpoint, revision, typeID, operationID, true, 1)
}

func runtimeFederationManifestWithPolicy(t *testing.T, serviceID, endpoint, revision, typeID, operationID string, retrySafe bool, cost uint64) schema.ServiceManifest {
	return runtimeFederationManifestWithSemantics(t, serviceID, endpoint, revision, typeID, operationID, retrySafe, cost, protocol.Query, "read")
}

func runtimeFederationManifestWithSemantics(t *testing.T, serviceID, endpoint, revision, typeID, operationID string, retrySafe bool, cost uint64, kind protocol.OperationKind, effect string) schema.ServiceManifest {
	t.Helper()
	input := fmt.Sprintf(`{
		"version":"1","canonicalVersion":"c14n-1","revision":%q,
		"types":[{"id":%q,"name":%q,"kind":"object","output":true,"entity":{"keys":["id"]},"fields":[{"id":%q,"name":"id","type":"ID"}]}],
		"operations":[{"id":%q,"name":%q,"kind":%q,"output":%q,"effect":%q,"retrySafe":%t,"cost":%d}],
		"members":[]
	}`, revision, typeID, typeID, typeID+".id", operationID, strings.TrimPrefix(strings.TrimPrefix(operationID, "query."), "mutation."), kind, typeID, effect, retrySafe, cost)
	document, err := schema.ParseDocument([]byte(input), schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := document.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return schema.ServiceManifest{
		ID: serviceID, Audience: "naatre:" + serviceID, EndpointReference: endpoint,
		FederationProfile: "core.federation-1",
		Schema:            document, SchemaRevision: revision, SchemaDigest: "sha256:" + digest.Hex,
		OwnedOperations: []string{operationID},
	}
}

func runtimeFederationTrust(manifest schema.ServiceManifest) schema.ServiceTrust {
	return schema.ServiceTrust{
		Audience: manifest.Audience, EndpointReference: manifest.EndpointReference,
		FederationProfile: manifest.FederationProfile, SchemaRevision: manifest.SchemaRevision, SchemaDigest: manifest.SchemaDigest,
	}
}

func federationDelegationPrivateKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
}

func federationDelegationIssuerAt(t *testing.T, now time.Time) *runtime.FederationDelegationIssuer {
	return federationDelegationIssuer(t, func() time.Time { return now })
}

func federationDelegationIssuer(t *testing.T, now func() time.Time) *runtime.FederationDelegationIssuer {
	issuer, err := runtime.NewFederationDelegationIssuer(runtime.FederationDelegationIssuerConfig{
		PrivateKey: federationDelegationPrivateKey(), Issuer: "gateway", Now: now,
	})
	return mustFederationValue(t, issuer, err)
}

func federationDelegationVerifierAt(t *testing.T, audience string, now time.Time) *runtime.FederationDelegationVerifier {
	verifier, err := runtime.NewFederationDelegationVerifier(runtime.FederationDelegationVerifierConfig{
		PublicKey: federationDelegationPrivateKey().Public().(ed25519.PublicKey), Issuer: "gateway", Audience: audience,
		Now: func() time.Time { return now },
	})
	return mustFederationValue(t, verifier, err)
}

func federationIssuer(t *testing.T) *runtime.FederationDelegationIssuer {
	return federationDelegationIssuer(t, nil)
}

func newFederationCoordinator(t *testing.T, composition schema.FederationComposition, issuer *runtime.FederationDelegationIssuer, invoker runtime.FederationInvoker, limits runtime.FederationLimits) *runtime.ReferenceFederationCoordinator {
	coordinator, err := runtime.NewReferenceFederationCoordinator(runtime.ReferenceFederationConfig{
		Composition: composition, Delegations: issuer, Invoker: invoker, Limits: limits, MaximumDuration: time.Second,
	})
	return mustFederationValue(t, coordinator, err)
}

func mustFederationValue[T any](t *testing.T, value *T, err error) *T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
