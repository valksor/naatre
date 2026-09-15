package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

func TestStreamCursorRejectsSignedTrailingContentAndSubsecondTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := []byte("0123456789abcdef0123456789abcdef")
	if _, err := NewStreamCursorCodec(StreamCursorCodecConfig{ActiveKeyID: "key", Keys: map[string][]byte{"key": key}, TTL: 500 * time.Millisecond}); !errors.Is(err, ErrInvalidStreamCursor) {
		t.Fatalf("subsecond TTL = %v", err)
	}
	codec, err := NewStreamCursorCodec(StreamCursorCodecConfig{
		ActiveKeyID: "key", Keys: map[string][]byte{"key": key}, TTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := internalStreamScope(t, "s")
	cursor, err := codec.Encode(scope, 1)
	if err != nil {
		t.Fatal(err)
	}
	payloadPart, _, _ := strings.Cut(cursor, ".")
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte(" true")...)
	forged := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(cursorSignature(key, payload))
	if _, err := codec.Decode(forged, scope); !errors.Is(err, ErrStreamHistoryUnavailable) {
		t.Fatalf("signed trailing cursor = %v", err)
	}
}

func TestStreamReplayConcurrentNextCancellationDetachesAndCancelsOwner(t *testing.T) {
	store, err := NewStreamReplayBuffer(StreamReplayConfig{MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := internalStreamScope(t, "s")
	subscription, err := store.Subscribe(context.Background(), scope, "", internalReplayOptions(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	ownerResult := make(chan error, 1)
	go func() {
		_, nextErr := subscription.Next(context.Background())
		ownerResult <- nextErr
	}()
	deadline := time.Now().Add(time.Second)
	for len(subscription.nextGate) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(subscription.nextGate) != 1 {
		t.Fatal("first Next did not acquire the delivery gate")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := subscription.Next(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v", err)
	}
	if err := <-ownerResult; !errors.Is(err, io.EOF) {
		t.Fatalf("owner after concurrent cancellation = %v", err)
	}
	store.mu.Lock()
	subscribers := store.totalSubscribers
	store.mu.Unlock()
	if subscribers != 0 {
		t.Fatalf("retained subscribers = %d", subscribers)
	}
}

func TestStreamReplayRetentionExpiresHistoryWithoutDisconnectingLive(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, err := NewStreamReplayBuffer(StreamReplayConfig{
		MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 4, MaxStreams: 2,
		RetentionDuration: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	scopeA := internalStreamScope(t, "a")
	scopeB := internalStreamScope(t, "b")
	subscription, err := store.Subscribe(context.Background(), scopeA, "", internalReplayOptions(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	for position := uint64(1); position <= 2; position++ {
		if err := store.Publish(scopeA, internalDataFrame("a", position)); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Minute)
	if err := store.Publish(scopeB, internalDataFrame("b", 1)); err != nil {
		t.Fatal(err)
	}
	options := internalReplayOptions(t, 1)
	cursor := internalResumeCursor(t, scopeA, 1, &options)
	if _, err := store.Subscribe(context.Background(), scopeA, cursor, options); !errors.Is(err, ErrStreamHistoryUnavailable) {
		t.Fatalf("expired replay cursor = %v", err)
	}
	if err := store.Publish(scopeA, internalDataFrame("a", 3)); err != nil {
		t.Fatal(err)
	}
	for want := uint64(1); want <= 3; want++ {
		frame, err := subscription.Next(context.Background())
		if err != nil || frame.Position != want {
			t.Fatalf("quiet live frame = %#v/%v, want position %d", frame, err, want)
		}
	}
	subscription.Close()

	slotNow := time.Unix(1_900_000_000, 0)
	slotStore, err := NewStreamReplayBuffer(StreamReplayConfig{
		MaxHistoryEvents: 1, MaxHistoryBytes: 4096, SubscriberQueue: 1, MaxStreams: 1,
		RetentionDuration: time.Minute, Now: func() time.Time { return slotNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := slotStore.Publish(scopeA, internalDataFrame("a", 1)); err != nil {
		t.Fatal(err)
	}
	slotNow = slotNow.Add(time.Minute)
	if err := slotStore.Publish(scopeB, internalDataFrame("b", 1)); err != nil {
		t.Fatalf("expired stream slot was not reclaimed: %v", err)
	}
}

func TestStreamReplayPressureDetachesOnlyRetentionContributor(t *testing.T) {
	scopeA := internalStreamScope(t, "a")
	scopeB := internalStreamScope(t, "b")
	firstABytes := internalFrameBytes(t, internalDataFrame("a", 1))
	secondABytes := internalFrameBytes(t, internalDataFrame("a", 2))
	firstBBytes := internalFrameBytes(t, internalDataFrame("b", 1))
	store, err := NewStreamReplayBuffer(StreamReplayConfig{
		MaxHistoryEvents: 2, MaxHistoryBytes: 4096, SubscriberQueue: 2,
		MaxStreams: 2, MaxTotalHistoryBytes: firstABytes + secondABytes + firstBBytes, MaxSubscribers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for position := uint64(1); position <= 2; position++ {
		if err := store.Publish(scopeA, internalDataFrame("a", position)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Publish(scopeB, internalDataFrame("b", 1)); err != nil {
		t.Fatal(err)
	}
	lagging, err := store.Subscribe(context.Background(), scopeA, "", internalReplayOptions(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := store.Subscribe(context.Background(), scopeB, "", internalReplayOptions(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(scopeA, internalDataFrame("a", 3)); err != nil {
		t.Fatalf("publish under subscriber retention pressure = %v", err)
	}
	if _, err := lagging.Next(context.Background()); !errors.Is(err, ErrStreamSlowConsumer) {
		t.Fatalf("lagging outcome = %v", err)
	}
	frame, err := healthy.Next(context.Background())
	if err != nil || frame.Position != 1 {
		t.Fatalf("unrelated subscriber = %#v/%v", frame, err)
	}
	healthy.Close()

	tiny, err := NewStreamReplayBuffer(StreamReplayConfig{
		MaxHistoryEvents: 1, MaxHistoryBytes: 4096, SubscriberQueue: 1, MaxTotalHistoryBytes: firstABytes - 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := tiny.Subscribe(context.Background(), scopeA, "", internalReplayOptions(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := tiny.Publish(scopeA, internalDataFrame("a", 1)); !errors.Is(err, ErrStreamReplayLimit) {
		t.Fatalf("oversized aggregate publish = %v", err)
	}
	subscription.mu.Lock()
	slow := subscription.slow
	subscription.mu.Unlock()
	if slow {
		t.Fatal("impossible publish detached subscriber")
	}
	subscription.Close()
}

func TestStreamReplayPressurePreservesEstablishedSubscriberDuringPreauthorization(t *testing.T) {
	scope := internalStreamScope(t, "s")
	frameSize := internalFrameBytes(t, internalDataFrame("s", 1))
	store, err := NewStreamReplayBuffer(StreamReplayConfig{
		MaxHistoryEvents: 1, MaxHistoryBytes: 4096, SubscriberQueue: 2,
		MaxTotalHistoryBytes: frameSize, MaxSubscribers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(scope, internalDataFrame("s", 1)); err != nil {
		t.Fatal(err)
	}
	healthy, err := store.Subscribe(context.Background(), scope, "", internalReplayOptions(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()

	started := make(chan struct{})
	establishingOptions := internalReplayOptions(t, 1)
	establishingOptions.ReauthorizeSession = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	establishment := make(chan error, 1)
	go func() {
		_, subscribeErr := store.Subscribe(context.Background(), scope, "", establishingOptions)
		establishment <- subscribeErr
	}()
	<-started
	if err := store.Publish(scope, internalDataFrame("s", 2)); !errors.Is(err, ErrStreamReplayLimit) {
		t.Fatalf("publish with temporary authorization retention = %v", err)
	}
	if err := <-establishment; !errors.Is(err, ErrStreamHistoryUnavailable) {
		t.Fatalf("cancelled establishment = %v", err)
	}
	frame, err := healthy.Next(context.Background())
	if err != nil || frame.Position != 1 {
		t.Fatalf("established subscriber after pressure = %#v/%v", frame, err)
	}
}

func TestStreamReplayRetainsCandidateBytesDuringAuthorization(t *testing.T) {
	scope := internalStreamScope(t, "s")
	frameSize := internalFrameBytes(t, internalDataFrame("s", 1))
	store, err := NewStreamReplayBuffer(StreamReplayConfig{
		MaxHistoryEvents: 1, MaxHistoryBytes: 4096, SubscriberQueue: 2,
		MaxTotalHistoryBytes: frameSize, MaxSubscribers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(scope, internalDataFrame("s", 1)); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	options := internalReplayOptions(t, 1)
	options.Reauthorize = func(context.Context, protocol.StreamFrame) error {
		close(started)
		<-release
		return nil
	}
	establishment := make(chan error, 1)
	go func() {
		subscription, subscribeErr := store.Subscribe(context.Background(), scope, "", options)
		if subscription != nil {
			subscription.Close()
		}
		establishment <- subscribeErr
	}()
	<-started
	if err := store.Publish(scope, internalDataFrame("s", 2)); !errors.Is(err, ErrStreamReplayLimit) {
		t.Fatalf("publish while candidate authorization retains bytes = %v", err)
	}
	close(release)
	if err := <-establishment; !errors.Is(err, ErrStreamHistoryUnavailable) {
		t.Fatalf("bounded establishment under retention pressure = %v", err)
	}
}

func TestStreamReplayAuthenticationAndSessionBudgetsAreAbsolute(t *testing.T) {
	store, err := NewStreamReplayBuffer(StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	scope := internalStreamScope(t, "s")
	past := internalReplayOptions(t, 1)
	past.AuthenticationExpiresAt = time.Now().Add(-time.Second)
	if _, err := store.Subscribe(context.Background(), scope, "", past); !errors.Is(err, ErrStreamAuthenticationExpired) {
		t.Fatalf("past replay authentication = %v", err)
	}
	future := internalReplayOptions(t, 1)
	future.AuthenticationExpiresAt = time.Now().Add(20 * time.Millisecond)
	subscription, err := store.Subscribe(context.Background(), scope, "", future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, ErrStreamAuthenticationExpired) {
		t.Fatalf("replay authentication deadline = %v", err)
	}

	for position := uint64(1); position <= 2; position++ {
		if err := store.Publish(scope, internalDataFrame("s", position)); err != nil {
			t.Fatal(err)
		}
	}
	limited := internalReplayOptions(t, 1)
	limited.Session.MaxEvents = 1
	if _, err := store.Subscribe(context.Background(), scope, "", limited); !errors.Is(err, ErrStreamSessionLimit) {
		t.Fatalf("initial session budget = %v", err)
	}

	liveStore, err := NewStreamReplayBuffer(StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 4096, SubscriberQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	live := internalReplayOptions(t, 1)
	live.Session.MaxEvents = 1
	liveSubscription, err := liveStore.Subscribe(context.Background(), scope, "", live)
	if err != nil {
		t.Fatal(err)
	}
	if err := liveStore.Publish(scope, internalDataFrame("s", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := liveSubscription.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := liveStore.Publish(scope, internalDataFrame("s", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := liveSubscription.Next(context.Background()); !errors.Is(err, ErrStreamSessionLimit) {
		t.Fatalf("live session budget = %v", err)
	}
}

func internalStreamScope(t testing.TB, stream string) StreamCursorScope {
	t.Helper()
	scope, err := NewStreamCursorScope(StreamCursorScopeOptions{
		Stream: stream, Tenant: "tenant", Principal: "principal", AuthorizationRevision: "auth", SchemaRevision: "schema",
	})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func internalReplayOptions(t testing.TB, first uint64) StreamReplaySubscriptionOptions {
	t.Helper()
	executor, err := NewStreamCheckExecutor(4)
	if err != nil {
		t.Fatal(err)
	}
	return StreamReplaySubscriptionOptions{
		Limits: StreamReplayLimits{MaxEvents: 16, MaxBytes: 8192}, FirstSequence: first,
		ProfileVersion: protocol.StreamProfileVersion, AuthenticationExpiresAt: time.Now().Add(time.Hour), CheckExecutor: executor,
		ReauthorizeSession: func(context.Context) error { return nil },
		Reauthorize:        func(context.Context, protocol.StreamFrame) error { return nil },
		ValidateSchema:     func(context.Context, string) error { return nil },
	}
}

func internalResumeCursor(t testing.TB, scope StreamCursorScope, position uint64, options *StreamReplaySubscriptionOptions) string {
	t.Helper()
	codec, err := NewStreamCursorCodec(StreamCursorCodecConfig{
		ActiveKeyID: "key", Keys: map[string][]byte{"key": []byte("0123456789abcdef0123456789abcdef")}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := codec.Encode(scope, position)
	if err != nil {
		t.Fatal(err)
	}
	options.CursorCodec = codec
	return cursor
}

func internalDataFrame(stream string, position uint64) protocol.StreamFrame {
	return protocol.StreamFrame{
		Type: protocol.StreamData, Stream: stream, Sequence: position + 1,
		Position: position, HasPosition: true, Data: []byte(`true`),
	}
}

func internalFrameBytes(t testing.TB, frame protocol.StreamFrame) uint64 {
	t.Helper()
	encoded, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	return uint64(len(encoded))
}
