package runtime_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

func TestStreamCursorCannotCrossSecurityOrRevisionBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	codec, err := runtime.NewStreamCursorCodec(runtime.StreamCursorCodecConfig{
		ActiveKeyID: "active", Keys: map[string][]byte{"active": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "stream-a", "tenant-a", "principal-a", "auth-r1", "schema-r1")
	cursor, err := codec.Encode(scope, 7)
	if err != nil {
		t.Fatal(err)
	}
	if position, err := codec.Decode(cursor, scope); err != nil || position != 7 {
		t.Fatalf("decode = %d, %v", position, err)
	}

	mismatches := []runtime.StreamCursorScope{
		streamScope(t, "stream-b", "tenant-a", "principal-a", "auth-r1", "schema-r1"),
		streamScope(t, "stream-a", "tenant-b", "principal-a", "auth-r1", "schema-r1"),
		streamScope(t, "stream-a", "tenant-a", "principal-b", "auth-r1", "schema-r1"),
		streamScope(t, "stream-a", "tenant-a", "principal-a", "auth-r2", "schema-r1"),
		streamScope(t, "stream-a", "tenant-a", "principal-a", "auth-r1", "schema-r2"),
	}
	for _, mismatch := range mismatches {
		if _, err := codec.Decode(cursor, mismatch); !errors.Is(err, runtime.ErrStreamHistoryUnavailable) {
			t.Fatalf("scope mismatch error = %v", err)
		}
	}
	tampered := cursor[:len(cursor)-1] + "A"
	if _, err := codec.Decode(tampered, scope); !errors.Is(err, runtime.ErrStreamHistoryUnavailable) {
		t.Fatalf("tampered cursor error = %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := codec.Decode(cursor, scope); !errors.Is(err, runtime.ErrStreamHistoryUnavailable) {
		t.Fatalf("expired cursor error = %v", err)
	}
}

func TestStreamCursorRejectsUnboundedOrControlKeyIdentifiers(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	for _, id := range []string{"key\u0085id", strings.Repeat("a", runtime.StreamReferenceMaxBytes+1)} {
		if _, err := runtime.NewStreamCursorCodec(runtime.StreamCursorCodecConfig{
			ActiveKeyID: id, Keys: map[string][]byte{id: key}, TTL: time.Minute,
		}); !errors.Is(err, runtime.ErrInvalidStreamCursor) {
			t.Fatalf("invalid key id error = %v", err)
		}
	}
}

func TestStreamReplayHandoffHasNoGapOrDuplicate(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 8, MaxHistoryBytes: 4096, SubscriberQueue: 4})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "stream-a", "tenant-a", "principal-a", "auth-r1", "schema-r1")
	if err := store.Publish(scope, streamDataFrame("stream-a", 2, 1, `{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(scope, streamDataFrame("stream-a", 3, 2, `{"value":2}`)); err != nil {
		t.Fatal(err)
	}

	subscription, err := subscribeStream(t, store, scope, 1, streamReplayOptions(3, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 2048}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Close)
	if err := store.Publish(scope, streamDataFrame("stream-a", 4, 3, `{"value":3}`)); err != nil {
		t.Fatal(err)
	}
	first := nextStreamFrame(t, subscription)
	second := nextStreamFrame(t, subscription)
	third := nextStreamFrame(t, subscription)
	if first.Type != protocol.StreamResume || first.Sequence != 3 || second.Position != 2 || second.Sequence != 4 || third.Position != 3 || third.Sequence != 5 {
		t.Fatalf("handoff frames = %#v, %#v, %#v", first, second, third)
	}

	emptyScope := streamScope(t, "stream-empty", "tenant-a", "principal-a", "auth-r1", "schema-r1")
	empty, err := subscribeStream(t, store, emptyScope, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 2048}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(empty.Close)
	if err := store.Publish(emptyScope, streamDataFrame("stream-empty", 2, 1, `{"value":"live"}`)); err != nil {
		t.Fatal(err)
	}
	if got := nextStreamFrame(t, empty); got.Position != 1 {
		t.Fatalf("empty-history live position = %d", got.Position)
	}
}

func TestStreamReplayUnavailableOutcomesAreSafeAndBounded(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "stream-a", "tenant-a", "principal-a", "auth-r1", "schema-r1")
	for position := uint64(1); position <= 3; position++ {
		if err := store.Publish(scope, streamDataFrame("stream-a", position+1, position, `{"value":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	for _, after := range []uint64{0, 4} {
		if _, err := subscribeStream(t, store, scope, after, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 2048})); !errors.Is(err, runtime.ErrStreamHistoryUnavailable) {
			t.Fatalf("unavailable after %d = %v", after, err)
		}
	}
	if _, err := subscribeStream(t, store, scope, 1, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 1, MaxBytes: 2048})); !errors.Is(err, runtime.ErrStreamHistoryUnavailable) {
		t.Fatalf("over-budget replay error = %v", err)
	}

	subscription, err := subscribeStream(t, store, scope, 3, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 2048}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Close)
	if err := store.Publish(scope, streamDataFrame("stream-a", 5, 4, `{"value":4}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(scope, streamDataFrame("stream-a", 6, 5, `{"value":5}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, runtime.ErrStreamSlowConsumer) {
		t.Fatalf("slow-consumer error = %v", err)
	}
}

func TestStreamReplayCancellationDetachesSubscriber(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "stream-a", "tenant-a", "principal-a", "auth-r1", "schema-r1")
	subscription, err := subscribeStream(t, store, scope, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 2048}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := subscription.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	for position := uint64(1); position <= 2; position++ {
		if err := store.Publish(scope, streamDataFrame("stream-a", position+1, position, `true`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("detached subscription error = %v", err)
	}
}

func TestStreamReplayBoundsGlobalScopesBytesAndSubscribers(t *testing.T) {
	t.Parallel()
	scopeA := streamScope(t, "stream-a", "tenant", "principal", "auth", "schema")
	scopeB := streamScope(t, "stream-b", "tenant", "principal", "auth", "schema")
	frameA := streamDataFrame("stream-a", 2, 1, `true`)
	encoded, err := protocol.MarshalStreamFrame(frameA, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}

	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{
		MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1,
		MaxStreams: 1, MaxTotalHistoryBytes: 4096, MaxSubscribers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := subscribeStream(t, store, scopeA, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscribeStream(t, store, scopeB, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})); !errors.Is(err, runtime.ErrStreamReplayLimit) {
		t.Fatalf("stream scope limit error = %v", err)
	}
	first.Close()
	second, err := subscribeStream(t, store, scopeB, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096}))
	if err != nil {
		t.Fatalf("reclaimed stream scope: %v", err)
	}
	second.Close()

	byteStore, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{
		MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1,
		MaxStreams: 2, MaxTotalHistoryBytes: uint64(len(encoded)), MaxSubscribers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := byteStore.Publish(scopeA, frameA); err != nil {
		t.Fatal(err)
	}
	if err := byteStore.Publish(scopeB, streamDataFrame("stream-b", 2, 1, `true`)); !errors.Is(err, runtime.ErrStreamReplayLimit) {
		t.Fatalf("aggregate byte limit error = %v", err)
	}

	subscriberStore, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{
		MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1,
		MaxStreams: 1, MaxTotalHistoryBytes: 4096, MaxSubscribers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := subscribeStream(t, subscriberStore, scopeA, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscribeStream(t, subscriberStore, scopeA, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})); !errors.Is(err, runtime.ErrStreamReplayLimit) {
		t.Fatalf("subscriber limit error = %v", err)
	}
	subscription.Close()
	if replacement, err := subscribeStream(t, subscriberStore, scopeA, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})); err != nil {
		t.Fatalf("reclaimed subscriber slot: %v", err)
	} else {
		replacement.Close()
	}
}

func TestStreamReplayResequencesSparsePositionsAndReauthorizes(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "s", "tenant", "principal", "auth", "schema-r1")
	for _, frame := range []protocol.StreamFrame{
		streamDataFrame("s", 40, 10, `{"value":false}`),
		{Type: protocol.StreamError, Stream: "s", Sequence: 70, Position: 15, HasPosition: true, Path: []any{"value"}, Error: &protocol.StreamFrameError{Code: "STALE", Message: "refresh failed"}},
		{Type: protocol.StreamPatch, Stream: "s", Sequence: 90, Position: 20, HasPosition: true, Path: []any{"value"}, Data: []byte(`true`)},
	} {
		if err := store.Publish(scope, frame); err != nil {
			t.Fatalf("publish sparse position %d: %v", frame.Position, err)
		}
	}
	checks := 0
	options := streamReplayOptions(3, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 4096})
	options.Reauthorize = func(context.Context, protocol.StreamFrame) error { checks++; return nil }
	subscription, err := subscribeStream(t, store, scope, 0, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Close)
	receiver, err := protocol.NewStreamReceiver("s", protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamResume, Stream: "s", Sequence: 2, Cursor: "cursor"},
	} {
		if _, err := receiver.Accept(control); err != nil {
			t.Fatal(err)
		}
	}
	for wantSequence := uint64(3); wantSequence <= 5; wantSequence++ {
		frame := nextStreamFrame(t, subscription)
		if frame.Sequence != wantSequence {
			t.Fatalf("delivery sequence = %d, want %d", frame.Sequence, wantSequence)
		}
		if _, err := receiver.Accept(frame); err != nil {
			t.Fatalf("receiver rejected resequenced replay: %v", err)
		}
	}
	if checks != 6 || len(receiver.Errors()) != 1 || string(receiver.Snapshot()) != `{"value":true}` {
		t.Fatalf("checks/snapshot = %d/%s", checks, receiver.Snapshot())
	}
}

func TestStreamReplayResumeAppliesPatchToCarriedSnapshot(t *testing.T) {
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "s", "tenant", "principal", "auth", "schema-r1")
	for _, frame := range []protocol.StreamFrame{
		streamDataFrame("s", 2, 1, `{"value":false}`),
		{Type: protocol.StreamPatch, Stream: "s", Sequence: 3, Position: 2, HasPosition: true, Path: []any{"value"}, Data: []byte(`true`)},
	} {
		if err := store.Publish(scope, frame); err != nil {
			t.Fatal(err)
		}
	}
	options := streamReplayOptions(2, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 4096})
	subscription, err := subscribeStream(t, store, scope, 1, options)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	receiver, err := protocol.NewResumingStreamReceiver("s", []byte(`{"value":false}`), 1, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := receiver.Accept(nextStreamFrame(t, subscription)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := receiver.Accept(protocol.StreamFrame{Type: protocol.StreamComplete, Stream: "s", Sequence: 4}); err != nil {
		t.Fatal(err)
	}
	if got := string(receiver.Snapshot()); got != `{"value":true}` {
		t.Fatalf("resumed replay snapshot = %s", got)
	}
}

func TestStreamReplayFailsClosedOnMissingPanickingOrCancelledGuards(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "s", "tenant", "principal", "auth", "schema-r1")
	if err := store.Publish(scope, streamDataFrame("s", 2, 1, `true`)); err != nil {
		t.Fatal(err)
	}
	missing := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	missing.Reauthorize = nil
	if _, err := subscribeStream(t, store, scope, 0, missing); !errors.Is(err, runtime.ErrInvalidStreamSubscription) {
		t.Fatalf("missing guard error = %v", err)
	}
	unsupported := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	unsupported.ProfileVersion = "2"
	if _, err := subscribeStream(t, store, scope, 0, unsupported); !errors.Is(err, protocol.ErrUnsupportedStreamVersion) {
		t.Fatalf("unsupported profile error = %v", err)
	}
	panicking := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	panicking.Reauthorize = func(context.Context, protocol.StreamFrame) error { panic("boom") }
	if _, err := subscribeStream(t, store, scope, 0, panicking); !errors.Is(err, runtime.ErrStreamAuthorizationRevoked) {
		t.Fatalf("panic fresh-subscription guard error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	cancelled.Reauthorize = func(context.Context, protocol.StreamFrame) error { cancel(); return nil }
	subscription, err := subscribeStream(t, store, scope, 0, cancelled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled guard delivery error = %v", err)
	}

	overflowStore, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	overflowScope := streamScope(t, "overflow", "tenant", "principal", "auth", "schema-r1")
	revoked := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 4, MaxBytes: 4096})
	var revokedNow atomic.Bool
	revoked.Reauthorize = func(context.Context, protocol.StreamFrame) error {
		if revokedNow.Load() {
			return runtime.ErrStreamAuthorizationRevoked
		}
		return nil
	}
	revoked.ReauthorizeSession = func(context.Context) error {
		if revokedNow.Load() {
			return runtime.ErrStreamAuthorizationRevoked
		}
		return nil
	}
	overflowed, err := subscribeStream(t, overflowStore, overflowScope, 0, revoked)
	if err != nil {
		t.Fatal(err)
	}
	for position := uint64(1); position <= 2; position++ {
		if err := overflowStore.Publish(overflowScope, streamDataFrame("overflow", position+1, position, `true`)); err != nil {
			t.Fatal(err)
		}
	}
	revokedNow.Store(true)
	if _, err := overflowed.Next(context.Background()); !errors.Is(err, runtime.ErrStreamAuthorizationRevoked) {
		t.Fatalf("revocation must win over overflow: %v", err)
	}
}

func TestStreamReplaySequenceExhaustionNeverReturnsZero(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "s", "tenant", "principal", "auth", "schema-r1")
	for position := uint64(1); position <= 1; position++ {
		if err := store.Publish(scope, streamDataFrame("s", position+1, position, `true`)); err != nil {
			t.Fatal(err)
		}
	}
	options := streamReplayOptions(^uint64(0), runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	if _, err := subscribeStream(t, store, scope, 0, options); !errors.Is(err, runtime.ErrStreamReplayLimit) {
		t.Fatalf("sequence exhaustion subscription error = %v", err)
	}

	liveStore, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	liveOptions := streamReplayOptions(^uint64(0)-1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	subscription, err := subscribeStream(t, liveStore, scope, 0, liveOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := liveStore.Publish(scope, streamDataFrame("s", 2, 1, `true`)); err != nil {
		t.Fatal(err)
	}
	if frame := nextStreamFrame(t, subscription); frame.Sequence != ^uint64(0)-1 {
		t.Fatalf("last deliverable sequence = %d", frame.Sequence)
	}
	if err := liveStore.Publish(scope, streamDataFrame("s", 3, 2, `true`)); err != nil {
		t.Fatal(err)
	}
	if frame, err := subscription.Next(context.Background()); !errors.Is(err, runtime.ErrStreamReplayLimit) || frame.Sequence != 0 {
		t.Fatalf("live sequence exhaustion frame/error = %#v/%v", frame, err)
	}
}

func TestStreamReplayFinalizeReclaimsCompletedStream(t *testing.T) {
	t.Parallel()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{
		MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1,
		MaxStreams: 1, MaxTotalHistoryBytes: 4096, MaxSubscribers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	scopeA := streamScope(t, "a", "tenant", "principal", "auth", "schema-r1")
	scopeB := streamScope(t, "b", "tenant", "principal", "auth", "schema-r1")
	subscription, err := subscribeStream(t, store, scopeA, 0, streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096}))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(scopeA, streamDataFrame("a", 2, 1, `true`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(scopeA); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("finalized subscription = %v", err)
	}
	if err := store.Publish(scopeB, streamDataFrame("b", 2, 1, `true`)); err != nil {
		t.Fatalf("completed stream slot was not reclaimed: %v", err)
	}
	if err := store.Finalize(runtime.StreamCursorScope{}); !errors.Is(err, runtime.ErrInvalidStreamSubscription) {
		t.Fatalf("invalid finalization scope = %v", err)
	}
}

func TestStreamReplayAuthenticationDeadlineUsesWallClock(t *testing.T) {
	retentionNow := time.Unix(1_800_000_000, 0)
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{
		MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1,
		Now: func() time.Time { return retentionNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "s", "tenant", "principal", "auth", "schema-r1")
	options := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	options.AuthenticationExpiresAt = time.Now().Add(20 * time.Millisecond)
	subscription, err := subscribeStream(t, store, scope, 0, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, runtime.ErrStreamAuthenticationExpired) {
		t.Fatalf("replay authentication expiry = %v", err)
	}
}

func TestStreamReplayCloseCancelsInFlightGuardWithEOFCause(t *testing.T) {
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := streamScope(t, "s", "tenant", "principal", "auth", "schema-r1")
	if err := store.Publish(scope, streamDataFrame("s", 2, 1, `true`)); err != nil {
		t.Fatal(err)
	}
	var block atomic.Bool
	started := make(chan struct{})
	options := streamReplayOptions(1, runtime.StreamReplayLimits{MaxEvents: 2, MaxBytes: 4096})
	options.Reauthorize = func(ctx context.Context, _ protocol.StreamFrame) error {
		if !block.Load() {
			return nil
		}
		close(started)
		<-ctx.Done()
		return nil
	}
	subscription, err := subscribeStream(t, store, scope, 0, options)
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	result := make(chan error, 1)
	go func() {
		_, nextErr := subscription.Next(context.Background())
		result <- nextErr
	}()
	<-started
	subscription.Close()
	if err := <-result; !errors.Is(err, io.EOF) {
		t.Fatalf("close during replay guard = %v", err)
	}
}

func streamScope(t testing.TB, stream, tenant, principal, authorization, schema string) runtime.StreamCursorScope {
	t.Helper()
	options := runtime.StreamCursorScopeOptions{
		Stream: stream, Tenant: tenant, Principal: principal,
		AuthorizationRevision: authorization, SchemaRevision: schema,
	}
	scope, err := runtime.NewStreamCursorScope(options)
	if err == nil {
		return scope
	}
	t.Fatalf("stream scope construction: %v", err)
	return runtime.StreamCursorScope{}
}

func streamDataFrame(stream string, sequence, position uint64, payload string) protocol.StreamFrame {
	return protocol.StreamFrame{
		Type: protocol.StreamData, Stream: stream, Sequence: sequence,
		Position: position, HasPosition: true, Data: []byte(payload),
	}
}

func nextStreamFrame(t testing.TB, subscription *runtime.StreamReplaySubscription) protocol.StreamFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	frame, err := subscription.Next(ctx)
	cancel()
	if err == nil {
		return frame
	}
	t.Fatalf("next stream frame: %v", err)
	return protocol.StreamFrame{}
}

func subscribeStream(t testing.TB, store *runtime.StreamReplayBuffer, scope runtime.StreamCursorScope, after uint64, options runtime.StreamReplaySubscriptionOptions) (*runtime.StreamReplaySubscription, error) {
	t.Helper()
	cursor := ""
	if after != 0 {
		codec, err := runtime.NewStreamCursorCodec(runtime.StreamCursorCodecConfig{
			ActiveKeyID: "test", Keys: map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, TTL: time.Hour,
		})
		if err != nil {
			t.Fatalf("cursor codec: %v", err)
		}
		cursor, err = codec.Encode(scope, after)
		if err != nil {
			t.Fatalf("cursor encode: %v", err)
		}
		options.CursorCodec = codec
	}
	return store.Subscribe(context.Background(), scope, cursor, options)
}

func streamReplayOptions(firstSequence uint64, limits runtime.StreamReplayLimits) runtime.StreamReplaySubscriptionOptions {
	executor, _ := runtime.NewStreamCheckExecutor(4)
	return runtime.StreamReplaySubscriptionOptions{
		Limits: limits, FirstSequence: firstSequence, ProfileVersion: protocol.StreamProfileVersion,
		ReauthorizeSession:      func(context.Context) error { return nil },
		Reauthorize:             func(context.Context, protocol.StreamFrame) error { return nil },
		ValidateSchema:          func(context.Context, string) error { return nil },
		CheckExecutor:           executor,
		AuthenticationExpiresAt: time.Now().Add(time.Hour),
	}
}
