package mutation_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/mutation"
)

func TestConditionalMutationCommitsAtMostOnce(t *testing.T) {
	t.Parallel()
	provider, revision, writes := providerFixture(t, mutation.ProviderCapabilities{ConditionalWrites: true})
	request := mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: revision, Update: nameUpdate("new")}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := provider.Apply(context.Background(), request)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var typed *mutation.Error
		if errors.As(err, &typed) && typed.Code == mutation.CodeRevisionConflict {
			conflicts++
			continue
		}
		t.Fatalf("unexpected apply error: %v", err)
	}
	if successes != 1 || conflicts != 1 || writes.Load() != 1 {
		t.Fatalf("successes=%d conflicts=%d writes=%d", successes, conflicts, writes.Load())
	}
}

func TestPreconditionFailuresDoNotInvokeWriteOrLeakState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		capabilities mutation.ProviderCapabilities
		request      func(mutation.Revision) mutation.ApplyRequest
		authorized   bool
		code         string
	}{
		{name: "missing", capabilities: mutation.ProviderCapabilities{ConditionalWrites: true}, request: func(mutation.Revision) mutation.ApplyRequest {
			return mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Update: nameUpdate("new")}
		}, authorized: true, code: mutation.CodePreconditionRequired},
		{name: "stale", capabilities: mutation.ProviderCapabilities{ConditionalWrites: true}, request: func(mutation.Revision) mutation.ApplyRequest {
			return mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: mutation.Revision("nr1_0000000000000000000000000000000000000000000"), Update: nameUpdate("new")}
		}, authorized: true, code: mutation.CodeRevisionConflict},
		{name: "not found", capabilities: mutation.ProviderCapabilities{ConditionalWrites: true}, request: func(revision mutation.Revision) mutation.ApplyRequest {
			return mutation.ApplyRequest{Mutation: "updateUser", Entity: "absent", Expected: revision, Update: nameUpdate("new")}
		}, authorized: true, code: mutation.CodeEntityNotFound},
		{name: "unsupported", capabilities: mutation.ProviderCapabilities{}, request: func(revision mutation.Revision) mutation.ApplyRequest {
			return mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: revision, Update: nameUpdate("new")}
		}, authorized: true, code: mutation.CodePreconditionUnsupported},
		{name: "unauthorized", capabilities: mutation.ProviderCapabilities{ConditionalWrites: true}, request: func(revision mutation.Revision) mutation.ApplyRequest {
			return mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: revision, Update: nameUpdate("new")}
		}, authorized: false, code: mutation.CodeUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, revision, writes := providerFixtureAuthorized(t, test.capabilities, test.authorized)
			_, err := provider.Apply(context.Background(), test.request(revision))
			assertCode(t, err, test.code)
			if writes.Load() != 0 {
				t.Fatalf("protected writes = %d", writes.Load())
			}
			var typed *mutation.Error
			if !errors.As(err, &typed) || len(typed.Path) != 0 {
				t.Fatalf("unsafe error details = %#v", err)
			}
		})
	}
}

func TestIdempotencyReplayPrecedesNewerRevisionCheck(t *testing.T) {
	t.Parallel()
	provider, firstRevision, writes := providerFixture(t, mutation.ProviderCapabilities{ConditionalWrites: true})
	first := mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: firstRevision, Update: nameUpdate("first"), IdempotencyKey: "request-1"}
	recorded, err := provider.Apply(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Apply(context.Background(), mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: recorded.Revision, Update: nameUpdate("second")}); err != nil {
		t.Fatal(err)
	}
	replayed, err := provider.Apply(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Revision != recorded.Revision || replayed.Value["name"] != "first" || writes.Load() != 2 {
		t.Fatalf("replay = %#v, writes = %d", replayed, writes.Load())
	}
}

func TestIdempotencyReplayReauthorizesFields(t *testing.T) {
	t.Parallel()
	provider, err := mutation.NewMemoryProvider(mutation.ProviderCapabilities{ConditionalWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	allowed.Store(true)
	if err := provider.Register("updateUser", mutation.Registration{
		Descriptor:     updateDescriptor(t, true),
		AuthorizeField: func(context.Context, string, string) bool { return allowed.Load() },
	}); err != nil {
		t.Fatal(err)
	}
	revision, err := provider.Seed("updateUser", "user-1", map[string]any{"name": "old"})
	if err != nil {
		t.Fatal(err)
	}
	request := mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: revision, Update: nameUpdate("new"), IdempotencyKey: "request-1"}
	if _, err := provider.Apply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	allowed.Store(false)
	result, err := provider.Apply(context.Background(), request)
	assertCode(t, err, mutation.CodeUnauthorized)
	if result.Revision != "" || len(result.Value) != 0 {
		t.Fatalf("unauthorized replay leaked result %#v", result)
	}
}

func TestReadConsistencySnapshotAndReadYourWrites(t *testing.T) {
	t.Parallel()
	provider, revision, _ := providerFixture(t, mutation.ProviderCapabilities{ConditionalWrites: true, SnapshotReads: true, ReadYourWrites: true})
	snapshot, err := provider.Read(context.Background(), mutation.ReadRequest{Mutation: "updateUser", Entity: "user-1", Consistency: mutation.ReadSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := provider.Apply(context.Background(), mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: revision, Update: nameUpdate("new")})
	if err != nil {
		t.Fatal(err)
	}
	old, err := provider.Read(context.Background(), mutation.ReadRequest{Mutation: "updateUser", Entity: "user-1", Consistency: mutation.ReadSnapshot, Snapshot: snapshot.Snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if old.Revision != snapshot.Revision || old.Value["name"] != "old" {
		t.Fatalf("snapshot read = %#v", old)
	}
	fresh, err := provider.Read(context.Background(), mutation.ReadRequest{Mutation: "updateUser", Entity: "user-1", Consistency: mutation.ReadYourWrites, AfterRevision: committed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Revision != committed.Revision || fresh.Value["name"] != "new" {
		t.Fatalf("read-your-writes = %#v", fresh)
	}
}

func TestUnsupportedReadConsistencyIsExplicit(t *testing.T) {
	t.Parallel()
	provider, _, _ := providerFixture(t, mutation.ProviderCapabilities{ConditionalWrites: true})
	_, err := provider.Read(context.Background(), mutation.ReadRequest{Mutation: "updateUser", Entity: "user-1", Consistency: mutation.ReadSnapshot})
	assertCode(t, err, mutation.CodeReadConsistencyUnsupported)
}

func TestFailedProtectedWritePreservesCommittedRevision(t *testing.T) {
	t.Parallel()
	provider, err := mutation.NewMemoryProvider(mutation.ProviderCapabilities{ConditionalWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := updateDescriptor(t, true)
	if err := provider.Register("updateUser", mutation.Registration{
		Descriptor: descriptor,
		Write:      func(context.Context, string, map[string]any, map[string]any) error { return errors.New("rollback") },
	}); err != nil {
		t.Fatal(err)
	}
	revision, err := provider.Seed("updateUser", "user-1", map[string]any{"name": "old"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Apply(context.Background(), mutation.ApplyRequest{Mutation: "updateUser", Entity: "user-1", Expected: revision, Update: nameUpdate("tentative")})
	assertCode(t, err, mutation.CodeProviderFailed)
	read, err := provider.Read(context.Background(), mutation.ReadRequest{Mutation: "updateUser", Entity: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if read.Revision != revision || read.Value["name"] != "old" {
		t.Fatalf("read after rollback = %#v", read)
	}
}

func providerFixture(t testing.TB, capabilities mutation.ProviderCapabilities) (*mutation.MemoryProvider, mutation.Revision, *atomic.Int32) {
	t.Helper()
	return providerFixtureAuthorized(t, capabilities, true)
}

func providerFixtureAuthorized(t testing.TB, capabilities mutation.ProviderCapabilities, authorized bool) (*mutation.MemoryProvider, mutation.Revision, *atomic.Int32) {
	t.Helper()
	provider, err := mutation.NewMemoryProvider(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	writes := new(atomic.Int32)
	if err := provider.Register("updateUser", mutation.Registration{
		Descriptor: updateDescriptor(t, true),
		Authorize:  func(context.Context, string) bool { return authorized },
		Write:      func(context.Context, string, map[string]any, map[string]any) error { writes.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	revision, err := provider.Seed("updateUser", "user-1", map[string]any{"name": "old", "nickname": "nick", "tags": []any{"a", "b"}, "labels": map[string]any{"x": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	return provider, revision, writes
}

func nameUpdate(value string) *mutation.UpdateInput {
	return &mutation.UpdateInput{Edits: []mutation.Edit{{Field: "User.name", Action: mutation.EditSet, Value: value}}}
}
