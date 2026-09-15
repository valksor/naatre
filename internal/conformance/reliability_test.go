package conformance_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type reliabilityFixture struct {
	Profile      string              `json:"profile"`
	Policies     []string            `json:"policies"`
	States       []string            `json:"states"`
	Durabilities []string            `json:"durabilities"`
	ErrorCodes   []string            `json:"errorCodes"`
	Vectors      []reliabilityVector `json:"vectors"`
}

type reliabilityVector struct {
	Name           string `json:"name"`
	Fault          string `json:"fault"`
	State          string `json:"state"`
	Code           string `json:"code"`
	Attempts       int    `json:"attempts"`
	ExecutesEffect bool   `json:"executesEffect"`
	Deduplicated   bool   `json:"deduplicated"`
}

func TestPortableReliabilityContractVectors(t *testing.T) {
	t.Parallel()
	var fixture reliabilityFixture
	readFixture(t, "reliability.json", &fixture)
	if fixture.Profile != "core.reliability-1" ||
		!slices.Equal(fixture.Policies, []string{"non-idempotent", "idempotent", "conditionally-idempotent"}) ||
		!slices.Equal(fixture.States, []string{"running", "completed", "indeterminate"}) ||
		!slices.Equal(fixture.Durabilities, []string{"process-local", "durable"}) {
		t.Fatal("reliability fixture header is incomplete")
	}
	required := map[string]bool{
		"handler-failure-retries": false, "transport-interruption-is-indeterminate": false,
		"concurrent-waiter-replays": false, "completed-record-expires": false,
		"running-lease-expires-indeterminate": false, "store-outage-fails-closed": false,
		"crash-after-effect-before-result": false, "stale-owner-is-fenced": false,
		"process-local-provider-restart": false, "mismatched-fingerprint-conflicts": false,
		"replay-is-reauthorized": false, "shared-budget-bounds-layers": false,
		"deadline-cancels-scheduling": false, "retry-after-lower-bound": false,
	}
	seen := make(map[string]bool, len(fixture.Vectors))
	for _, vector := range fixture.Vectors {
		if vector.Name == "" || seen[vector.Name] || vector.State == "" || vector.Attempts < 0 {
			t.Fatalf("incomplete or duplicate reliability vector %q", vector.Name)
		}
		seen[vector.Name] = true
		if _, ok := required[vector.Name]; ok {
			required[vector.Name] = true
		}
	}
	for name, present := range required {
		if !present {
			t.Errorf("reliability fixture lacks %q", name)
		}
	}
	for _, code := range fixture.ErrorCodes {
		if code == "" {
			t.Fatal("reliability fixture contains an empty error code")
		}
	}
}

// TestPortableReliabilityContractVectorsAgainstRuntime decodes each reliability
// vector and drives it through the real retry/idempotency machinery in package
// runtime (Prepare + Plan.ExecuteWith and the MemoryIdempotencyStore), asserting
// that the runtime's actual code, attempt count, effect execution, and
// deduplication match the fixture's declared expectation for that fault.
func TestPortableReliabilityContractVectorsAgainstRuntime(t *testing.T) {
	t.Parallel()
	var fixture reliabilityFixture
	readFixture(t, "reliability.json", &fixture)

	// Bind each declared error code to the concrete runtime constant so the
	// fixture vocabulary cannot drift from the runtime it is meant to describe.
	wantCodes := map[string]string{
		"RETRY_BUDGET_EXHAUSTED":        runtime.CodeRetryBudgetExhausted,
		"IDEMPOTENCY_CONFLICT":          runtime.CodeIdempotencyConflict,
		"IDEMPOTENCY_INDETERMINATE":     runtime.CodeIdempotencyIndeterminate,
		"IDEMPOTENCY_NOT_ALLOWED":       runtime.CodeIdempotencyNotAllowed,
		"IDEMPOTENCY_STORE_UNAVAILABLE": runtime.CodeIdempotencyStoreUnavailable,
	}
	for _, code := range fixture.ErrorCodes {
		if runtimeCode, ok := wantCodes[code]; !ok || runtimeCode != code {
			t.Fatalf("reliability error code %q is not a known runtime code", code)
		}
	}

	drivers := map[string]func(*testing.T, reliabilityVector){
		"handler-failure-retries":                 reliabilityDriveHandlerFailureRetries,
		"transport-interruption-is-indeterminate": reliabilityDriveTransportInterruption,
		"concurrent-waiter-replays":               reliabilityDriveConcurrentWaiterReplays,
		"completed-record-expires":                reliabilityDriveCompletedRecordExpires,
		"running-lease-expires-indeterminate":     reliabilityDriveLeaseExpiresIndeterminate,
		"store-outage-fails-closed":               reliabilityDriveStoreOutageFailsClosed,
		"crash-after-effect-before-result":        reliabilityDriveCrashAfterEffect,
		"stale-owner-is-fenced":                   reliabilityDriveStaleOwnerFenced,
		"process-local-provider-restart":          reliabilityDriveProviderRestart,
		"mismatched-fingerprint-conflicts":        reliabilityDriveMismatchedFingerprint,
		"replay-is-reauthorized":                  reliabilityDriveReplayReauthorized,
		"shared-budget-bounds-layers":             reliabilityDriveSharedBudget,
		"deadline-cancels-scheduling":             reliabilityDriveDeadlineCancels,
		"retry-after-lower-bound":                 reliabilityDriveRetryAfterLowerBound,
	}

	executed := 0
	for _, vector := range fixture.Vectors {
		driver, ok := drivers[vector.Name]
		if !ok {
			continue
		}
		executed++
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			driver(t, vector)
		})
	}
	if executed < 14 {
		t.Fatalf("drove only %d reliability vectors against the runtime, want at least 14", executed)
	}
}

func reliabilityDriveHandlerFailureRetries(t *testing.T, vector reliabilityVector) {
	var attempts atomic.Int64
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
		}
		return "ready", nil
	})
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{
		MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil },
	}})
	if len(outcome.Errors) != 0 || outcome.Data["read"] != "ready" {
		t.Fatalf("retry outcome = %#v", outcome)
	}
	if int(outcome.Reliability.Attempts) != vector.Attempts || outcome.Reliability.Deduplicated != vector.Deduplicated {
		t.Fatalf("attempts = %d dedup = %v, want %d/%v", outcome.Reliability.Attempts, outcome.Reliability.Deduplicated, vector.Attempts, vector.Deduplicated)
	}
	if vector.ExecutesEffect && attempts.Load() == 0 {
		t.Fatal("handler did not execute the effect")
	}
}

func reliabilityDriveTransportInterruption(t *testing.T, vector reliabilityVector) {
	var calls atomic.Int64
	provider := fakeConformanceTransactionProvider{begin: func(ctx context.Context, _ runtime.TransactionRequest) (context.Context, runtime.Transaction, error) {
		return ctx, fakeConformanceTransaction{
			commit: func(context.Context) (runtime.CommitOutcome, error) {
				return runtime.CommitUnknown, errors.New("transport interrupted")
			},
			rollback: func(context.Context) error { return nil },
		}, nil
	}}
	plan := mutationExecPlan(t, provider, nil, mutationOperationDocument(), func(context.Context) (string, error) {
		calls.Add(1)
		return "written", nil
	})
	store := runtime.NewMemoryIdempotencyStore()
	options := runtime.ExecuteOptions{
		Retry:       runtime.RetryPolicy{MaxAttempts: 3},
		Idempotency: runtime.IdempotencyOptions{Store: store, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour},
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	first := plan.ExecuteWith(ctx, options)
	if first.Effects != runtime.EffectIndeterminate || calls.Load() != int64(vector.Attempts) || !vector.ExecutesEffect {
		t.Fatalf("first = %#v calls = %d", first, calls.Load())
	}
	// The persisted indeterminate record makes every subsequent claim resolve to
	// the fixture's declared IDEMPOTENCY_INDETERMINATE code.
	second := plan.ExecuteWith(ctx, options)
	if len(second.Errors) != 1 || second.Errors[0].Code != vector.Code {
		t.Fatalf("replay = %#v, want code %q", second, vector.Code)
	}
}

func reliabilityDriveConcurrentWaiterReplays(t *testing.T, vector reliabilityVector) {
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "create-42", Fingerprint: "fingerprint-a",
		Now: now, LeaseDuration: time.Minute,
	}
	owner, err := store.Claim(context.Background(), request)
	if err != nil || owner.State != runtime.IdempotencyRunning {
		t.Fatalf("owner claim = %#v, %v", owner, err)
	}
	result := make(chan runtime.IdempotencyClaim, 1)
	go func() {
		claim, _ := store.Claim(context.Background(), request)
		result <- claim
	}()
	if err := store.Complete(context.Background(), runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key, Fingerprint: request.Fingerprint,
		Fence: owner.Fence, Outcome: runtime.Outcome{Data: map[string]any{"create": "ok"}, Effects: runtime.EffectApplied},
		Now: now, Retention: time.Hour,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case claim := <-result:
		if string(reliabilityIdempotencyState(claim.State)) != vector.State || claim.Deduplicated != vector.Deduplicated {
			t.Fatalf("waiter claim state = %q dedup = %v, want %q/%v", claim.State, claim.Deduplicated, vector.State, vector.Deduplicated)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent waiter did not receive the protected result")
	}
}

func reliabilityDriveCompletedRecordExpires(t *testing.T, vector reliabilityVector) {
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "key", Fingerprint: "one", Now: now, LeaseDuration: time.Minute,
	}
	owner, err := store.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Complete(context.Background(), runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key, Fingerprint: request.Fingerprint,
		Fence: owner.Fence, Outcome: runtime.Outcome{Data: map[string]any{"create": "ok"}}, Now: now, Retention: time.Hour,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	expired := request
	expired.Now = now.Add(time.Hour + time.Minute)
	replacement, err := store.Claim(context.Background(), expired)
	if err != nil || string(reliabilityIdempotencyState(replacement.State)) != vector.State || replacement.Deduplicated != vector.Deduplicated {
		t.Fatalf("post-expiry claim = %#v, %v; want state %q dedup %v", replacement, err, vector.State, vector.Deduplicated)
	}
}

func reliabilityDriveLeaseExpiresIndeterminate(t *testing.T, vector reliabilityVector) {
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Now()
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant/user", Operation: "write", Key: "key", Fingerprint: "same", Now: now, LeaseDuration: 20 * time.Millisecond,
	}
	if claim, err := store.Claim(context.Background(), request); err != nil || claim.State != runtime.IdempotencyRunning {
		t.Fatalf("owner Claim = %#v, %v", claim, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claim, err := store.Claim(ctx, request)
	if !errors.Is(err, runtime.ErrIdempotencyIndeterminate) || string(reliabilityIdempotencyState(claim.State)) != vector.State || claim.Deduplicated != vector.Deduplicated {
		t.Fatalf("waiter Claim = %#v, %v; want state %q dedup %v", claim, err, vector.State, vector.Deduplicated)
	}
	if reliabilityErrorCode(err) != vector.Code {
		t.Fatalf("lease expiry code = %q, want %q", reliabilityErrorCode(err), vector.Code)
	}
}

func reliabilityDriveStoreOutageFailsClosed(t *testing.T, vector reliabilityVector) {
	var calls atomic.Int64
	plan := reliabilityMutationPlan(t, runtime.IdempotencyConditional, nil, func(context.Context) (string, error) {
		calls.Add(1)
		return "created", nil
	})
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: failingConformanceIdempotencyStore{}, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour,
	}})
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != vector.Code || calls.Load() != int64(vector.Attempts) ||
		outcome.Effects != runtime.EffectNone || vector.ExecutesEffect {
		t.Fatalf("store outage outcome = %#v calls = %d, want code %q", outcome, calls.Load(), vector.Code)
	}
}

func reliabilityDriveCrashAfterEffect(t *testing.T, vector reliabilityVector) {
	var calls atomic.Int64
	plan := reliabilityMutationPlan(t, runtime.IdempotencyIdempotent, nil, func(context.Context) (string, error) {
		calls.Add(1)
		return "created", nil
	})
	store := &completionFailingConformanceIdempotencyStore{MemoryIdempotencyStore: runtime.NewMemoryIdempotencyStore()}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a"})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Idempotency: runtime.IdempotencyOptions{
		Store: store, RequestKey: "key", LeaseDuration: time.Minute, Retention: time.Hour,
	}})
	if calls.Load() != int64(vector.Attempts) || !vector.ExecutesEffect || outcome.Effects != runtime.EffectIndeterminate ||
		len(outcome.Errors) != 1 || outcome.Errors[0].Code != vector.Code {
		t.Fatalf("crash-after-effect outcome = %#v calls = %d, want code %q", outcome, calls.Load(), vector.Code)
	}
}

func reliabilityDriveStaleOwnerFenced(t *testing.T, vector reliabilityVector) {
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "key", Fingerprint: "one", Now: now, LeaseDuration: time.Minute,
	}
	owner, err := store.Claim(context.Background(), request)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	completion := runtime.IdempotencyCompletion{
		Scope: request.Scope, Operation: request.Operation, Key: request.Key, Fingerprint: request.Fingerprint,
		Fence: owner.Fence, Outcome: runtime.Outcome{Data: map[string]any{"create": "ok"}, Effects: runtime.EffectApplied},
		Now: now, Retention: time.Hour,
	}
	if err := store.Complete(context.Background(), completion); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stale := completion
	stale.Fence = owner.Fence - 1
	if !errors.Is(store.Complete(context.Background(), stale), runtime.ErrStaleIdempotencyLease) {
		t.Fatal("stale-fenced owner was allowed to overwrite the completed result")
	}
	replay, err := store.Claim(context.Background(), request)
	if err != nil || string(reliabilityIdempotencyState(replay.State)) != vector.State || replay.Deduplicated != vector.Deduplicated {
		t.Fatalf("post-fence replay = %#v, %v; want state %q dedup %v", replay, err, vector.State, vector.Deduplicated)
	}
}

func reliabilityDriveProviderRestart(t *testing.T, vector reliabilityVector) {
	store := runtime.NewMemoryIdempotencyStore()
	if store.Durability() != runtime.IdempotencyProcessLocal {
		t.Fatalf("durability = %q, want process-local", store.Durability())
	}
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "key", Fingerprint: "one", Now: now, LeaseDuration: time.Minute,
	}
	if _, err := store.Claim(context.Background(), request); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A restarted process-local provider forgets the record, so the same key
	// claims fresh (running) rather than deduplicating against the lost owner.
	restarted := runtime.NewMemoryIdempotencyStore()
	claim, err := restarted.Claim(context.Background(), request)
	if err != nil || string(reliabilityIdempotencyState(claim.State)) != vector.State || claim.Deduplicated != vector.Deduplicated {
		t.Fatalf("restart claim = %#v, %v; want state %q dedup %v", claim, err, vector.State, vector.Deduplicated)
	}
}

func reliabilityDriveMismatchedFingerprint(t *testing.T, vector reliabilityVector) {
	store := runtime.NewMemoryIdempotencyStore()
	now := time.Unix(1_800_000_000, 0)
	request := runtime.IdempotencyClaimRequest{
		Scope: "tenant-a:user-a", Operation: "Create", Key: "key", Fingerprint: "one", Now: now, LeaseDuration: time.Minute,
	}
	if _, err := store.Claim(context.Background(), request); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	mismatch := request
	mismatch.Fingerprint = "two"
	_, err := store.Claim(context.Background(), mismatch)
	if !errors.Is(err, runtime.ErrIdempotencyConflict) || reliabilityErrorCode(err) != vector.Code {
		t.Fatalf("mismatched Claim error = %v, want code %q", err, vector.Code)
	}
}

func reliabilityDriveReplayReauthorized(t *testing.T, vector reliabilityVector) {
	var handlerCalls, authorizationCalls atomic.Int32
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
		Store: store, RequestKey: "secret-key", Now: func() time.Time { return now }, LeaseDuration: time.Minute, Retention: time.Hour,
	}}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", Subject: "user-a", AuthorizationRevision: "auth-v1"})
	if first := plan.ExecuteWith(ctx, options); len(first.Errors) != 0 {
		t.Fatalf("first = %#v", first)
	}
	allowed.Store(false)
	denied := plan.ExecuteWith(ctx, options)
	if len(denied.Errors) != 1 || denied.Errors[0].Code != vector.Code || handlerCalls.Load() != 1 {
		t.Fatalf("reauthorized replay = %#v handler calls = %d, want code %q", denied, handlerCalls.Load(), vector.Code)
	}
	// The reauthorized replay is deduplicated (the handler is not re-run) even
	// though it is denied, matching the fixture's deduplicated=true.
	if !vector.Deduplicated || handlerCalls.Load() != 1 {
		t.Fatalf("replay dedup expectation = %v, handler calls = %d", vector.Deduplicated, handlerCalls.Load())
	}
}

func reliabilityDriveSharedBudget(t *testing.T, vector reliabilityVector) {
	var attempts atomic.Int64
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts.Add(1)
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	budget := runtime.NewRetryBudget(3)
	options := runtime.ExecuteOptions{Retry: runtime.RetryPolicy{MaxAttempts: 3, Budget: budget,
		Sleep: func(context.Context, time.Duration) error { return nil }}}
	first := plan.ExecuteWith(context.Background(), options)
	second := plan.ExecuteWith(context.Background(), options)
	if int(first.Reliability.Attempts) != vector.Attempts {
		t.Fatalf("first attempts = %d, want %d", first.Reliability.Attempts, vector.Attempts)
	}
	if len(second.Errors) != 1 || second.Errors[0].Code != vector.Code || second.Reliability.Attempts != 0 {
		t.Fatalf("budget-bounded second = %#v, want code %q", second, vector.Code)
	}
}

func reliabilityDriveDeadlineCancels(t *testing.T, vector reliabilityVector) {
	deadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var attempts atomic.Int64
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		attempts.Add(1)
		return "", &runtime.Error{Code: "TEMPORARY", Message: "try again", Retryable: true}
	})
	outcome := plan.ExecuteWith(ctx, runtime.ExecuteOptions{Retry: runtime.RetryPolicy{
		MaxAttempts: 3, BaseDelay: time.Hour, Now: func() time.Time { return deadline },
	}})
	if int(attempts.Load()) != vector.Attempts || int(outcome.Reliability.Attempts) != vector.Attempts {
		t.Fatalf("deadline attempts = %d/%d, want %d", attempts.Load(), outcome.Reliability.Attempts, vector.Attempts)
	}
}

func reliabilityDriveRetryAfterLowerBound(t *testing.T, vector reliabilityVector) {
	var attempts atomic.Int64
	plan := reliabilityQueryPlan(t, true, runtime.IdempotencyIdempotent, func(context.Context) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &runtime.Error{Code: "BACKEND_OVERLOADED", Message: "try later", Retryable: true, RetryAfter: 40 * time.Millisecond}
		}
		return "ready", nil
	})
	var delay time.Duration
	outcome := plan.ExecuteWith(context.Background(), runtime.ExecuteOptions{Retry: runtime.RetryPolicy{
		MaxAttempts: 2, BaseDelay: 10 * time.Millisecond,
		Sleep: func(_ context.Context, value time.Duration) error { delay = value; return nil },
	}})
	if len(outcome.Errors) != 0 || int(outcome.Reliability.Attempts) != vector.Attempts || delay < 40*time.Millisecond {
		t.Fatalf("retry-after outcome = %#v attempts = %d delay = %s, want %d attempts and >= 40ms", outcome, outcome.Reliability.Attempts, delay, vector.Attempts)
	}
}

// reliabilityIdempotencyState maps a runtime idempotency state to the fixture's
// declared reliability state vocabulary.
func reliabilityIdempotencyState(state runtime.IdempotencyState) string {
	switch state {
	case runtime.IdempotencyRunning:
		return "running"
	case runtime.IdempotencyCompleted:
		return "completed"
	case runtime.IdempotencyIndeterminate:
		return "indeterminate"
	default:
		return string(state)
	}
}

func reliabilityErrorCode(err error) string {
	switch {
	case errors.Is(err, runtime.ErrIdempotencyConflict):
		return runtime.CodeIdempotencyConflict
	case errors.Is(err, runtime.ErrIdempotencyIndeterminate):
		return runtime.CodeIdempotencyIndeterminate
	default:
		return ""
	}
}

// reliabilityQueryPlan freezes a retry-safe/idempotent read query and prepares a
// plan whose single "read" call is driven by handler.
func reliabilityQueryPlan(t *testing.T, retrySafe bool, policy runtime.IdempotencyPolicy, handler func(context.Context) (string, error)) *runtime.Plan {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCoreTypes(t))
	metadata := conformanceMetadata(runtime.ReadEffect)
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
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"read"}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

// reliabilityMutationPlan freezes a transactional "write" mutation with the given
// idempotency policy and optional authorizer, then prepares its plan.
func reliabilityMutationPlan(t *testing.T, policy runtime.IdempotencyPolicy, authorizer runtime.Authorizer, handler func(context.Context) (string, error)) *runtime.Plan {
	t.Helper()
	registry := runtime.NewRegistry(conformanceCoreTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault, Authorizer: authorizer}); err != nil {
		t.Fatalf("ConfigureAuthorization: %v", err)
	}
	metadata := conformanceMetadata(runtime.WriteEffect)
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
	plan, err := runtime.Prepare(snapshot, decodeConformanceRequest(t, `{"version":"1","document":{"operations":[{"name":"M","kind":"mutation","select":[{"$call":{"name":"write"}}]}]}}`))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

type failingConformanceIdempotencyStore struct{}

func (failingConformanceIdempotencyStore) Claim(context.Context, runtime.IdempotencyClaimRequest) (runtime.IdempotencyClaim, error) {
	return runtime.IdempotencyClaim{}, errors.New("store unavailable")
}

func (failingConformanceIdempotencyStore) Complete(context.Context, runtime.IdempotencyCompletion) error {
	return errors.New("store unavailable")
}

func (failingConformanceIdempotencyStore) MarkIndeterminate(context.Context, runtime.IdempotencyIndeterminateUpdate) error {
	return errors.New("store unavailable")
}

func (failingConformanceIdempotencyStore) Durability() runtime.IdempotencyDurability {
	return runtime.IdempotencyDurable
}

type completionFailingConformanceIdempotencyStore struct {
	*runtime.MemoryIdempotencyStore
}

func (*completionFailingConformanceIdempotencyStore) Complete(context.Context, runtime.IdempotencyCompletion) error {
	return errors.New("result persistence failed")
}
