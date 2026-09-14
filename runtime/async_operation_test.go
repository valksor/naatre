package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncOperationStateTransitionsAreMonotonic(t *testing.T) {
	t.Parallel()

	allowed := map[AsyncOperationState][]AsyncOperationState{
		AsyncOperationPending:    {AsyncOperationRunning, AsyncOperationCancelled},
		AsyncOperationRunning:    {AsyncOperationSucceeded, AsyncOperationFailed, AsyncOperationCancelling, AsyncOperationIndeterminate},
		AsyncOperationCancelling: {AsyncOperationSucceeded, AsyncOperationFailed, AsyncOperationCancelled, AsyncOperationIndeterminate},
	}
	states := []AsyncOperationState{
		AsyncOperationPending,
		AsyncOperationRunning,
		AsyncOperationSucceeded,
		AsyncOperationFailed,
		AsyncOperationCancelling,
		AsyncOperationCancelled,
		AsyncOperationIndeterminate,
	}
	for _, from := range states {
		for _, to := range states {
			want := false
			for _, candidate := range allowed[from] {
				want = want || candidate == to
			}
			if got := ValidAsyncOperationTransition(from, to); got != want {
				t.Errorf("transition %s -> %s = %t, want %t", from, to, got, want)
			}
		}
	}
}

func TestAsyncOperationTerminalStatesCannotRegress(t *testing.T) {
	t.Parallel()
	for _, state := range []AsyncOperationState{
		AsyncOperationSucceeded,
		AsyncOperationFailed,
		AsyncOperationCancelled,
		AsyncOperationIndeterminate,
	} {
		if !state.Terminal() {
			t.Errorf("%s is not terminal", state)
		}
		if ValidAsyncOperationTransition(state, AsyncOperationRunning) {
			t.Errorf("terminal state %s regressed to running", state)
		}
	}
}

func TestAsyncOperationAcceptanceSurvivesDisconnectAndDeduplicates(t *testing.T) {
	t.Parallel()
	store := newAsyncTestStore()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	coordinator := newAsyncTestCoordinator(t, store, &now, true, allowAsyncAuthorization)
	ctx, cancel := context.WithCancel(WithPrincipal(context.Background(), Principal{
		Subject: "user-1", Tenant: "tenant-1", AuthorizationRevision: "auth-1",
	}))

	accepted, err := coordinator.Submit(ctx, AsyncOperationSubmission{
		Operation: "document-digest:operation-name", SchemaRevision: "schema-1",
		IdempotencyKey: "key-1", Fingerprint: "fingerprint-1", RequireIdempotency: true,
		Payload: json.RawMessage(`{"input":1}`),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	cancel()
	if !accepted.Created || accepted.Handle.State != AsyncOperationPending || accepted.Handle.Revision != 1 {
		t.Fatalf("accepted = %#v", accepted)
	}
	if accepted.HTTP.Status != http.StatusAccepted || accepted.HTTP.Location != "/v1/operations/op-1" || accepted.HTTP.RetryAfter != 2*time.Second || accepted.HTTP.ETag == "" {
		t.Fatalf("acceptance HTTP metadata = %#v", accepted.HTTP)
	}

	duplicate, err := coordinator.Submit(WithPrincipal(context.Background(), Principal{
		Subject: "user-1", Tenant: "tenant-1", AuthorizationRevision: "auth-1",
	}), AsyncOperationSubmission{
		Operation: "document-digest:operation-name", SchemaRevision: "schema-1",
		IdempotencyKey: "key-1", Fingerprint: "fingerprint-1", RequireIdempotency: true,
		Payload: json.RawMessage(`{"input":1}`),
	})
	if err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	if duplicate.Created || duplicate.Handle.ID != accepted.Handle.ID {
		t.Fatalf("duplicate acceptance = %#v, original = %#v", duplicate, accepted)
	}

	_, err = coordinator.Submit(WithPrincipal(context.Background(), Principal{
		Subject: "user-1", Tenant: "tenant-1", AuthorizationRevision: "auth-1",
	}), AsyncOperationSubmission{
		Operation: "document-digest:operation-name", SchemaRevision: "schema-1",
		IdempotencyKey: "key-1", Fingerprint: "different", RequireIdempotency: true,
	})
	if !errors.Is(err, ErrAsyncOperationIdempotencyConflict) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}

	// A new coordinator using the same durable store can recover accepted work
	// that was never dispatched by the accepting process.
	restarted := newAsyncTestCoordinator(t, store, &now, true, allowAsyncAuthorization)
	var executions atomic.Int32
	recovered, err := restarted.Recover(context.Background(), 10, AsyncOperationWorkerFunc(func(_ context.Context, work AsyncOperationWork, _ AsyncOperationControl) AsyncOperationCompletion {
		executions.Add(1)
		if string(work.Payload) != `{"input":1}` {
			t.Errorf("worker payload = %s", work.Payload)
		}
		return AsyncOperationCompletion{State: AsyncOperationSucceeded, Result: &Outcome{Data: map[string]any{"ok": true}}}
	}))
	if err != nil {
		t.Fatalf("recover pending operations: %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID != accepted.Handle.ID || recovered[0].State != AsyncOperationSucceeded || executions.Load() != 1 {
		t.Fatalf("recovered handles = %#v, executions = %d", recovered, executions.Load())
	}
	if _, err := restarted.Run(context.Background(), accepted.Handle.ID, AsyncOperationWorkerFunc(func(context.Context, AsyncOperationWork, AsyncOperationControl) AsyncOperationCompletion {
		executions.Add(1)
		return AsyncOperationCompletion{State: AsyncOperationFailed}
	})); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("duplicate delivery executed worker %d times", executions.Load())
	}
}

func TestAsyncOperationPollResumeAndAuthorizationShareOneState(t *testing.T) {
	t.Parallel()
	store := newAsyncTestStore()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	var authorizationCalls atomic.Int32
	authorizer := AsyncOperationAuthorizerFunc(func(_ context.Context, request AsyncOperationAuthorizationRequest) (bool, error) {
		authorizationCalls.Add(1)
		return request.Principal.AuthorizationRevision == "auth-current", nil
	})
	coordinator := newAsyncTestCoordinator(t, store, &now, true, authorizer)
	owner := WithPrincipal(context.Background(), Principal{Subject: "owner", Tenant: "tenant", AuthorizationRevision: "auth-current"})
	accepted, err := coordinator.Submit(owner, AsyncOperationSubmission{Operation: "op", SchemaRevision: "schema", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	other := WithPrincipal(context.Background(), Principal{Subject: "other", Tenant: "tenant", AuthorizationRevision: "auth-current"})
	if _, err := coordinator.Poll(other, accepted.Handle.ID, ""); !errors.Is(err, ErrAsyncOperationUnavailable) {
		t.Fatalf("cross-principal poll error = %v", err)
	}
	staleAuthorization := WithPrincipal(context.Background(), Principal{Subject: "owner", Tenant: "tenant", AuthorizationRevision: "auth-old"})
	if _, err := coordinator.Poll(staleAuthorization, accepted.Handle.ID, ""); !errors.Is(err, ErrAsyncOperationUnavailable) {
		t.Fatalf("stale authorization poll error = %v", err)
	}

	poll, err := coordinator.Poll(owner, accepted.Handle.ID, "")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	conditional, err := coordinator.Poll(owner, accepted.Handle.ID, poll.HTTP.ETag)
	if err != nil {
		t.Fatalf("conditional poll: %v", err)
	}
	if conditional.HTTP.Status != http.StatusNotModified || !conditional.NotModified || conditional.Handle.ID != "" {
		t.Fatalf("conditional poll = %#v", conditional)
	}

	resumed := make(chan AsyncOperationPoll, 1)
	resumeErrors := make(chan error, 1)
	go func() {
		result, resumeErr := coordinator.Resume(owner, accepted.Handle.ID, accepted.Handle.Revision)
		if resumeErr != nil {
			resumeErrors <- resumeErr
			return
		}
		resumed <- result
	}()
	store.transitionForTest(t, accepted.Handle.ID, AsyncOperationRunning, now.Add(time.Second))
	select {
	case resumeErr := <-resumeErrors:
		t.Fatalf("resume: %v", resumeErr)
	case result := <-resumed:
		if result.Handle.State != AsyncOperationRunning || result.Handle.Revision != 2 {
			t.Fatalf("resumed state = %#v", result.Handle)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not observe state change")
	}
	if authorizationCalls.Load() < 4 {
		t.Fatalf("authorization calls = %d, want poll and both resume boundaries", authorizationCalls.Load())
	}
}

func TestAsyncOperationCancellationOutcomesAndWorkerRaces(t *testing.T) {
	t.Parallel()
	store := newAsyncTestStore()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	owner := WithPrincipal(context.Background(), Principal{Subject: "owner", Tenant: "tenant", AuthorizationRevision: "auth"})
	unsupported := newAsyncTestCoordinator(t, store, &now, false, allowAsyncAuthorization)
	first, err := unsupported.Submit(owner, AsyncOperationSubmission{Operation: "unsupported", SchemaRevision: "schema"})
	if err != nil {
		t.Fatalf("submit unsupported: %v", err)
	}
	cancellation, err := unsupported.Cancel(owner, first.Handle.ID)
	if err != nil || cancellation.Disposition != AsyncCancellationUnsupported || cancellation.Handle.State != AsyncOperationPending {
		t.Fatalf("unsupported cancellation = %#v, %v", cancellation, err)
	}

	coordinator := newAsyncTestCoordinator(t, store, &now, true, allowAsyncAuthorization)
	pending, err := coordinator.Submit(owner, AsyncOperationSubmission{Operation: "pending", SchemaRevision: "schema"})
	if err != nil {
		t.Fatalf("submit pending: %v", err)
	}
	cancellation, err = coordinator.Cancel(owner, pending.Handle.ID)
	if err != nil || cancellation.Disposition != AsyncCancellationEffective || cancellation.Handle.State != AsyncOperationCancelled {
		t.Fatalf("effective cancellation = %#v, %v", cancellation, err)
	}
	tooLate, err := coordinator.Cancel(owner, pending.Handle.ID)
	if err != nil || tooLate.Disposition != AsyncCancellationTooLate {
		t.Fatalf("too-late cancellation = %#v, %v", tooLate, err)
	}

	running, err := coordinator.Submit(owner, AsyncOperationSubmission{Operation: "running", SchemaRevision: "schema"})
	if err != nil {
		t.Fatalf("submit running: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	runResult := make(chan AsyncOperationHandle, 1)
	runErrors := make(chan error, 1)
	var executions atomic.Int32
	go func() {
		handle, runErr := coordinator.Run(context.Background(), running.Handle.ID, AsyncOperationWorkerFunc(func(ctx context.Context, _ AsyncOperationWork, control AsyncOperationControl) AsyncOperationCompletion {
			executions.Add(1)
			close(started)
			<-release
			requested, checkErr := control.CancellationRequested(ctx)
			if checkErr != nil {
				t.Errorf("check cancellation: %v", checkErr)
			}
			if !requested {
				t.Error("worker did not observe requested cancellation")
			}
			return AsyncOperationCompletion{State: AsyncOperationCancelled}
		}))
		if runErr != nil {
			runErrors <- runErr
			return
		}
		runResult <- handle
	}()
	<-started
	if _, err := coordinator.Run(context.Background(), running.Handle.ID, AsyncOperationWorkerFunc(func(context.Context, AsyncOperationWork, AsyncOperationControl) AsyncOperationCompletion {
		executions.Add(1)
		return AsyncOperationCompletion{State: AsyncOperationSucceeded}
	})); !errors.Is(err, ErrAsyncOperationBusy) {
		t.Fatalf("duplicate worker error = %v", err)
	}
	cancellation, err = coordinator.Cancel(owner, running.Handle.ID)
	if err != nil || cancellation.Disposition != AsyncCancellationRequested || cancellation.Handle.State != AsyncOperationCancelling {
		t.Fatalf("requested cancellation = %#v, %v", cancellation, err)
	}
	close(release)
	select {
	case runErr := <-runErrors:
		t.Fatalf("run cancelled worker: %v", runErr)
	case handle := <-runResult:
		if handle.State != AsyncOperationCancelled {
			t.Fatalf("cancelled worker state = %s", handle.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled worker did not finish")
	}
	if executions.Load() != 1 {
		t.Fatalf("workers executed = %d", executions.Load())
	}
}

func TestAsyncOperationCrashProgressProjectionAndExpiry(t *testing.T) {
	t.Parallel()
	store := newAsyncTestStore()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	coordinator := newAsyncTestCoordinator(t, store, &now, true, allowAsyncAuthorization)
	owner := WithPrincipal(context.Background(), Principal{Subject: "owner", Tenant: "tenant", AuthorizationRevision: "auth"})

	crashing, err := coordinator.Submit(owner, AsyncOperationSubmission{Operation: "crash", SchemaRevision: "schema"})
	if err != nil {
		t.Fatalf("submit crash: %v", err)
	}
	handle, err := coordinator.Run(context.Background(), crashing.Handle.ID, AsyncOperationWorkerFunc(func(context.Context, AsyncOperationWork, AsyncOperationControl) AsyncOperationCompletion {
		panic("worker crash")
	}))
	if !errors.Is(err, ErrAsyncOperationWorkerCrash) || handle.State != AsyncOperationIndeterminate {
		t.Fatalf("crash result = %#v, %v", handle, err)
	}
	late, err := coordinator.Run(context.Background(), crashing.Handle.ID, AsyncOperationWorkerFunc(func(context.Context, AsyncOperationWork, AsyncOperationControl) AsyncOperationCompletion {
		t.Fatal("late worker executed after indeterminate terminal")
		return AsyncOperationCompletion{State: AsyncOperationSucceeded}
	}))
	if err != nil || late.State != AsyncOperationIndeterminate {
		t.Fatalf("late completion = %#v, %v", late, err)
	}

	projected, err := coordinator.Submit(owner, AsyncOperationSubmission{Operation: "project", SchemaRevision: "schema"})
	if err != nil {
		t.Fatalf("submit projection: %v", err)
	}
	handle, err = coordinator.Run(context.Background(), projected.Handle.ID, AsyncOperationWorkerFunc(func(ctx context.Context, _ AsyncOperationWork, control AsyncOperationControl) AsyncOperationCompletion {
		if _, progressErr := control.UpdateProgress(ctx, json.RawMessage(`{"step":1}`)); progressErr != nil {
			t.Errorf("valid progress: %v", progressErr)
		}
		if _, progressErr := control.UpdateProgress(ctx, json.RawMessage(`{"payload":"this exceeds the configured byte boundary"}`)); !errors.Is(progressErr, ErrAsyncOperationProgressInvalid) {
			t.Errorf("oversized progress error = %v", progressErr)
		}
		return AsyncOperationCompletion{
			State:            AsyncOperationSucceeded,
			Result:           &Outcome{Data: map[string]any{"stored": true}},
			ProjectionErrors: []ExecutionError{{Code: CodeOutputCompletion, Message: "projection unavailable"}},
		}
	}))
	if err != nil {
		t.Fatalf("run projected operation: %v", err)
	}
	if handle.State != AsyncOperationSucceeded || handle.Result == nil || len(handle.ProjectionErrors) != 1 || len(handle.Errors) != 0 || handle.Progress.Sequence != 1 {
		t.Fatalf("projected completion = %#v", handle)
	}

	now = handle.ExpiresAt
	if _, err := coordinator.Poll(owner, handle.ID, ""); !errors.Is(err, ErrAsyncOperationUnavailable) {
		t.Fatalf("expired poll error = %v", err)
	}
	removed, err := coordinator.GarbageCollect(context.Background(), 10)
	if err != nil || removed == 0 {
		t.Fatalf("garbage collect = %d, %v", removed, err)
	}
}

func TestAsyncOperationProgressValidationRejectsTrailingAndUnboundedJSON(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
		bytes   int
		tokens  int
	}{
		{name: "empty", payload: "", bytes: 32, tokens: 8},
		{name: "malformed", payload: "{", bytes: 32, tokens: 8},
		{name: "trailing", payload: "{} {}", bytes: 32, tokens: 8},
		{name: "bytes", payload: `{"value":true}`, bytes: 4, tokens: 8},
		{name: "tokens", payload: `[1,2,3]`, bytes: 32, tokens: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateAsyncProgress(json.RawMessage(test.payload), test.bytes, test.tokens); !errors.Is(err, ErrAsyncOperationProgressInvalid) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	if err := validateAsyncProgress(json.RawMessage(`{"value":true}`), 32, 8); err != nil {
		t.Fatalf("valid progress: %v", err)
	}
}

func TestAsyncOperationCoordinatorRequiresDurabilityAndOptionalSubscription(t *testing.T) {
	t.Parallel()
	store := newAsyncTestStore()
	config := AsyncOperationConfig{
		Store: asyncProcessLocalStore{asyncTestStore: store}, Authorizer: allowAsyncAuthorization,
		Now: time.Now, NewID: func() (string, error) { return "id", nil }, Location: func(string) string { return "/id" },
		Retention: time.Minute, RetryAfter: time.Second, MaxProgressBytes: 32, MaxProgressTokens: 8,
	}
	if _, err := NewAsyncOperationCoordinator(config); err == nil {
		t.Fatal("process-local store was accepted")
	}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	withoutSubscription := asyncNoChangeStore{asyncTestStore: store}
	coordinator := newAsyncTestCoordinator(t, withoutSubscription, &now, true, allowAsyncAuthorization)
	owner := WithPrincipal(context.Background(), Principal{Subject: "owner", Tenant: "tenant"})
	accepted, err := coordinator.Submit(owner, AsyncOperationSubmission{Operation: "op", SchemaRevision: "schema"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := coordinator.Resume(owner, accepted.Handle.ID, accepted.Handle.Revision); !errors.Is(err, ErrAsyncOperationSubscriptionUnsupported) {
		t.Fatalf("resume error = %v", err)
	}
}

var allowAsyncAuthorization = AsyncOperationAuthorizerFunc(func(context.Context, AsyncOperationAuthorizationRequest) (bool, error) {
	return true, nil
})

func newAsyncTestCoordinator(t *testing.T, store AsyncOperationStore, now *time.Time, cancellation bool, authorizer AsyncOperationAuthorizer) *AsyncOperationCoordinator {
	t.Helper()
	var ids atomic.Uint64
	coordinator, err := NewAsyncOperationCoordinator(AsyncOperationConfig{
		Store: store, Authorizer: authorizer,
		Now:       func() time.Time { return *now },
		NewID:     func() (string, error) { return fmt.Sprintf("op-%d", ids.Add(1)), nil },
		Location:  func(id string) string { return "/v1/operations/" + id },
		Retention: 5 * time.Minute, RetryAfter: 2 * time.Second,
		MaxProgressBytes: 32, MaxProgressTokens: 8,
		CancellationSupported: cancellation,
	})
	if err != nil {
		t.Fatalf("new async coordinator: %v", err)
	}
	return coordinator
}

type asyncTestStore struct {
	mu      sync.Mutex
	records map[string]AsyncOperationRecord
	keys    map[string]string
	waiters map[string][]chan struct{}
}

type asyncProcessLocalStore struct{ asyncTestStore *asyncTestStore }

func (s asyncProcessLocalStore) Durability() IdempotencyDurability { return IdempotencyProcessLocal }
func (s asyncProcessLocalStore) Accept(ctx context.Context, record AsyncOperationRecord, deduplicate bool) (AsyncOperationRecord, bool, error) {
	return s.asyncTestStore.Accept(ctx, record, deduplicate)
}
func (s asyncProcessLocalStore) Load(ctx context.Context, id string) (AsyncOperationRecord, error) {
	return s.asyncTestStore.Load(ctx, id)
}
func (s asyncProcessLocalStore) Pending(ctx context.Context, limit int) ([]AsyncOperationRecord, error) {
	return s.asyncTestStore.Pending(ctx, limit)
}
func (s asyncProcessLocalStore) CompareAndSwap(ctx context.Context, id string, expected uint64, next AsyncOperationHandle) (AsyncOperationRecord, bool, error) {
	return s.asyncTestStore.CompareAndSwap(ctx, id, expected, next)
}
func (s asyncProcessLocalStore) DeleteExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	return s.asyncTestStore.DeleteExpired(ctx, before, limit)
}

type asyncNoChangeStore struct{ asyncTestStore *asyncTestStore }

func (s asyncNoChangeStore) Durability() IdempotencyDurability { return s.asyncTestStore.Durability() }
func (s asyncNoChangeStore) Accept(ctx context.Context, record AsyncOperationRecord, deduplicate bool) (AsyncOperationRecord, bool, error) {
	return s.asyncTestStore.Accept(ctx, record, deduplicate)
}
func (s asyncNoChangeStore) Load(ctx context.Context, id string) (AsyncOperationRecord, error) {
	return s.asyncTestStore.Load(ctx, id)
}
func (s asyncNoChangeStore) Pending(ctx context.Context, limit int) ([]AsyncOperationRecord, error) {
	return s.asyncTestStore.Pending(ctx, limit)
}
func (s asyncNoChangeStore) CompareAndSwap(ctx context.Context, id string, expected uint64, next AsyncOperationHandle) (AsyncOperationRecord, bool, error) {
	return s.asyncTestStore.CompareAndSwap(ctx, id, expected, next)
}
func (s asyncNoChangeStore) DeleteExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	return s.asyncTestStore.DeleteExpired(ctx, before, limit)
}

func newAsyncTestStore() *asyncTestStore {
	return &asyncTestStore{records: map[string]AsyncOperationRecord{}, keys: map[string]string{}, waiters: map[string][]chan struct{}{}}
}

func (*asyncTestStore) Durability() IdempotencyDurability { return IdempotencyDurable }

func (s *asyncTestStore) Accept(_ context.Context, record AsyncOperationRecord, deduplicate bool) (AsyncOperationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deduplicate {
		key := record.Binding.Tenant + "\x00" + record.Binding.Principal + "\x00" + record.Binding.Operation + "\x00" + record.Binding.IdempotencyKey
		if id := s.keys[key]; id != "" {
			existing := s.records[id]
			if existing.Binding.Fingerprint != record.Binding.Fingerprint {
				return AsyncOperationRecord{}, false, ErrAsyncOperationIdempotencyConflict
			}
			return existing, false, nil
		}
		s.keys[key] = record.Handle.ID
	}
	s.records[record.Handle.ID] = record
	return record, true, nil
}

func (s *asyncTestStore) Load(_ context.Context, id string) (AsyncOperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return AsyncOperationRecord{}, ErrAsyncOperationNotFound
	}
	return record, nil
}

func (s *asyncTestStore) Pending(_ context.Context, limit int) ([]AsyncOperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]AsyncOperationRecord, 0, limit)
	for _, record := range s.records {
		if record.Handle.State == AsyncOperationPending {
			records = append(records, record)
			if len(records) == limit {
				break
			}
		}
	}
	return records, nil
}

func (s *asyncTestStore) CompareAndSwap(_ context.Context, id string, expected uint64, next AsyncOperationHandle) (AsyncOperationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return AsyncOperationRecord{}, false, ErrAsyncOperationNotFound
	}
	if record.Handle.Revision != expected {
		return record, false, nil
	}
	record.Handle = next
	s.records[id] = record
	for _, waiter := range s.waiters[id] {
		close(waiter)
	}
	delete(s.waiters, id)
	return record, true, nil
}

func (s *asyncTestStore) WaitForRevision(ctx context.Context, id string, after uint64) error {
	s.mu.Lock()
	record, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return ErrAsyncOperationNotFound
	}
	if record.Handle.Revision > after {
		s.mu.Unlock()
		return nil
	}
	waiter := make(chan struct{})
	s.waiters[id] = append(s.waiters[id], waiter)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-waiter:
		return nil
	}
}

func (s *asyncTestStore) DeleteExpired(_ context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, record := range s.records {
		if removed == limit {
			break
		}
		if !record.Handle.ExpiresAt.After(before) {
			delete(s.records, id)
			removed++
		}
	}
	return removed, nil
}

func (s *asyncTestStore) transitionForTest(t *testing.T, id string, state AsyncOperationState, now time.Time) {
	t.Helper()
	record, err := s.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load test record: %v", err)
	}
	next := record.Handle
	next.State = state
	next.Revision++
	next.UpdatedAt = now
	if state == AsyncOperationRunning {
		next.StartedAt = now
	}
	if _, swapped, err := s.CompareAndSwap(context.Background(), id, record.Handle.Revision, next); err != nil || !swapped {
		t.Fatalf("test transition: swapped=%t err=%v", swapped, err)
	}
}
