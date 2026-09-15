package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

func TestSubscriptionHandleBindsAuthorityAndRevisions(t *testing.T) {
	coordinator, _, authorization, prepareCalls := subscriptionHandleFixture(t)
	owner := subscriptionPrincipal("owner", "tenant")
	request := SubscriptionHandleRequest{
		Request: &protocol.Request{}, IdempotencyKey: "establish-1", Fingerprint: "sha256:request-1",
		RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer, SubscriptionSameOriginCookie},
	}
	first, err := coordinator.Establish(owner, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.Establish(owner, request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.Handle.ID != second.Handle.ID || !slices.Equal(first.Snapshot, second.Snapshot) || first.ReconciliationCursor != second.ReconciliationCursor || prepareCalls.Load() != 1 {
		t.Fatalf("idempotent establishment = %#v / %#v, prepares %d", first, second, prepareCalls.Load())
	}
	conflict := request
	conflict.Fingerprint = "sha256:different"
	if _, err := coordinator.Establish(owner, conflict); !errors.Is(err, ErrSubscriptionIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	for _, identity := range []context.Context{subscriptionPrincipal("other", "tenant"), subscriptionPrincipal("owner", "other-tenant")} {
		if _, err := coordinator.Observe(identity, first.Handle.ID); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
			t.Fatalf("foreign observe = %v", err)
		}
		if _, err := coordinator.Open(identity, first.Handle.ID, ""); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
			t.Fatalf("foreign open = %v", err)
		}
		if _, err := coordinator.Renew(identity, first.Handle.ID); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
			t.Fatalf("foreign renew = %v", err)
		}
		if err := coordinator.Cancel(identity, first.Handle.ID); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
			t.Fatalf("foreign cancel = %v", err)
		}
	}
	authorization.mu.Lock()
	authorization.revision = "authorization-r2"
	authorization.mu.Unlock()
	if _, err := coordinator.Open(owner, first.Handle.ID, ""); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
		t.Fatalf("authorization revision drift = %v", err)
	}
	authorization.mu.Lock()
	authorization.revision = "authorization-r1"
	authorization.schema = "schema-r2"
	authorization.mu.Unlock()
	if _, err := coordinator.Renew(owner, first.Handle.ID); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
		t.Fatalf("schema revision drift = %v", err)
	}
}

func TestSubscriptionHandleSnapshotReplayLiveHandoffAndTerminal(t *testing.T) {
	coordinator, broker, _, _ := subscriptionHandleFixture(t)
	owner := subscriptionPrincipal("owner", "tenant")
	established, err := coordinator.Establish(owner, SubscriptionHandleRequest{
		Request: &protocol.Request{}, Fingerprint: "sha256:handoff",
		RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionSameOriginCookie},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"count"}, Data: json.RawMessage(`2`)}); err != nil {
		t.Fatal(err)
	}
	source, err := coordinator.Open(owner, established.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, source, protocol.StreamOpen, 1, 0)
	assertSubscriptionFrame(t, source, protocol.StreamResume, 2, 0)
	assertSubscriptionFrame(t, source, protocol.StreamPatch, 3, 2)
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"count"}, Data: json.RawMessage(`3`)}); err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, source, protocol.StreamPatch, 4, 3)
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamComplete}); err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, source, protocol.StreamComplete, 5, 0)
	if _, err := source.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after terminal = %v", err)
	}
	coordinator.mu.Lock()
	attachments := coordinator.attachments
	coordinator.mu.Unlock()
	if attachments != 0 {
		t.Fatalf("attachments after terminal = %d", attachments)
	}
}

func TestSubscriptionHandleExpiryCancellationAndDrainReleaseSources(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	coordinator, broker, authorization, _ := subscriptionHandleFixtureAt(t, &now)
	owner := subscriptionPrincipal("owner", "tenant")
	establish := func(fingerprint string) SubscriptionHandleEstablishment {
		result, err := coordinator.Establish(owner, SubscriptionHandleRequest{Request: &protocol.Request{}, Fingerprint: fingerprint, RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := establish("sha256:first")
	source, err := coordinator.Open(owner, first.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Cancel(owner, first.Handle.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Next(context.Background()); err == nil {
		t.Fatal("cancelled source delivered after cancellation")
	}
	if _, err := coordinator.Observe(owner, first.Handle.ID); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
		t.Fatalf("cancelled observe = %v", err)
	}
	second := establish("sha256:second")
	secondSource, err := coordinator.Open(owner, second.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	now = second.Handle.ExpiresAt
	authorization.mu.Lock()
	authorization.expires = now.Add(time.Hour)
	authorization.mu.Unlock()
	if _, err := coordinator.Observe(owner, second.Handle.ID); !errors.Is(err, ErrSubscriptionHandleUnavailable) {
		t.Fatalf("expired observe = %v", err)
	}
	if _, err := secondSource.Next(context.Background()); err == nil {
		t.Fatal("expired source delivered after expiry")
	}
	third := establish("sha256:third")
	thirdSource, err := coordinator.Open(owner, third.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := coordinator.Drain(context.Background()); got != 1 {
		t.Fatalf("drained handles = %d", got)
	}
	if _, err := thirdSource.Next(context.Background()); err == nil {
		t.Fatal("drained source delivered after drain")
	}
	broker.mu.Lock()
	retained := len(broker.records)
	broker.mu.Unlock()
	if retained != 0 {
		t.Fatalf("broker records after lifecycle cleanup = %d", retained)
	}
}

func TestSubscriptionHandleRevocationStopsBufferedProtectedDelivery(t *testing.T) {
	coordinator, broker, authorization, _ := subscriptionHandleFixture(t)
	owner := subscriptionPrincipal("owner", "tenant")
	established, err := coordinator.Establish(owner, SubscriptionHandleRequest{
		Request: &protocol.Request{}, Fingerprint: "sha256:revocation",
		RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := coordinator.Open(owner, established.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, source, protocol.StreamOpen, 1, 0)
	assertSubscriptionFrame(t, source, protocol.StreamResume, 2, 0)
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"count"}, Data: json.RawMessage(`2`)}); err != nil {
		t.Fatal(err)
	}
	authorization.mu.Lock()
	authorization.allowed = false
	authorization.mu.Unlock()
	if _, err := source.Next(context.Background()); !errors.Is(err, ErrStreamAuthorizationRevoked) {
		t.Fatalf("buffered delivery after revocation = %v", err)
	}
	coordinator.mu.Lock()
	attachments := coordinator.attachments
	coordinator.mu.Unlock()
	if attachments != 0 {
		t.Fatalf("attachments after revocation = %d", attachments)
	}
}

func TestSubscriptionHandleSchemaDriftStopsBufferedProtectedDelivery(t *testing.T) {
	coordinator, broker, authorization, _ := subscriptionHandleFixture(t)
	owner := subscriptionPrincipal("owner", "tenant")
	established, err := coordinator.Establish(owner, SubscriptionHandleRequest{
		Request: &protocol.Request{}, Fingerprint: "sha256:schema-drift",
		RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := coordinator.Open(owner, established.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, source, protocol.StreamOpen, 1, 0)
	assertSubscriptionFrame(t, source, protocol.StreamResume, 2, 0)
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"count"}, Data: json.RawMessage(`2`)}); err != nil {
		t.Fatal(err)
	}
	authorization.mu.Lock()
	authorization.schema = "schema-r2"
	authorization.mu.Unlock()
	if _, err := source.Next(context.Background()); !errors.Is(err, ErrStreamSchemaRetired) {
		t.Fatalf("buffered delivery after schema drift = %v", err)
	}
}

func TestSubscriptionHandleBoundsConcurrentEstablishment(t *testing.T) {
	coordinator, _, _, _ := subscriptionHandleFixture(t)
	coordinator.config.MaxHandles = 1
	prepare := coordinator.config.Prepare
	started := make(chan struct{})
	release := make(chan struct{})
	coordinator.config.Prepare = func(ctx context.Context, request SubscriptionHandleRequest) (SubscriptionHandlePreparation, error) {
		close(started)
		<-release
		return prepare(ctx, request)
	}
	owner := subscriptionPrincipal("owner", "tenant")
	firstResult := make(chan error, 1)
	go func() {
		_, err := coordinator.Establish(owner, SubscriptionHandleRequest{
			Request: &protocol.Request{}, Fingerprint: "sha256:bounded-first",
			RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer},
		})
		firstResult <- err
	}()
	<-started
	_, secondErr := coordinator.Establish(owner, SubscriptionHandleRequest{
		Request: &protocol.Request{}, Fingerprint: "sha256:bounded-second",
		RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer},
	})
	if !errors.Is(secondErr, ErrSubscriptionCapacity) {
		t.Fatalf("concurrent establishment beyond handle cap = %v", secondErr)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("admitted establishment = %v", err)
	}
}

func TestSubscriptionBrokerFidelityRequiresExplicitMercureSemantics(t *testing.T) {
	complete := ReportSubscriptionBrokerFidelity(SubscriptionBrokerCapabilities{
		Adapter: SubscriptionAdapterMercure,
		Frames: []protocol.StreamEventType{
			protocol.StreamOpen, protocol.StreamData, protocol.StreamPatch, protocol.StreamError,
			protocol.StreamComplete, protocol.StreamKeepalive, protocol.StreamResume, protocol.StreamHistoryUnavailable,
		},
		Replay: protocol.StreamReplayDurable, PrivateScopedDelivery: true, ReplayAuthorization: true,
		HistoryLossDetection: true, TerminalFrameRetention: true, DuplicateSuppression: true, BoundedRetry: true,
	})
	if !complete.Compatible || len(complete.Failures) != 0 {
		t.Fatalf("complete Mercure fidelity = %#v", complete)
	}
	incomplete := ReportSubscriptionBrokerFidelity(SubscriptionBrokerCapabilities{
		Adapter: SubscriptionAdapterMercure,
		Frames:  []protocol.StreamEventType{protocol.StreamData, protocol.StreamPatch},
		Replay:  protocol.StreamReplayBounded, PrivateScopedDelivery: true,
	})
	if incomplete.Compatible || len(incomplete.Failures) == 0 {
		t.Fatalf("incomplete Mercure fidelity = %#v", incomplete)
	}
	encoded, err := json.Marshal(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"bearer", "cursor.", "raw-variable", "operation document"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("fidelity report leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestSubscriptionBrokerTerminalHandleDoesNotBlockActivePeer(t *testing.T) {
	coordinator, broker, _, _ := subscriptionHandleFixture(t)
	owner := subscriptionPrincipal("owner", "tenant")
	establish := func(fingerprint string) SubscriptionHandleEstablishment {
		result, err := coordinator.Establish(owner, SubscriptionHandleRequest{
			Request: &protocol.Request{}, Fingerprint: fingerprint,
			RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := establish("sha256:first-terminal")
	firstSource, err := coordinator.Open(owner, first.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamComplete}); err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, firstSource, protocol.StreamOpen, 1, 0)
	assertSubscriptionFrame(t, firstSource, protocol.StreamResume, 2, 0)
	assertSubscriptionFrame(t, firstSource, protocol.StreamComplete, 3, 0)

	second := establish("sha256:second-active")
	secondSource, err := coordinator.Open(owner, second.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish("operation:orders", "variables:none", protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"count"}, Data: json.RawMessage(`2`)}); err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, secondSource, protocol.StreamOpen, 1, 0)
	assertSubscriptionFrame(t, secondSource, protocol.StreamResume, 2, 0)
	assertSubscriptionFrame(t, secondSource, protocol.StreamPatch, 3, 2)
}

func TestSubscriptionBrokerClassifiesDuplicateEventIDs(t *testing.T) {
	coordinator, broker, _, _ := subscriptionHandleFixture(t)
	owner := subscriptionPrincipal("owner", "tenant")
	established, err := coordinator.Establish(owner, SubscriptionHandleRequest{
		Request: &protocol.Request{}, Fingerprint: "sha256:duplicates",
		RequestedAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := protocol.StreamFrame{Type: protocol.StreamPatch, EventID: "upstream-1", Path: []any{"count"}, Data: json.RawMessage(`2`)}
	if err := broker.Publish("operation:orders", "variables:none", frame); err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish("operation:orders", "variables:none", frame); err != nil {
		t.Fatalf("idempotent duplicate = %v", err)
	}
	conflict := frame
	conflict.Data = json.RawMessage(`3`)
	if err := broker.Publish("operation:orders", "variables:none", conflict); !errors.Is(err, protocol.ErrStreamDuplicateConflict) {
		t.Fatalf("conflicting duplicate = %v", err)
	}
	source, err := coordinator.Open(owner, established.Handle.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSubscriptionFrame(t, source, protocol.StreamOpen, 1, 0)
	assertSubscriptionFrame(t, source, protocol.StreamResume, 2, 0)
	assertSubscriptionFrame(t, source, protocol.StreamPatch, 3, 2)
}

type subscriptionAuthorizationFixture struct {
	mu       sync.Mutex
	allowed  bool
	schema   string
	revision string
	expires  time.Time
}

func (a *subscriptionAuthorizationFixture) authorize(_ context.Context, _ SubscriptionHandleAuthorizationRequest) (SubscriptionHandleAuthorizationDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return SubscriptionHandleAuthorizationDecision{Allowed: a.allowed, SchemaRevision: a.schema, AuthorizationRevision: a.revision, AuthenticationExpiresAt: a.expires}, nil
}

func subscriptionHandleFixture(t *testing.T) (*SubscriptionHandleCoordinator, *MemorySubscriptionBroker, *subscriptionAuthorizationFixture, *atomic.Int32) {
	now := time.Now()
	return subscriptionHandleFixtureAt(t, &now)
}

func subscriptionHandleFixtureAt(t *testing.T, now *time.Time) (*SubscriptionHandleCoordinator, *MemorySubscriptionBroker, *subscriptionAuthorizationFixture, *atomic.Int32) {
	t.Helper()
	codec, err := NewStreamCursorCodec(StreamCursorCodecConfig{
		ActiveKeyID: "key-1", Keys: map[string][]byte{"key-1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := SubscriptionDeliveryProfile{
		Name: "in-process-v1", MediaType: "text/event-stream", Replay: protocol.StreamReplayBounded,
		RetentionHorizon: time.Minute, LossDetection: true, TerminalFrames: true,
		MaxConnectionLifetime: 30 * time.Minute, ReconnectStagger: 5 * time.Minute, MaxReconnectAttempts: 5,
		BrowserAuthentication: []SubscriptionBrowserAuthentication{SubscriptionFetchBearer, SubscriptionSameOriginCookie},
	}
	broker, err := NewMemorySubscriptionBroker(MemorySubscriptionBrokerConfig{
		Profile: profile, CursorCodec: codec, Snapshot: func(context.Context, SubscriptionHandleBinding) (json.RawMessage, error) {
			return json.RawMessage(`{"count":1}`), nil
		},
		MaxSubscriptions: 8, MaxHistoryEvents: 8, MaxHistoryBytes: 16 << 10, MaxTotalHistoryBytes: 64 << 10,
		SubscriberQueue: 8, RetentionDuration: time.Minute, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewStreamCheckExecutor(4)
	if err != nil {
		t.Fatal(err)
	}
	authorization := &subscriptionAuthorizationFixture{allowed: true, schema: "schema-r1", revision: "authorization-r1", expires: now.Add(time.Hour)}
	var prepareCalls atomic.Int32
	var nextID atomic.Int32
	coordinator, err := NewSubscriptionHandleCoordinator(SubscriptionHandleConfig{
		Broker: broker, CheckExecutor: executor, HandleTTL: 20 * time.Minute, MaxHandles: 8, MaxAttachments: 8,
		MaxTotalSnapshotBytes: 64 << 10, GarbageCollectionBatch: 8, Now: func() time.Time { return *now },
		NewHandleID: func() (string, error) { return "handle-" + string(rune('a'+nextID.Add(1)-1)), nil },
		Prepare: func(_ context.Context, request SubscriptionHandleRequest) (SubscriptionHandlePreparation, error) {
			prepareCalls.Add(1)
			if request.Request == nil {
				return SubscriptionHandlePreparation{}, errors.New("missing request")
			}
			return SubscriptionHandlePreparation{
				OperationIdentity: "operation:orders", VariableIdentity: "variables:none", SchemaRevision: "schema-r1", AuthorizationRevision: "authorization-r1",
				AuthenticationExpiresAt: now.Add(time.Hour), Limits: SubscriptionHandleLimits{Session: DefaultStreamSessionLimits(), MaxSnapshotBytes: 8 << 10},
			}, nil
		},
		Authorizer: SubscriptionHandleAuthorizerFunc(authorization.authorize),
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, broker, authorization, &prepareCalls
}

func subscriptionPrincipal(subject, tenant string) context.Context {
	return WithPrincipal(context.Background(), Principal{Subject: subject, Tenant: tenant, AuthorizationRevision: "authorization-r1"})
}

func assertSubscriptionFrame(t *testing.T, source StreamSource, kind protocol.StreamEventType, sequence, position uint64) {
	t.Helper()
	frame, err := source.Next(context.Background())
	if err != nil || frame.Type != kind || frame.Sequence != sequence || (position != 0 && (!frame.HasPosition || frame.Position != position)) {
		t.Fatalf("frame = %#v/%v, want %s sequence %d position %d", frame, err, kind, sequence, position)
	}
}
