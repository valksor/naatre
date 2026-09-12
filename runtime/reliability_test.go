package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestRetryPolicyUsesInjectedBackoffAndRandomness(t *testing.T) {
	t.Parallel()
	attempts := 0
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts++
		if attempts == 1 {
			return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
		}
		return "ready", nil
	})
	var delays []time.Duration
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
		Jitter:      0.5,
		Random:      func() float64 { return 1 },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}})
	if attempts != 2 || len(outcome.Errors) != 0 || outcome.Data["read"] != "ready" {
		t.Fatalf("attempts = %d, outcome = %#v", attempts, outcome)
	}
	if len(delays) != 1 || delays[0] != 15*time.Millisecond {
		t.Fatalf("delays = %v, want [15ms]", delays)
	}
	if outcome.Reliability.Attempts != 2 || outcome.Reliability.Deduplicated {
		t.Fatalf("reliability = %#v", outcome.Reliability)
	}
}

func TestNonRetrySafeHandlerIsNeverAutomaticallyRetried(t *testing.T) {
	t.Parallel()
	attempts := 0
	plan := reliabilityQueryPlan(t, false, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts++
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 5}})
	if attempts != 1 || outcome.Reliability.Attempts != 1 || len(outcome.Errors) != 1 {
		t.Fatalf("attempts = %d, outcome = %#v", attempts, outcome)
	}
}

func TestNonIdempotentQueryIsNeverAutomaticallyRetriedOrProtected(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyNonIdempotent, func(context.Context) (string, error) {
		calls.Add(1)
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	retried := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 5}})
	if calls.Load() != 1 || retried.Reliability.Attempts != 1 || len(retried.Errors) != 1 {
		t.Fatalf("retry outcome = %#v, calls = %d", retried, calls.Load())
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	protected := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: runtime.NewMemoryIdempotencyStore(), RequestKey: "key",
	}})
	if calls.Load() != 1 || len(protected.Errors) != 1 || protected.Errors[0].Code != runtime.CodeIdempotencyNotAllowed {
		t.Fatalf("protected outcome = %#v, calls = %d", protected, calls.Load())
	}
}

func TestSharedRetryBudgetBoundsAttemptsAcrossLayers(t *testing.T) {
	t.Parallel()
	attempts := 0
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts++
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	budget := runtime.NewRetryBudget(3)
	options := runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 3, Budget: budget}}
	first := plan.ExecuteWith(context.Background(), options)
	second := plan.ExecuteWith(context.Background(), options)
	if attempts != 3 || first.Reliability.Attempts != 3 || second.Reliability.Attempts != 0 {
		t.Fatalf("handler attempts = %d, first = %#v, second = %#v", attempts, first.Reliability, second.Reliability)
	}
	if len(second.Errors) != 1 || second.Errors[0].Code != runtime.CodeRetryBudgetExhausted {
		t.Fatalf("second errors = %#v", second.Errors)
	}
}

func TestMemoryIdempotencyStoreSharesConcurrentProtectedExecution(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "create-42", Fingerprint: "fingerprint-a",
		Now: now, LeaseDuration: time.Minute,
	}
	owner, err := store.Claim(context.Background(), request)
	if err != nil || owner.State != runtime.IdempotencyRunning || owner.Fence == 0 {
		t.Fatalf("owner claim = %#v, %v", owner, err)
	}
	waiterResult := make(chan runtime.IdempotencyClaim, 1)
	waiterError := make(chan error, 1)
	go func() {
		claim, claimErr := store.Claim(context.Background(), request)
		waiterResult <- claim
		waiterError <- claimErr
	}()
	want := runtime.Outcome{Data: map[string]any{"create": "ok"}, Effects: runtime.EffectApplied}
	if err := store.Complete(context.Background(), runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key,
		Fingerprint: request.Fingerprint, Fence: owner.Fence, Outcome: want,
		Now: now, Retention: time.Hour,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case claim := <-waiterResult:
		if claim.State != runtime.IdempotencyCompleted || claim.Outcome.Data["create"] != "ok" || !claim.Deduplicated {
			t.Fatalf("waiter claim = %#v", claim)
		}
		if err := <-waiterError; err != nil {
			t.Fatalf("waiter error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent waiter did not receive the protected result")
	}
	if !errors.Is(store.Complete(context.Background(), runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key,
		Fingerprint: request.Fingerprint, Fence: owner.Fence - 1, Outcome: want,
		Now: now, Retention: time.Hour,
	}), runtime.ErrStaleIdempotencyLease) {
		t.Fatal("stale lease owner was allowed to overwrite the result")
	}
}

func TestMemoryIdempotencyStoreRejectsMismatchesExpiresResultsAndClonesReplays(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "key", Fingerprint: "one",
		Now: now, LeaseDuration: time.Minute,
	}
	owner, err := store.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	mismatch := request
	mismatch.Fingerprint = "two"
	if _, err := store.Claim(context.Background(), mismatch); !errors.Is(err, runtime.ErrIdempotencyConflict) {
		t.Fatalf("mismatched Claim error = %v", err)
	}
	want := runtime.Outcome{Data: map[string]any{"nested": map[string]any{"value": "original"}}, Effects: runtime.EffectApplied}
	if err := store.Complete(context.Background(), runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key, Fingerprint: request.Fingerprint,
		Fence: owner.Fence, Outcome: want, Now: now, Retention: time.Hour,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want.Data["nested"].(map[string]any)["value"] = "mutated"
	replay, err := store.Claim(context.Background(), request)
	if err != nil || replay.Outcome.Data["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	replay.Outcome.Data["nested"].(map[string]any)["value"] = "mutated-again"
	secondReplay, err := store.Claim(context.Background(), request)
	if err != nil || secondReplay.Outcome.Data["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("second replay = %#v, %v", secondReplay, err)
	}
	expired := request
	expired.Now = now.Add(time.Hour)
	replacement, err := store.Claim(context.Background(), expired)
	if err != nil || replacement.State != runtime.IdempotencyRunning || replacement.Fence == owner.Fence {
		t.Fatalf("replacement = %#v, %v", replacement, err)
	}
}

func TestMemoryIdempotencyStoreExpiredLeaseIsIndeterminateAndProcessLocal(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryIdempotencyStore()
	if store.Durability() != runtime.IdempotencyProcessLocal {
		t.Fatalf("Durability = %q", store.Durability())
	}
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "key", Fingerprint: "one",
		Now: now, LeaseDuration: time.Minute,
	}
	owner, err := store.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	expired := request
	expired.Now = now.Add(time.Minute)
	claim, err := store.Claim(context.Background(), expired)
	if !errors.Is(err, runtime.ErrIdempotencyIndeterminate) || claim.State != runtime.IdempotencyIndeterminate {
		t.Fatalf("expired Claim = %#v, %v", claim, err)
	}
	if !errors.Is(store.Complete(context.Background(), runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key,
		Fingerprint: request.Fingerprint, Fence: owner.Fence, Now: expired.Now, Retention: time.Hour,
	}), runtime.ErrStaleIdempotencyLease) {
		t.Fatal("expired owner completed an indeterminate record")
	}
	restarted := runtime.NewMemoryIdempotencyStore()
	newOwner, err := restarted.Claim(context.Background(), request)
	if err != nil || newOwner.State != runtime.IdempotencyRunning {
		t.Fatalf("process-local restart Claim = %#v, %v", newOwner, err)
	}
}

func TestMemoryIdempotencyStoreWaiterStopsAtLeaseExpiry(t *testing.T) {
	t.Parallel()
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Now()
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant/user", Operation: "write", Key: "key", Fingerprint: "same",
		Now: now, LeaseDuration: 20 * time.Millisecond,
	}
	if claim, err := store.Claim(context.Background(), request); err != nil || claim.State != runtime.IdempotencyRunning {
		t.Fatalf("owner Claim = %#v, %v", claim, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claim, err := store.Claim(ctx, request)
	if !errors.Is(err, runtime.ErrIdempotencyIndeterminate) || claim.State != runtime.IdempotencyIndeterminate || !claim.Deduplicated {
		t.Fatalf("waiter Claim = %#v, %v", claim, err)
	}
}

func TestIdempotencyMetadataIsTypedAndValidated(t *testing.T) {
	t.Parallel()
	for _, policy := range []runtime.IdempotencyPolicy{
		runtime.IdempotencyNonIdempotent,
		runtime.IdempotencyIdempotent,
		runtime.IdempotencyConditional,
	} {
		registry := runtime.NewRegistry(coreTypes(t))
		metadata := completeMetadata(runtime.WriteEffect)
		metadata.Idempotency = policy
		descriptor := runtime.Descriptor{
			Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
		}
		if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) {
			return "ok", nil
		})); err != nil {
			t.Fatalf("Register(%q): %v", policy, err)
		}
	}
	registry := runtime.NewRegistry(coreTypes(t))
	metadata := completeMetadata(runtime.WriteEffect)
	metadata.Idempotency = runtime.IdempotencyPolicy("unknown")
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) {
		return "ok", nil
	})); err == nil {
		t.Fatal("Register accepted unknown idempotency policy")
	}
}

func TestExecuteWithRequestKeyDeduplicatesAndReauthorizesReplay(t *testing.T) {
	t.Parallel()
	var handlerCalls atomic.Int32
	var authorizationCalls atomic.Int32
	var allowed atomic.Bool
	allowed.Store(true)
	plan := reliabilityMutationPlan(t, runtime.IdempotencyConditional,
		runtime.AuthorizerFunc(func(context.Context, runtime.AuthorizationRequest) (runtime.AuthorizationDecision, error) {
			authorizationCalls.Add(1)
			return runtime.AuthorizationDecision{Allowed: allowed.Load()}, nil
		}), func(context.Context) (string, error) {
			handlerCalls.Add(1)
			return "created", nil
		})
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	options := runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: store, RequestKey: "secret-key", Now: func() time.Time { return now },
		LeaseDuration: time.Minute, Retention: time.Hour,
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Tenant: "tenant-a", Subject: "user-a", AuthorizationRevision: "auth-v1",
	})
	first := plan.ExecuteWith(ctx, options)
	second := plan.ExecuteWith(ctx, options)
	if len(first.Errors) != 0 || first.Data["write"] != "created" || first.Reliability.Deduplicated {
		t.Fatalf("first = %#v", first)
	}
	if len(second.Errors) != 0 || second.Data["write"] != "created" || !second.Reliability.Deduplicated {
		t.Fatalf("second = %#v", second)
	}
	if handlerCalls.Load() != 1 || authorizationCalls.Load() != 2 {
		t.Fatalf("handler calls = %d, authorization calls = %d", handlerCalls.Load(), authorizationCalls.Load())
	}
	allowed.Store(false)
	denied := plan.ExecuteWith(ctx, options)
	if len(denied.Errors) != 1 || denied.Errors[0].Code != runtime.CodeUnauthorized || len(denied.Data) != 0 {
		t.Fatalf("denied replay = %#v", denied)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler calls after denied replay = %d", handlerCalls.Load())
	}
}

func TestExecuteIdempotencyFingerprintIncludesAuthorizationRevision(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	plan := reliabilityMutationPlan(t, runtime.IdempotencyIdempotent, nil, func(context.Context) (string, error) {
		calls.Add(1)
		return "created", nil
	})
	store := runtime.NewMemoryIdempotencyStore()
	options := runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: store, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour,
	}}
	firstContext := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Tenant: "tenant-a", Subject: "user-a", AuthorizationRevision: "auth-v1",
	})
	if outcome := plan.ExecuteWith(firstContext, options); len(outcome.Errors) != 0 {
		t.Fatalf("first = %#v", outcome)
	}
	changedContext := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Tenant: "tenant-a", Subject: "user-a", AuthorizationRevision: "auth-v2",
	})
	conflict := plan.ExecuteWith(changedContext, options)
	if len(conflict.Errors) != 1 || conflict.Errors[0].Code != runtime.CodeIdempotencyConflict || calls.Load() != 1 {
		t.Fatalf("conflict = %#v, calls = %d", conflict, calls.Load())
	}
}

func TestExecuteIdempotencyFingerprintIncludesCanonicalTypedVariables(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	registry := runtime.NewRegistry(coreTypes(t))
	metadata := completeMetadata(runtime.WriteEffect)
	metadata.Idempotency = runtime.IdempotencyIdempotent
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.Bind(descriptor, func(_ context.Context, value string) (string, error) {
		calls.Add(1)
		return value, nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	prepare := func(value string) *runtime.Plan {
		request := decodeRuntimeRequest(t, `{"version":"1","variables":{"value":"`+value+`"},"document":{"operations":[{"name":"M","kind":"mutation","variables":[{"name":"value","type":"String","required":true}],"select":[{"$call":{"name":"write","args":{"value":{"$var":"value"}}}}]}]}}`)
		plan, prepareErr := runtime.Prepare(snapshot, request)
		if prepareErr != nil {
			t.Fatalf("Prepare: %v", prepareErr)
		}
		return plan
	}
	store := runtime.NewMemoryIdempotencyStore()
	options := runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: store, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour,
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	if outcome := prepare("first").ExecuteWith(ctx, options); len(outcome.Errors) != 0 {
		t.Fatalf("first = %#v", outcome)
	}
	conflict := prepare("second").ExecuteWith(ctx, options)
	if len(conflict.Errors) != 1 || conflict.Errors[0].Code != runtime.CodeIdempotencyConflict || calls.Load() != 1 {
		t.Fatalf("conflict = %#v, calls = %d", conflict, calls.Load())
	}
}

func TestExecuteIdempotencyFailsClosedOnPolicyAndStoreErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		policy runtime.IdempotencyPolicy
		store  runtime.IdempotencyStore
		code   string
	}{
		{name: "non-idempotent", policy: runtime.IdempotencyNonIdempotent, store: runtime.NewMemoryIdempotencyStore(), code: runtime.CodeIdempotencyNotAllowed},
		{name: "store-outage", policy: runtime.IdempotencyConditional, store: failingIdempotencyStore{}, code: runtime.CodeIdempotencyStoreUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			plan := reliabilityMutationPlan(t, test.policy, nil, func(context.Context) (string, error) {
				calls.Add(1)
				return "created", nil
			})
			ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
			outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
				Store: test.store, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour,
			}})
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != test.code || calls.Load() != 0 || outcome.Effects != runtime.EffectNone {
				t.Fatalf("outcome = %#v, calls = %d", outcome, calls.Load())
			}
		})
	}
}

func TestRetrySchedulingStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts.Add(1)
		cancel()
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Hour}})
	if attempts.Load() != 1 || outcome.Reliability.Attempts != 1 {
		t.Fatalf("attempts = %d, outcome = %#v", attempts.Load(), outcome)
	}
}

func TestRetrySchedulingStopsAtContextDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var attempts atomic.Int32
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts.Add(1)
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Hour}})
	if attempts.Load() != 1 || outcome.Reliability.Attempts != 1 {
		t.Fatalf("attempts = %d, outcome = %#v", attempts.Load(), outcome)
	}
}

func TestMutationRetryRequiresIdempotentOrProtectedConditionalMetadata(t *testing.T) {
	t.Parallel()
	for _, policy := range []runtime.IdempotencyPolicy{runtime.IdempotencyNonIdempotent, runtime.IdempotencyConditional} {
		policy := policy
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			plan := reliabilityMutationPlan(t, policy, nil, func(context.Context) (string, error) {
				attempts.Add(1)
				return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
			})
			outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 3}})
			if attempts.Load() != 1 || outcome.Reliability.Attempts != 1 {
				t.Fatalf("attempts = %d, outcome = %#v", attempts.Load(), outcome)
			}
		})
	}
}

func TestRetryPolicyRespectsHandlerRetryAfter(t *testing.T) {
	t.Parallel()
	attempts := 0
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts++
		if attempts == 1 {
			return "", &runtime.Error{
				Code: "OVERLOADED", Message: "try later", Retryable: true, RetryAfter: 40 * time.Millisecond,
			}
		}
		return "ready", nil
	})
	var delay time.Duration
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{
		MaxAttempts: 2, BaseDelay: 10 * time.Millisecond,
		Sleep: func(_ context.Context, value time.Duration) error { delay = value; return nil },
	}})
	if len(outcome.Errors) != 0 || attempts != 2 || delay != 40*time.Millisecond {
		t.Fatalf("outcome = %#v, attempts = %d, delay = %s", outcome, attempts, delay)
	}
}

func TestIdempotencyTelemetryContainsOnlySafeAttemptAndDedupFields(t *testing.T) {
	t.Parallel()
	plan := reliabilityMutationPlan(t, runtime.IdempotencyIdempotent, nil, func(context.Context) (string, error) {
		return "created", nil
	})
	var events []runtime.ReliabilityEvent
	options := runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: runtime.NewMemoryIdempotencyStore(), RequestKey: "must-not-appear",
		LeaseDuration: time.Minute, Retention: time.Hour,
		Observe: func(event runtime.ReliabilityEvent) { events = append(events, event) },
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	plan.ExecuteWith(ctx, options)
	plan.ExecuteWith(ctx, options)
	if len(events) != 2 || events[0].Deduplicated || !events[1].Deduplicated || events[0].Attempt != 1 || events[1].Attempt != 0 {
		t.Fatalf("events = %#v", events)
	}
}

func TestNamedMutationGroupKeysReplayGroupsIndependently(t *testing.T) {
	t.Parallel()
	var handlerCalls atomic.Int32
	var transactionBegins atomic.Int32
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		transactionBegins.Add(1)
		return ctx, fakeTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	plan := transactionPlan(t, provider, nil,
		`{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"first","select":[{"$call":{"name":"write","as":"a"}}]}},{"$atomic":{"name":"second","select":[{"$call":{"name":"write","as":"b"}}]}}]}]}`,
		func(context.Context) (string, error) {
			handlerCalls.Add(1)
			return "written", nil
		})
	options := runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: runtime.NewMemoryIdempotencyStore(), GroupKeys: map[string]string{"first": "key-a", "second": "key-b"},
		LeaseDuration: time.Minute, Retention: time.Hour,
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	first := plan.ExecuteWith(ctx, options)
	second := plan.ExecuteWith(ctx, options)
	if len(first.Errors) != 0 || len(second.Errors) != 0 || !second.Reliability.Deduplicated {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
	if handlerCalls.Load() != 2 || transactionBegins.Load() != 2 {
		t.Fatalf("handler calls = %d, transaction begins = %d", handlerCalls.Load(), transactionBegins.Load())
	}
	assertJSONEqual(t, second.Data, map[string]any{"a": "written", "b": "written"})
}

func TestNamedMutationGroupRetriesWhileHoldingItsClaim(t *testing.T) {
	t.Parallel()
	var handlerCalls atomic.Int32
	var events []runtime.ReliabilityEvent
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{
			commit:   func(context.Context) (runtime.CommitOutcome, error) { return runtime.CommitApplied, nil },
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	plan := transactionPlan(t, provider, nil,
		`{"operations":[{"name":"M","kind":"mutation","atomicity":"group","select":[{"$atomic":{"name":"only","select":[{"$call":{"name":"write","as":"value"}}]}}]}]}`,
		func(context.Context) (string, error) {
			if handlerCalls.Add(1) == 1 {
				return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
			}
			return "written", nil
		})
	budget := runtime.NewRetryBudget(2)
	options := runtime.ExecuteOptions{
		Retry: runtime.RetryPolicy{MaxAttempts: 2, Budget: budget},
		Idempotency: runtime.IdempotencyOptions{
			Store: runtime.NewMemoryIdempotencyStore(), GroupKeys: map[string]string{"only": "key"},
			LeaseDuration: time.Minute, Retention: time.Hour,
			Observe: func(event runtime.ReliabilityEvent) { events = append(events, event) },
		},
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	outcome := plan.ExecuteWith(ctx, options)
	if len(outcome.Errors) != 0 || outcome.Data["value"] != "written" || outcome.Reliability.Attempts != 2 || handlerCalls.Load() != 2 || budget.Remaining() != 0 {
		t.Fatalf("outcome = %#v, calls = %d, budget = %d", outcome, handlerCalls.Load(), budget.Remaining())
	}
	if len(events) != 2 || events[0].Group != "only" || events[0].Attempt != 2 || events[0].State != runtime.IdempotencyRunning ||
		events[1].Group != "" || events[1].Attempt != 2 || events[1].State != "" {
		t.Fatalf("events = %#v", events)
	}
}

func TestUnknownCommitBecomesIndeterminateAndIsNeverRetried(t *testing.T) {
	t.Parallel()
	var handlerCalls atomic.Int32
	provider := fakeTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				return runtime.CommitUnknown, errors.New("transport interrupted")
			},
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	plan := transactionPlan(t, provider, nil, operationAtomicDocument(), func(context.Context) (string, error) {
		handlerCalls.Add(1)
		return "written", nil
	})
	options := runtime.ExecuteOptions{
		Retry: runtime.RetryPolicy{MaxAttempts: 3},
		Idempotency: runtime.IdempotencyOptions{
			Store: runtime.NewMemoryIdempotencyStore(), RequestKey: "key",
			LeaseDuration: time.Minute, Retention: time.Hour,
		},
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	first := plan.ExecuteWith(ctx, options)
	second := plan.ExecuteWith(ctx, options)
	if handlerCalls.Load() != 1 || first.Effects != runtime.EffectIndeterminate || len(first.Errors) != 1 || first.Errors[0].Code != runtime.CodeTransactionCommitUnknown {
		t.Fatalf("first = %#v, calls = %d", first, handlerCalls.Load())
	}
	if second.Effects != runtime.EffectNone || len(second.Errors) != 1 || second.Errors[0].Code != runtime.CodeIdempotencyIndeterminate {
		t.Fatalf("second = %#v", second)
	}
}

func TestResultPersistenceFailureAfterAppliedEffectIsIndeterminate(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	plan := reliabilityMutationPlan(t, runtime.IdempotencyIdempotent, nil, func(context.Context) (string, error) {
		calls.Add(1)
		return "created", nil
	})
	store := &completionFailingIdempotencyStore{MemoryIdempotencyStore: runtime.NewMemoryIdempotencyStore()}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: store, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour,
	}})
	if calls.Load() != 1 || outcome.Effects != runtime.EffectIndeterminate || len(outcome.Errors) != 1 || outcome.Errors[0].Code != runtime.CodeIdempotencyStoreUnavailable {
		t.Fatalf("outcome = %#v, calls = %d", outcome, calls.Load())
	}
}

type failingIdempotencyStore struct{}

func (failingIdempotencyStore) Claim(context.Context, runtime.IdempotencyClaimRequest) (runtime.IdempotencyClaim, error) {
	return runtime.IdempotencyClaim{}, errors.New("store unavailable")
}

func (failingIdempotencyStore) Complete(context.Context, runtime.IdempotencyCompletion) error {
	return errors.New("store unavailable")
}

func (failingIdempotencyStore) MarkIndeterminate(context.Context, runtime.IdempotencyIndeterminateUpdate) error {
	return errors.New("store unavailable")
}

func (failingIdempotencyStore) Durability() runtime.IdempotencyDurability {
	return runtime.IdempotencyDurable
}

type completionFailingIdempotencyStore struct {
	*runtime.MemoryIdempotencyStore
}

func (*completionFailingIdempotencyStore) Complete(context.Context, runtime.IdempotencyCompletion) error {
	return errors.New("result persistence failed")
}

func reliabilityMutationPlan(t *testing.T, policy runtime.IdempotencyPolicy, authorizer runtime.Authorizer, handler func(context.Context) (string, error)) *runtime.Plan {
	t.Helper()
	registry := runtime.NewRegistry(coreTypes(t))
	if authorizer != nil {
		if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Authorizer: authorizer}); err != nil {
			t.Fatalf("ConfigureAuthorization: %v", err)
		}
	}
	metadata := completeMetadata(runtime.WriteEffect)
	metadata.Idempotency = policy
	descriptor := runtime.Descriptor{
		Name: "write", Scope: runtime.RootScope, Kind: protocol.Mutation, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		return handler(ctx)
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"write"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

func reliabilityQueryPlan(t *testing.T, retrySafe bool, policy runtime.IdempotencyPolicy, handler func(context.Context) (string, error)) *runtime.Plan {
	t.Helper()
	registry := runtime.NewRegistry(coreTypes(t))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.RetrySafe = retrySafe
	metadata.Idempotency = policy
	descriptor := runtime.Descriptor{
		Name: "read", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(ctx context.Context, _ runtime.Invocation) (string, error) {
		return handler(ctx)
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"read"}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}
