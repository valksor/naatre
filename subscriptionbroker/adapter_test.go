package subscriptionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
)

type fixtureDriver struct {
	mu             sync.Mutex
	establish      EstablishResult
	establishErr   error
	attach         AttachResult
	attachErr      error
	revokeErr      error
	revoked        bool
	snapshotCalls  int
	establishLimit Limits
	attachLimit    Limits
}

type incompleteDriver struct{ fixtureDriver }

func (*incompleteDriver) Capabilities() DriverCapabilities { return DriverCapabilities{} }

func (*fixtureDriver) PostgreSQLAtomicDriver()    {}
func (*fixtureDriver) RedisStreamsAtomicDriver()  {}
func (*fixtureDriver) NATSJetStreamAtomicDriver() {}

func (*fixtureDriver) Capabilities() DriverCapabilities {
	return DriverCapabilities{
		AtomicSnapshotPosition: true, AtomicReplayLiveAttach: true, BoundedReplay: true, Revocation: true,
		HistoryLossDetection: true, SlowConsumerLimit: true, TerminalRetention: true, DuplicateClassification: true,
	}
}

func (d *fixtureDriver) Establish(ctx context.Context, request EstablishRequest) (EstablishResult, error) {
	d.mu.Lock()
	d.establishLimit = request.Limits
	d.mu.Unlock()
	if d.establishErr != nil {
		return EstablishResult{}, d.establishErr
	}
	snapshot, err := request.Snapshot(ctx, request.Binding)
	if err != nil {
		return EstablishResult{}, err
	}
	d.mu.Lock()
	d.snapshotCalls++
	d.mu.Unlock()
	result := d.establish
	result.Snapshot = snapshot
	return result, nil
}

func (d *fixtureDriver) Attach(_ context.Context, request AttachRequest) (AttachResult, error) {
	d.mu.Lock()
	d.attachLimit = request.Limits
	d.mu.Unlock()
	return d.attach, d.attachErr
}

func (d *fixtureDriver) Revoke(context.Context, naatreruntime.SubscriptionHandleBinding) error {
	d.mu.Lock()
	d.revoked = true
	if source, ok := d.attach.Source.(*fixtureSource); ok {
		source.mu.Lock()
		source.err = ErrDriverRevoked
		source.mu.Unlock()
	}
	d.mu.Unlock()
	return d.revokeErr
}

type fixtureSource struct {
	mu        sync.Mutex
	items     []DriverItem
	err       error
	closed    chan struct{}
	started   chan struct{}
	once      sync.Once
	startOnce sync.Once
	block     bool
}

func (s *fixtureSource) Next(ctx context.Context) (DriverItem, error) {
	if s.block {
		s.startOnce.Do(func() {
			if s.started != nil {
				close(s.started)
			}
		})
		select {
		case <-ctx.Done():
			return DriverItem{}, context.Cause(ctx)
		case <-s.closed:
			return DriverItem{}, io.EOF
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		if s.err != nil {
			return DriverItem{}, s.err
		}
		return DriverItem{}, io.EOF
	}
	item := s.items[0]
	s.items = s.items[1:]
	return item, nil
}

func (s *fixtureSource) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type brokerFactory struct {
	name string
	new  func(*fixtureDriver, Config) (naatreruntime.SubscriptionBroker, naatreruntime.SubscriptionBrokerFidelityReport, error)
}

func brokerFactories() []brokerFactory {
	return []brokerFactory{
		{name: "postgresql", new: func(driver *fixtureDriver, config Config) (naatreruntime.SubscriptionBroker, naatreruntime.SubscriptionBrokerFidelityReport, error) {
			adapter, err := NewPostgreSQL(driver, config)
			if err != nil {
				return nil, naatreruntime.SubscriptionBrokerFidelityReport{}, err
			}
			return adapter, adapter.FidelityReport(), nil
		}},
		{name: "redis-streams", new: func(driver *fixtureDriver, config Config) (naatreruntime.SubscriptionBroker, naatreruntime.SubscriptionBrokerFidelityReport, error) {
			adapter, err := NewRedisStreams(driver, config)
			if err != nil {
				return nil, naatreruntime.SubscriptionBrokerFidelityReport{}, err
			}
			return adapter, adapter.FidelityReport(), nil
		}},
		{name: "nats-jetstream", new: func(driver *fixtureDriver, config Config) (naatreruntime.SubscriptionBroker, naatreruntime.SubscriptionBrokerFidelityReport, error) {
			adapter, err := NewNATSJetStream(driver, config)
			if err != nil {
				return nil, naatreruntime.SubscriptionBrokerFidelityReport{}, err
			}
			return adapter, adapter.FidelityReport(), nil
		}},
	}
}

func TestAdvertisedAdaptersProveAtomicReplayToLiveHandoff(t *testing.T) {
	for _, factory := range brokerFactories() {
		t.Run(factory.name, func(t *testing.T) {
			driverSource := &fixtureSource{closed: make(chan struct{}), items: []DriverItem{
				{Frame: protocol.StreamFrame{Type: protocol.StreamData, Data: json.RawMessage(`{"value":2}`)}, Position: 2},
				{CaughtUp: true, Position: 2},
				{Frame: protocol.StreamFrame{Type: protocol.StreamPatch, Path: []any{"value"}, Data: json.RawMessage(`3`)}, Position: 3},
			}}
			driver := &fixtureDriver{establish: EstablishResult{Position: 1}, attach: AttachResult{Source: driverSource, EarliestPosition: 1, ReplayHighWater: 2}}
			broker, report, err := factory.new(driver, fixtureConfig(t, DefaultLimits()))
			if err != nil {
				t.Fatal(err)
			}
			if !report.Compatible || report.Adapter != naatreruntime.SubscriptionAdapterKind(factory.name) {
				t.Fatalf("fidelity = %#v", report)
			}
			snapshot, err := broker.Establish(context.Background(), fixtureBinding())
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Position != 1 || string(snapshot.Data) != `{"value":1}` || driver.snapshotCalls != 1 {
				t.Fatalf("snapshot = %#v, calls=%d", snapshot, driver.snapshotCalls)
			}
			source, err := broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor)
			if err != nil {
				t.Fatal(err)
			}
			wantTypes := []protocol.StreamEventType{protocol.StreamOpen, protocol.StreamResume, protocol.StreamData, protocol.StreamPatch}
			wantPositions := []uint64{0, 0, 2, 3}
			for index := range wantTypes {
				frame, nextErr := source.Next(context.Background())
				if nextErr != nil {
					t.Fatal(nextErr)
				}
				if frame.Type != wantTypes[index] || frame.Sequence != uint64(index+1) || frame.Position != wantPositions[index] {
					t.Fatalf("frame %d = %#v", index, frame)
				}
			}
			if driver.attachLimit.MaxPendingEvents != DefaultLimits().MaxPendingEvents {
				t.Fatalf("attach limits = %#v", driver.attachLimit)
			}
		})
	}
}

func TestAdvertisedAdaptersProveHistoryLossRevocationAndSlowConsumer(t *testing.T) {
	for _, factory := range brokerFactories() {
		t.Run(factory.name, func(t *testing.T) {
			driver := &fixtureDriver{establish: EstablishResult{Position: 1}, attachErr: errors.New("redis password=secret: " + ErrDriverHistoryLost.Error())}
			driver.attachErr = errors.Join(driver.attachErr, ErrDriverHistoryLost)
			broker, _, err := factory.new(driver, fixtureConfig(t, DefaultLimits()))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := broker.Establish(context.Background(), fixtureBinding())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor); !errors.Is(err, naatreruntime.ErrSubscriptionHistoryUnavailable) || ErrorCode(err) != CodeHistoryLost {
				t.Fatalf("history error = %v", err)
			}
			revokedSource := &fixtureSource{closed: make(chan struct{})}
			driver.attachErr = nil
			driver.attach = AttachResult{Source: revokedSource, EarliestPosition: 1, ReplayHighWater: 1}
			active, err := broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = active.Next(context.Background())
			_, _ = active.Next(context.Background())
			if err := broker.Revoke(context.Background(), fixtureBinding()); err != nil {
				t.Fatal(err)
			}
			if !driver.revoked {
				t.Fatal("revocation did not reach driver")
			}
			if _, err := active.Next(context.Background()); !errors.Is(err, naatreruntime.ErrSubscriptionHandleUnavailable) || ErrorCode(err) != CodeUnavailable {
				t.Fatalf("revoked source = %v", err)
			}

			slow := &fixtureSource{closed: make(chan struct{}), err: errors.Join(ErrDriverSlowConsumer, errors.New("nats://credential@protected"))}
			driver.attach = AttachResult{Source: slow, EarliestPosition: 1, ReplayHighWater: 1}
			source, err := broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = source.Next(context.Background())
			_, _ = source.Next(context.Background())
			if _, err := source.Next(context.Background()); !errors.Is(err, naatreruntime.ErrStreamSlowConsumer) || ErrorCode(err) != CodeSlowConsumer {
				t.Fatalf("slow consumer = %v", err)
			}
		})
	}
}

func TestAdvertisedAdaptersBoundReplayAndRejectMalformedHandoffs(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxReplayEvents = 1
	for _, factory := range brokerFactories() {
		t.Run(factory.name, func(t *testing.T) {
			driverSource := &fixtureSource{closed: make(chan struct{}), items: []DriverItem{
				{Frame: protocol.StreamFrame{Type: protocol.StreamData, Data: json.RawMessage(`1`)}, Position: 2},
				{Frame: protocol.StreamFrame{Type: protocol.StreamData, Data: json.RawMessage(`2`)}, Position: 3},
			}}
			driver := &fixtureDriver{establish: EstablishResult{Position: 1}, attach: AttachResult{Source: driverSource, EarliestPosition: 1, ReplayHighWater: 3}}
			broker, _, err := factory.new(driver, fixtureConfig(t, limits))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := broker.Establish(context.Background(), fixtureBinding())
			if err != nil {
				t.Fatal(err)
			}
			source, err := broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = source.Next(context.Background())
			_, _ = source.Next(context.Background())
			if _, err := source.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := source.Next(context.Background()); ErrorCode(err) != CodeResourceExhausted {
				t.Fatalf("replay bound = %v", err)
			}
		})
	}
}

func TestAdvertisedAdaptersCancellationAndCloseUnblock(t *testing.T) {
	for _, factory := range brokerFactories() {
		t.Run(factory.name, func(t *testing.T) {
			driverSource := &fixtureSource{closed: make(chan struct{}), started: make(chan struct{}), block: true}
			driver := &fixtureDriver{establish: EstablishResult{Position: 1}, attach: AttachResult{Source: driverSource, EarliestPosition: 1, ReplayHighWater: 1}}
			broker, _, err := factory.new(driver, fixtureConfig(t, DefaultLimits()))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, _ := broker.Establish(context.Background(), fixtureBinding())
			source, _ := broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor)
			_, _ = source.Next(context.Background())
			_, _ = source.Next(context.Background())
			result := make(chan error, 1)
			go func() { _, nextErr := source.Next(context.Background()); result <- nextErr }()
			<-driverSource.started
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("close result = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not unblock Next")
			}

			cancelSource := &fixtureSource{closed: make(chan struct{}), block: true}
			driver.attach = AttachResult{Source: cancelSource, EarliestPosition: 1, ReplayHighWater: 1}
			source, _ = broker.Open(context.Background(), fixtureBinding(), snapshot.Cursor)
			_, _ = source.Next(context.Background())
			_, _ = source.Next(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := source.Next(ctx); ErrorCode(err) != CodeCancelled {
				t.Fatalf("cancel error = %v", err)
			}
		})
	}
}

func TestPublicFailuresNeverLeakDriverMetadata(t *testing.T) {
	secrets := []string{"postgres://admin:secret@database/internal", "authorization-revision-private", "cursor.private", "payload-value"}
	for _, factory := range brokerFactories() {
		driver := &fixtureDriver{establishErr: errors.New(strings.Join(secrets, " "))}
		broker, _, err := factory.new(driver, fixtureConfig(t, DefaultLimits()))
		if err != nil {
			t.Fatal(err)
		}
		_, err = broker.Establish(context.Background(), fixtureBinding())
		encoded, marshalErr := json.Marshal(err)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) || strings.Contains(string(encoded), secret) {
				t.Fatalf("%s leaked %q: %s", factory.name, secret, encoded)
			}
		}
		if ErrorCode(err) != CodeUnavailable {
			t.Fatalf("code = %q", ErrorCode(err))
		}
	}
}

func TestAdapterRejectsIncompleteDriverCapabilities(t *testing.T) {
	driver := &incompleteDriver{}
	_, err := NewPostgreSQL(driver, fixtureConfig(t, DefaultLimits()))
	if ErrorCode(err) != CodeInvalidConfig || !errors.Is(err, naatreruntime.ErrSubscriptionCapabilityUnsupported) {
		t.Fatalf("capability error = %v", err)
	}
}

func TestRolloutStaggersReconnectsAndRollbackStopsCandidateSelection(t *testing.T) {
	stable := &stubBroker{name: "stable"}
	candidate := &stubBroker{name: "candidate"}
	rollout, err := NewRollout(RolloutConfig{
		Stable: stable, Candidate: candidate, Revision: "rollout-r1", CandidateBasisPoints: 10000,
		MaxConnectionLifetime: 10 * time.Minute, ReconnectStagger: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	established := time.Unix(1000, 0)
	first, err := rollout.Select("opaque-a", established, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := rollout.Select("opaque-b", established, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Lane != RolloutCandidate || second.Lane != RolloutCandidate || first.ReconnectDeadline.Equal(second.ReconnectDeadline) {
		t.Fatalf("selections = %#v / %#v", first, second)
	}
	if first.ReconnectDeadline.After(established.Add(10*time.Minute)) || first.ReconnectDeadline.Before(established.Add(8*time.Minute)) {
		t.Fatalf("deadline = %v", first.ReconnectDeadline)
	}
	rollout.Rollback()
	after, err := rollout.Select("opaque-a", established, established.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if after.Lane != RolloutStable || after.Broker != stable || !after.ReconnectDeadline.Equal(established.Add(time.Minute)) {
		t.Fatalf("rollback selection = %#v", after)
	}
	if err := rollout.Observe(RolloutHistoryLost); err != nil {
		t.Fatal(err)
	}
	evidence := rollout.Evidence()
	encoded, _ := json.Marshal(evidence)
	if !evidence.RolledBack || evidence.CandidateBasisPoints != 0 || evidence.Outcomes[RolloutHistoryLost] != 1 || strings.Contains(string(encoded), "opaque-a") {
		t.Fatalf("evidence = %s", encoded)
	}
}

type stubBroker struct{ name string }

func (s *stubBroker) DeliveryProfile() naatreruntime.SubscriptionDeliveryProfile {
	return naatreruntime.SubscriptionDeliveryProfile{Name: s.name}
}
func (*stubBroker) Establish(context.Context, naatreruntime.SubscriptionHandleBinding) (naatreruntime.SubscriptionBrokerSnapshot, error) {
	return naatreruntime.SubscriptionBrokerSnapshot{}, nil
}
func (*stubBroker) Open(context.Context, naatreruntime.SubscriptionHandleBinding, string) (naatreruntime.StreamSource, error) {
	return nil, nil
}
func (*stubBroker) Revoke(context.Context, naatreruntime.SubscriptionHandleBinding) error { return nil }

func fixtureConfig(t *testing.T, limits Limits) Config {
	t.Helper()
	now := time.Unix(1000, 0)
	codec, err := naatreruntime.NewStreamCursorCodec(naatreruntime.StreamCursorCodecConfig{
		ActiveKeyID: "key-1", Keys: map[string][]byte{"key-1": []byte("01234567890123456789012345678901")}, TTL: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Profile: naatreruntime.SubscriptionDeliveryProfile{
			Name: "broker", MediaType: "text/event-stream", Replay: protocol.StreamReplayDurable,
			RetentionHorizon: time.Hour, LossDetection: true, TerminalFrames: true,
			MaxConnectionLifetime: 10 * time.Minute, ReconnectStagger: time.Minute, MaxReconnectAttempts: 5,
			BrowserAuthentication: []naatreruntime.SubscriptionBrowserAuthentication{naatreruntime.SubscriptionFetchBearer},
		},
		CursorCodec: codec, Snapshot: func(context.Context, naatreruntime.SubscriptionHandleBinding) (json.RawMessage, error) {
			return json.RawMessage(`{"value":1}`), nil
		}, Limits: limits,
	}
}

func fixtureBinding() naatreruntime.SubscriptionHandleBinding {
	return naatreruntime.SubscriptionHandleBinding{
		HandleID: "handle-1", Principal: "principal-1", Tenant: "tenant-1", OperationIdentity: "operation-1",
		VariableIdentity: "variables-1", SchemaRevision: "schema-r1", AuthorizationRevision: "auth-r1",
	}
}
