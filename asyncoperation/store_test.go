package asyncoperation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	naatreruntime "github.com/valksor/naatre/runtime"
)

func TestSQLiteStoreSurvivesRestartAndDeduplicatesAcceptance(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "operations.db")
	store := openTestStore(t, path, clock)
	coordinator := newTestCoordinator(t, store, clock, "first")
	owner := ownerContext("principal-a")
	submission := testSubmission("key-a", "fingerprint-a")
	accepted, err := coordinator.Submit(owner, submission)
	if err != nil || !accepted.Created {
		t.Fatalf("submit = %#v, %v", accepted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestStore(t, path, clock)
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := newTestCoordinator(t, reopened, clock, "second")
	duplicate, err := restarted.Submit(owner, submission)
	if err != nil || duplicate.Created || duplicate.Handle.ID != accepted.Handle.ID || duplicate.Handle.Revision != 1 {
		t.Fatalf("duplicate = %#v, %v", duplicate, err)
	}
	conflict := submission
	conflict.Fingerprint = "different-fingerprint"
	if _, err := restarted.Submit(owner, conflict); ErrorCode(err) != naatreruntime.CodeIdempotencyConflict {
		t.Fatalf("conflict code = %q, error = %v", ErrorCode(err), err)
	}
}

func TestLeaseExpiryFencesStaleWorkerPublication(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "operations.db"), clock)
	t.Cleanup(func() { _ = store.Close() })
	coordinator := newTestCoordinator(t, store, clock, "lease")
	accepted, err := coordinator.Submit(ownerContext("principal-a"), testSubmission("", ""))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	worker := naatreruntime.AsyncOperationWorkerFunc(func(context.Context, naatreruntime.AsyncOperationWork, naatreruntime.AsyncOperationControl) naatreruntime.AsyncOperationCompletion {
		close(started)
		<-release
		outcome := naatreruntime.Outcome{Data: map[string]any{"protected": "stale-result"}}
		return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationSucceeded, Result: &outcome}
	})
	type runResult struct {
		handle naatreruntime.AsyncOperationHandle
		err    error
	}
	result := make(chan runResult, 1)
	go func() {
		handle, runErr := coordinator.Run(context.Background(), accepted.Handle.ID, worker)
		result <- runResult{handle: handle, err: runErr}
	}()
	<-started
	lease, err := store.CurrentLease(context.Background(), accepted.Handle.ID)
	if err != nil || lease.Fence != 1 {
		t.Fatalf("lease = %#v, %v", lease, err)
	}
	if _, err := store.RenewLease(context.Background(), Lease{HandleID: lease.HandleID, Fence: lease.Fence + 1, Until: lease.Until}); ErrorCode(err) != CodeFenceRejected {
		t.Fatalf("wrong-fence renewal code = %q, error = %v", ErrorCode(err), err)
	}
	clock.Advance(11 * time.Second)
	close(release)
	stale := <-result
	if ErrorCode(stale.err) != CodeLeaseExpired || stale.handle.Result != nil {
		t.Fatalf("expired worker publication = %#v, %v", stale.handle, stale.err)
	}
	if expired, err := store.ExpireLeases(context.Background(), clock.Now(), 1); err != nil || expired != 1 {
		t.Fatalf("expired = %d, %v", expired, err)
	}
	recovered, err := store.Load(context.Background(), accepted.Handle.ID)
	if err != nil || recovered.Handle.State != naatreruntime.AsyncOperationIndeterminate || recovered.Handle.Result != nil {
		t.Fatalf("expired lease recovery = %#v, %v", recovered.Handle, err)
	}
	invocations := 0
	terminal, err := coordinator.Run(context.Background(), accepted.Handle.ID, naatreruntime.AsyncOperationWorkerFunc(func(context.Context, naatreruntime.AsyncOperationWork, naatreruntime.AsyncOperationControl) naatreruntime.AsyncOperationCompletion {
		invocations++
		return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationSucceeded}
	}))
	if err != nil || terminal.State != naatreruntime.AsyncOperationIndeterminate || invocations != 0 {
		t.Fatalf("duplicate delivery = %#v, %v, invocations %d", terminal, err, invocations)
	}
}

func TestAuthorizedPollingDoesNotLeakAndRetentionIsBounded(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "operations.db"), clock)
	t.Cleanup(func() { _ = store.Close() })
	coordinator := newTestCoordinator(t, store, clock, "poll")
	owner := ownerContext("principal-a")
	accepted, err := coordinator.Submit(owner, testSubmission("", ""))
	if err != nil {
		t.Fatal(err)
	}
	outcome := naatreruntime.Outcome{Data: map[string]any{"visible": "owner-only"}}
	completed, err := coordinator.Run(context.Background(), accepted.Handle.ID, naatreruntime.AsyncOperationWorkerFunc(func(ctx context.Context, _ naatreruntime.AsyncOperationWork, control naatreruntime.AsyncOperationControl) naatreruntime.AsyncOperationCompletion {
		if _, progressErr := control.UpdateProgress(ctx, []byte(`{"completed":1}`)); progressErr != nil {
			return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationIndeterminate}
		}
		return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationSucceeded, Result: &outcome}
	}))
	if err != nil || completed.ExpiresAt.Sub(completed.FinishedAt) != 5*time.Minute || completed.Progress.Sequence != 1 || string(completed.Progress.Payload) != `{"completed":1}` {
		t.Fatalf("completion = %#v, %v", completed, err)
	}
	_, deniedErr := coordinator.Poll(ownerContext("principal-b"), accepted.Handle.ID, "")
	_, unknownErr := coordinator.Poll(ownerContext("principal-b"), "unknown-handle", "")
	if !errors.Is(deniedErr, naatreruntime.ErrAsyncOperationUnavailable) || !errors.Is(unknownErr, naatreruntime.ErrAsyncOperationUnavailable) || deniedErr.Error() != unknownErr.Error() {
		t.Fatalf("denied = %v, unknown = %v", deniedErr, unknownErr)
	}
	poll, err := coordinator.Poll(owner, accepted.Handle.ID, "")
	if err != nil || poll.Handle.Result == nil || poll.Handle.Result.Data["visible"] != "owner-only" {
		t.Fatalf("owner poll = %#v, %v", poll, err)
	}
	clock.Advance(5 * time.Minute)
	if _, err := coordinator.Poll(owner, accepted.Handle.ID, ""); !errors.Is(err, naatreruntime.ErrAsyncOperationUnavailable) {
		t.Fatalf("expired poll error = %v", err)
	}
	if deleted, err := store.DeleteExpired(context.Background(), clock.Now(), 1); err != nil || deleted != 1 {
		t.Fatalf("delete expired = %d, %v", deleted, err)
	}
}

func TestWorkerAdapterPropagatesCancellationAndRenewsLease(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "operations.db"), clock)
	t.Cleanup(func() { _ = store.Close() })
	coordinator := newTestCoordinator(t, store, clock, "cancel")
	owner := ownerContext("principal-a")
	accepted, err := coordinator.Submit(owner, testSubmission("", ""))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	worker := naatreruntime.AsyncOperationWorkerFunc(func(ctx context.Context, _ naatreruntime.AsyncOperationWork, control naatreruntime.AsyncOperationControl) naatreruntime.AsyncOperationCompletion {
		close(started)
		for {
			cancelled, err := control.CancellationRequested(ctx)
			if err != nil {
				return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationIndeterminate}
			}
			if cancelled {
				return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationCancelled}
			}
			time.Sleep(time.Millisecond)
		}
	})
	adapter, err := NewWorkerAdapter(WorkerAdapterConfig{Store: store, Worker: worker, HeartbeatInterval: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan naatreruntime.AsyncOperationHandle, 1)
	go func() {
		handle, _ := coordinator.Run(context.Background(), accepted.Handle.ID, adapter)
		result <- handle
	}()
	<-started
	cancellation, err := coordinator.Cancel(owner, accepted.Handle.ID)
	if err != nil || cancellation.Disposition != naatreruntime.AsyncCancellationRequested {
		t.Fatalf("cancellation = %#v, %v", cancellation, err)
	}
	completed := <-result
	if completed.State != naatreruntime.AsyncOperationCancelled || completed.Result != nil || !completed.ExpiresAt.After(completed.FinishedAt) {
		t.Fatalf("cancelled completion = %#v", completed)
	}
}

func TestDispatcherRecoversPendingAndHintFailurePreservesAcceptance(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "operations.db"), clock)
	t.Cleanup(func() { _ = store.Close() })
	coordinator := newTestCoordinator(t, store, clock, "dispatch")
	adapter := SubmissionAdapter{
		Coordinator: coordinator,
		Publisher: HintPublisherFunc(func(context.Context, string) error {
			return errors.New("broker password=must-not-escape")
		}),
	}
	accepted, hinted, err := adapter.Submit(ownerContext("principal-a"), testSubmission("", ""))
	if err != nil || hinted || !accepted.Created {
		t.Fatalf("accept with lost hint = %#v, hinted %t, %v", accepted, hinted, err)
	}
	invocations := 0
	dispatcher := Dispatcher{
		Coordinator: coordinator,
		Store:       store,
		Worker: naatreruntime.AsyncOperationWorkerFunc(func(context.Context, naatreruntime.AsyncOperationWork, naatreruntime.AsyncOperationControl) naatreruntime.AsyncOperationCompletion {
			invocations++
			return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationSucceeded}
		}),
		Now: clock.Now, MaxBatch: 4,
	}
	handles, err := dispatcher.RunOnce(context.Background())
	if err != nil || len(handles) != 1 || handles[0].ID != accepted.Handle.ID || handles[0].State != naatreruntime.AsyncOperationSucceeded {
		t.Fatalf("recovery = %#v, %v", handles, err)
	}
	if duplicate, err := coordinator.Run(context.Background(), accepted.Handle.ID, dispatcher.Worker); err != nil || duplicate.State != naatreruntime.AsyncOperationSucceeded || invocations != 1 {
		t.Fatalf("terminal duplicate = %#v, %v, invocations %d", duplicate, err, invocations)
	}
}

func TestStoreRejectsResourceExcessAndRedactsBackendFailures(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "password=database-secret.db"), clock)
	coordinator := newTestCoordinator(t, store, clock, "limits")
	secret := strings.Repeat("credential-value", 20_000)
	submission := testSubmission("", "")
	submission.Payload = []byte(fmt.Sprintf(`{"secret":%q}`, secret))
	if _, err := coordinator.Submit(ownerContext("principal-a"), submission); ErrorCode(err) != CodeResourceExhausted || strings.Contains(err.Error(), "credential-value") {
		t.Fatalf("oversized failure code = %q, error = %v", ErrorCode(err), err)
	}
	if _, err := store.Pending(context.Background(), DefaultLimits().MaxPendingBatch+1); ErrorCode(err) != CodeResourceExhausted {
		t.Fatalf("pending bound code = %q, error = %v", ErrorCode(err), err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(context.Background(), "any-id")
	if ErrorCode(err) != CodeStoreUnavailable || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "database-secret") {
		t.Fatalf("backend failure = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Pending(cancelled, 1); ErrorCode(err) != CodeCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled failure = %v", err)
	}
}

func openTestStore(t *testing.T, path string, clock *testClock) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(context.Background(), path, StoreConfig{
		Now: clock.Now, LeaseDuration: 10 * time.Second, ResultRetention: 5 * time.Minute, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.db.SetMaxOpenConns(1)
	return store
}

func newTestCoordinator(t *testing.T, store *SQLiteStore, clock *testClock, prefix string) *naatreruntime.AsyncOperationCoordinator {
	t.Helper()
	var sequence atomic.Uint64
	coordinator, err := naatreruntime.NewAsyncOperationCoordinator(naatreruntime.AsyncOperationConfig{
		Store: store,
		Authorizer: naatreruntime.AsyncOperationAuthorizerFunc(func(_ context.Context, request naatreruntime.AsyncOperationAuthorizationRequest) (bool, error) {
			return request.Principal.Subject == request.Binding.Principal && request.Principal.Tenant == request.Binding.Tenant, nil
		}),
		Now:       clock.Now,
		NewID:     func() (string, error) { return fmt.Sprintf("%s-%d", prefix, sequence.Add(1)), nil },
		Location:  func(id string) string { return "/v1/operations/" + id },
		Retention: 5 * time.Minute, RetryAfter: time.Second,
		MaxProgressBytes: 1024, MaxProgressTokens: 64,
		CancellationSupported: true,
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	return coordinator
}

func ownerContext(subject string) context.Context {
	return naatreruntime.WithPrincipal(context.Background(), naatreruntime.Principal{Subject: subject, Tenant: "tenant-a", AuthorizationRevision: "policy-1"})
}

func testSubmission(key, fingerprint string) naatreruntime.AsyncOperationSubmission {
	return naatreruntime.AsyncOperationSubmission{
		Operation: "mutation.longTask", SchemaRevision: "schema-1", IdempotencyKey: key, Fingerprint: fingerprint,
		RequireIdempotency: key != "", Payload: []byte(`{"input":"opaque"}`),
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}
