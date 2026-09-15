package runtime

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
)

// SubscriptionAdapterKind is a bounded fidelity-report label.
type SubscriptionAdapterKind string

const (
	SubscriptionAdapterInProcess SubscriptionAdapterKind = "in-process"
	SubscriptionAdapterMercure   SubscriptionAdapterKind = "mercure"
)

// SubscriptionBrokerCapabilities declares semantic facts an adapter proves.
// A transport-specific topic or event identifier never replaces Naatre state.
type SubscriptionBrokerCapabilities struct {
	Adapter                SubscriptionAdapterKind
	Frames                 []protocol.StreamEventType
	Replay                 protocol.StreamReplayCapability
	PrivateScopedDelivery  bool
	ReplayAuthorization    bool
	HistoryLossDetection   bool
	TerminalFrameRetention bool
	DuplicateSuppression   bool
	BoundedRetry           bool
}

type SubscriptionFidelityFailure struct {
	Capability string `json:"capability"`
	Reason     string `json:"reason"`
}

type SubscriptionBrokerFidelityReport struct {
	Adapter    SubscriptionAdapterKind       `json:"adapter"`
	Compatible bool                          `json:"compatible"`
	Failures   []SubscriptionFidelityFailure `json:"failures"`
}

// ReportSubscriptionBrokerFidelity returns an explicit, bounded report. A
// Mercure adapter is compatible only when it uses private narrowly scoped
// topics while preserving Naatre authorization, replay, error, and terminal
// semantics independently of Mercure's wire model.
func ReportSubscriptionBrokerFidelity(capabilities SubscriptionBrokerCapabilities) SubscriptionBrokerFidelityReport {
	report := SubscriptionBrokerFidelityReport{Adapter: capabilities.Adapter, Compatible: true, Failures: []SubscriptionFidelityFailure{}}
	require := func(ok bool, capability, reason string) {
		if ok {
			return
		}
		report.Compatible = false
		report.Failures = append(report.Failures, SubscriptionFidelityFailure{Capability: capability, Reason: reason})
	}
	require(capabilities.Adapter == SubscriptionAdapterInProcess || capabilities.Adapter == SubscriptionAdapterMercure, "adapter", "unsupported adapter kind")
	wantedFrames := []protocol.StreamEventType{
		protocol.StreamOpen, protocol.StreamData, protocol.StreamPatch, protocol.StreamError,
		protocol.StreamComplete, protocol.StreamKeepalive, protocol.StreamResume, protocol.StreamHistoryUnavailable,
	}
	for _, frame := range wantedFrames {
		require(slices.Contains(capabilities.Frames, frame), "frame."+string(frame), "logical frame is not preserved")
	}
	require(capabilities.Replay == protocol.StreamReplayNone || capabilities.Replay == protocol.StreamReplayBounded || capabilities.Replay == protocol.StreamReplayDurable, "replay", "replay capability is not explicit")
	require(capabilities.PrivateScopedDelivery, "private-delivery", "delivery scope is not private and narrow")
	require(capabilities.ReplayAuthorization, "replay-authorization", "replay is not reauthorized")
	if capabilities.Replay != protocol.StreamReplayNone {
		require(capabilities.HistoryLossDetection, "history-loss", "history loss is not explicit")
	}
	require(capabilities.TerminalFrameRetention, "terminal", "terminal loss can be reported as completion")
	require(capabilities.DuplicateSuppression, "duplicates", "conflicting or duplicate delivery is not classified")
	require(capabilities.BoundedRetry, "retry", "retry behavior is not bounded")
	return report
}

type MemorySubscriptionSnapshotFunc func(context.Context, SubscriptionHandleBinding) (json.RawMessage, error)

type MemorySubscriptionBrokerConfig struct {
	Profile              SubscriptionDeliveryProfile
	CursorCodec          *StreamCursorCodec
	Snapshot             MemorySubscriptionSnapshotFunc
	MaxSubscriptions     int
	MaxHistoryEvents     int
	MaxHistoryBytes      uint64
	MaxTotalHistoryBytes uint64
	SubscriberQueue      int
	RetentionDuration    time.Duration
	Now                  func() time.Time
}

type memoryBrokerRecord struct {
	binding        SubscriptionHandleBinding
	scope          StreamCursorScope
	lastPosition   uint64
	evictedThrough uint64
	historyBytes   uint64
	history        []memoryBrokerFrame
	subscribers    map[*memorySubscriptionSource]struct{}
	terminal       bool
}

type memoryBrokerFrame struct {
	frame     protocol.StreamFrame
	position  uint64
	bytes     uint64
	expiresAt time.Time
}

// MemorySubscriptionBroker is the bounded in-process reference adapter. Its
// lock covers snapshot high-water capture, replay selection, and live consumer
// registration, which gives a gap-free replay-to-live handoff.
type MemorySubscriptionBroker struct {
	mu         sync.Mutex
	config     MemorySubscriptionBrokerConfig
	records    map[string]*memoryBrokerRecord
	topics     map[string]map[string]*memoryBrokerRecord
	duplicates map[string]*memoryBrokerDuplicateWindow
	totalBytes uint64
	now        func() time.Time
}

type memoryBrokerDuplicateWindow struct {
	entries map[string]memoryBrokerDuplicate
	order   []string
}

type memoryBrokerDuplicate struct {
	frame     protocol.StreamFrame
	expiresAt time.Time
}

func NewMemorySubscriptionBroker(config MemorySubscriptionBrokerConfig) (*MemorySubscriptionBroker, error) {
	if config.CursorCodec == nil || config.Snapshot == nil || validateSubscriptionDeliveryProfile(config.Profile) != nil {
		return nil, ErrInvalidSubscriptionHandleConfig
	}
	if config.MaxSubscriptions == 0 {
		config.MaxSubscriptions = 1024
	}
	if config.MaxHistoryEvents == 0 {
		config.MaxHistoryEvents = 1024
	}
	if config.MaxHistoryBytes == 0 {
		config.MaxHistoryBytes = 8 << 20
	}
	if config.MaxTotalHistoryBytes == 0 {
		config.MaxTotalHistoryBytes = 64 << 20
	}
	if config.SubscriberQueue == 0 {
		config.SubscriberQueue = 64
	}
	if config.RetentionDuration == 0 {
		config.RetentionDuration = config.Profile.RetentionHorizon
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxSubscriptions <= 0 || config.MaxHistoryEvents <= 0 || config.MaxHistoryBytes == 0 || config.MaxTotalHistoryBytes == 0 || config.SubscriberQueue <= 0 || config.RetentionDuration <= 0 {
		return nil, ErrInvalidSubscriptionHandleConfig
	}
	return &MemorySubscriptionBroker{
		config: config, records: make(map[string]*memoryBrokerRecord), topics: make(map[string]map[string]*memoryBrokerRecord),
		duplicates: make(map[string]*memoryBrokerDuplicateWindow), now: config.Now,
	}, nil
}

func (b *MemorySubscriptionBroker) DeliveryProfile() SubscriptionDeliveryProfile {
	if b == nil {
		return SubscriptionDeliveryProfile{}
	}
	return cloneSubscriptionDeliveryProfile(b.config.Profile)
}

func (b *MemorySubscriptionBroker) FidelityReport() SubscriptionBrokerFidelityReport {
	return ReportSubscriptionBrokerFidelity(SubscriptionBrokerCapabilities{
		Adapter: SubscriptionAdapterInProcess,
		Frames: []protocol.StreamEventType{
			protocol.StreamOpen, protocol.StreamData, protocol.StreamPatch, protocol.StreamError,
			protocol.StreamComplete, protocol.StreamKeepalive, protocol.StreamResume, protocol.StreamHistoryUnavailable,
		},
		Replay: b.config.Profile.Replay, PrivateScopedDelivery: true, ReplayAuthorization: true,
		HistoryLossDetection: true, TerminalFrameRetention: true, DuplicateSuppression: true, BoundedRetry: true,
	})
}

func (b *MemorySubscriptionBroker) Establish(ctx context.Context, binding SubscriptionHandleBinding) (SubscriptionBrokerSnapshot, error) {
	if b == nil || !validSubscriptionIdentity(binding.HandleID) {
		return SubscriptionBrokerSnapshot{}, ErrSubscriptionBrokerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.records) >= b.config.MaxSubscriptions {
		return SubscriptionBrokerSnapshot{}, ErrSubscriptionCapacity
	}
	if _, exists := b.records[binding.HandleID]; exists {
		return SubscriptionBrokerSnapshot{}, ErrSubscriptionBrokerUnavailable
	}
	snapshot, err := b.config.Snapshot(ctx, cloneSubscriptionBinding(binding))
	if err != nil || len(snapshot) == 0 {
		return SubscriptionBrokerSnapshot{}, ErrSubscriptionBrokerUnavailable
	}
	scope, err := NewStreamCursorScope(StreamCursorScopeOptions{
		Stream: binding.HandleID, Tenant: binding.Tenant, Principal: binding.Principal,
		AuthorizationRevision: binding.AuthorizationRevision, SchemaRevision: binding.SchemaRevision,
	})
	if err != nil {
		return SubscriptionBrokerSnapshot{}, ErrSubscriptionBrokerUnavailable
	}
	const snapshotPosition = uint64(1)
	cursor, err := b.config.CursorCodec.Encode(scope, snapshotPosition)
	if err != nil {
		return SubscriptionBrokerSnapshot{}, ErrSubscriptionBrokerUnavailable
	}
	record := &memoryBrokerRecord{binding: cloneSubscriptionBinding(binding), scope: scope, lastPosition: snapshotPosition, subscribers: make(map[*memorySubscriptionSource]struct{})}
	b.records[binding.HandleID] = record
	topic := subscriptionTopic(binding)
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[string]*memoryBrokerRecord)
		b.duplicates[topic] = &memoryBrokerDuplicateWindow{entries: make(map[string]memoryBrokerDuplicate)}
	}
	b.topics[topic][binding.HandleID] = record
	return SubscriptionBrokerSnapshot{Data: slices.Clone(snapshot), Cursor: cursor, Position: snapshotPosition}, nil
}

func (b *MemorySubscriptionBroker) Open(ctx context.Context, binding SubscriptionHandleBinding, cursor string) (StreamSource, error) {
	if b == nil || cursor == "" {
		return nil, ErrSubscriptionHistoryUnavailable
	}
	now, err := callProcessClock(b.now)
	if err != nil {
		return nil, ErrSubscriptionBrokerUnavailable
	}
	b.mu.Lock()
	record := b.records[binding.HandleID]
	if record == nil || !sameSubscriptionBinding(record.binding, binding) {
		b.mu.Unlock()
		return nil, ErrSubscriptionHistoryUnavailable
	}
	b.expireHistoryLocked(record, now)
	position, err := b.config.CursorCodec.Decode(cursor, record.scope)
	if err != nil || position < record.evictedThrough || position > record.lastPosition {
		b.mu.Unlock()
		return nil, ErrSubscriptionHistoryUnavailable
	}
	initial := []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: binding.HandleID, Sequence: 1, SchemaRevision: binding.SchemaRevision}}
	sequence := uint64(2)
	initial = append(initial, protocol.StreamFrame{Type: protocol.StreamResume, Stream: binding.HandleID, Sequence: sequence, Cursor: cursor})
	sequence++
	for _, stored := range record.history {
		if stored.position <= position {
			continue
		}
		frame := cloneReplayFrame(stored.frame)
		frame.Sequence = sequence
		sequence++
		initial = append(initial, frame)
	}
	source := &memorySubscriptionSource{
		broker: b, record: record, initial: initial, live: make(chan protocol.StreamFrame, b.config.SubscriberQueue),
		nextSequence: sequence, closed: make(chan struct{}),
	}
	record.subscribers[source] = struct{}{}
	b.mu.Unlock()
	return source, nil
}

func (b *MemorySubscriptionBroker) Revoke(_ context.Context, binding SubscriptionHandleBinding) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	record := b.records[binding.HandleID]
	if record == nil {
		b.mu.Unlock()
		return nil
	}
	delete(b.records, binding.HandleID)
	topic := subscriptionTopic(record.binding)
	delete(b.topics[topic], binding.HandleID)
	if len(b.topics[topic]) == 0 {
		delete(b.topics, topic)
		delete(b.duplicates, topic)
	}
	b.totalBytes -= record.historyBytes
	record.history = nil
	record.historyBytes = 0
	subscribers := make([]*memorySubscriptionSource, 0, len(record.subscribers))
	for subscriber := range record.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	record.subscribers = make(map[*memorySubscriptionSource]struct{})
	b.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.closeWithCause(ErrSubscriptionHandleUnavailable)
	}
	return nil
}

// Publish commits one logical event to every established handle for the exact
// canonical operation and variable identities. Position assignment, history
// retention, and live enqueue occur under one lock.
func (b *MemorySubscriptionBroker) Publish(operationIdentity, variableIdentity string, frame protocol.StreamFrame) error {
	if b == nil || !validSubscriptionIdentity(operationIdentity) || !validSubscriptionIdentity(variableIdentity) ||
		(frame.Type != protocol.StreamData && frame.Type != protocol.StreamPatch && frame.Type != protocol.StreamError && frame.Type != protocol.StreamComplete && frame.Type != protocol.StreamKeepalive) {
		return protocol.ErrInvalidStreamFrame
	}
	now, err := callProcessClock(b.now)
	if err != nil {
		return ErrSubscriptionBrokerUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	topic := operationIdentity + "\x00" + variableIdentity
	records := b.topics[topic]
	duplicates := b.duplicates[topic]
	if frame.EventID != "" && duplicates != nil {
		b.expireDuplicateWindowLocked(duplicates, now)
		if prior, ok := duplicates.entries[frame.EventID]; ok {
			if reflect.DeepEqual(subscriptionBrokerEventIdentity(prior.frame), subscriptionBrokerEventIdentity(frame)) {
				return nil
			}
			return protocol.ErrStreamDuplicateConflict
		}
	}
	activeRecords := 0
	for _, record := range records {
		if record.terminal {
			continue
		}
		activeRecords++
		if record.lastPosition == ^uint64(0) {
			return ErrSubscriptionCapacity
		}
		candidate := cloneReplayFrame(frame)
		candidate.Stream = record.binding.HandleID
		candidate.Sequence = 1
		replayable := candidate.Type == protocol.StreamData || candidate.Type == protocol.StreamPatch || (candidate.Type == protocol.StreamError && !candidate.Final)
		retained := candidate.Type != protocol.StreamKeepalive
		if retained {
			record.lastPosition++
		}
		if replayable {
			candidate.Position = record.lastPosition
			candidate.HasPosition = true
		}
		encoded, marshalErr := protocol.MarshalStreamFrame(candidate, protocol.DefaultStreamLimits())
		if marshalErr != nil {
			return marshalErr
		}
		storedBytes := uint64(len(encoded))
		if retained {
			if storedBytes > b.config.MaxHistoryBytes || storedBytes > b.config.MaxTotalHistoryBytes {
				return ErrSubscriptionCapacity
			}
			b.expireHistoryLocked(record, now)
			for len(record.history) >= b.config.MaxHistoryEvents || record.historyBytes > b.config.MaxHistoryBytes-storedBytes || b.totalBytes > b.config.MaxTotalHistoryBytes-storedBytes {
				if len(record.history) == 0 {
					return ErrSubscriptionCapacity
				}
				b.evictOldestLocked(record)
			}
			record.history = append(record.history, memoryBrokerFrame{frame: candidate, position: record.lastPosition, bytes: storedBytes, expiresAt: now.Add(b.config.RetentionDuration)})
			record.historyBytes += storedBytes
			b.totalBytes += storedBytes
		}
		for subscriber := range record.subscribers {
			delivery := cloneReplayFrame(candidate)
			delivery.Sequence = subscriber.nextSequence
			subscriber.nextSequence++
			select {
			case subscriber.live <- delivery:
			default:
				delete(record.subscribers, subscriber)
				subscriber.closeWithCause(ErrStreamSlowConsumer)
			}
		}
		if candidate.Type == protocol.StreamComplete || (candidate.Type == protocol.StreamError && candidate.Final) {
			record.terminal = true
			for subscriber := range record.subscribers {
				delete(record.subscribers, subscriber)
			}
		}
	}
	if activeRecords == 0 && len(records) != 0 {
		return protocol.ErrStreamClosed
	}
	if activeRecords != 0 && frame.EventID != "" && duplicates != nil {
		duplicates.entries[frame.EventID] = memoryBrokerDuplicate{frame: cloneReplayFrame(frame), expiresAt: now.Add(b.config.RetentionDuration)}
		duplicates.order = append(duplicates.order, frame.EventID)
		for len(duplicates.order) > b.config.MaxHistoryEvents {
			b.evictDuplicateLocked(duplicates)
		}
	}
	return nil
}

func (b *MemorySubscriptionBroker) expireDuplicateWindowLocked(window *memoryBrokerDuplicateWindow, now time.Time) {
	for len(window.order) != 0 && !now.Before(window.entries[window.order[0]].expiresAt) {
		b.evictDuplicateLocked(window)
	}
}

func (b *MemorySubscriptionBroker) evictDuplicateLocked(window *memoryBrokerDuplicateWindow) {
	oldest := window.order[0]
	delete(window.entries, oldest)
	copy(window.order, window.order[1:])
	window.order[len(window.order)-1] = ""
	window.order = window.order[:len(window.order)-1]
}

func subscriptionBrokerEventIdentity(frame protocol.StreamFrame) protocol.StreamFrame {
	frame = cloneReplayFrame(frame)
	frame.Stream = ""
	frame.Sequence = 0
	frame.Position = 0
	frame.HasPosition = false
	return frame
}

func (b *MemorySubscriptionBroker) expireHistoryLocked(record *memoryBrokerRecord, now time.Time) {
	for len(record.history) > 0 && !now.Before(record.history[0].expiresAt) {
		b.evictOldestLocked(record)
	}
}

func (b *MemorySubscriptionBroker) evictOldestLocked(record *memoryBrokerRecord) {
	oldest := record.history[0]
	record.evictedThrough = oldest.position
	record.historyBytes -= oldest.bytes
	b.totalBytes -= oldest.bytes
	copy(record.history, record.history[1:])
	record.history[len(record.history)-1] = memoryBrokerFrame{}
	record.history = record.history[:len(record.history)-1]
}

func (b *MemorySubscriptionBroker) detach(source *memorySubscriptionSource) {
	b.mu.Lock()
	delete(source.record.subscribers, source)
	b.mu.Unlock()
}

type memorySubscriptionSource struct {
	closeOnce    sync.Once
	broker       *MemorySubscriptionBroker
	record       *memoryBrokerRecord
	initial      []protocol.StreamFrame
	live         chan protocol.StreamFrame
	nextSequence uint64
	closed       chan struct{}
	mu           sync.Mutex
	cause        error
}

func (s *memorySubscriptionSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	s.mu.Lock()
	select {
	case <-s.closed:
		cause := s.cause
		s.mu.Unlock()
		if cause == nil {
			cause = io.EOF
		}
		return protocol.StreamFrame{}, cause
	default:
	}
	if len(s.initial) != 0 {
		frame := cloneReplayFrame(s.initial[0])
		s.initial[0] = protocol.StreamFrame{}
		s.initial = s.initial[1:]
		s.mu.Unlock()
		return frame, nil
	}
	s.mu.Unlock()
	select {
	case frame := <-s.live:
		return cloneReplayFrame(frame), nil
	case <-s.closed:
		s.mu.Lock()
		cause := s.cause
		s.mu.Unlock()
		if cause == nil {
			cause = io.EOF
		}
		return protocol.StreamFrame{}, cause
	case <-ctx.Done():
		return protocol.StreamFrame{}, context.Cause(ctx)
	}
}

func (s *memorySubscriptionSource) Close() error {
	s.closeWithCause(io.EOF)
	s.broker.detach(s)
	return nil
}

func (s *memorySubscriptionSource) closeWithCause(cause error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.cause = cause
		s.mu.Unlock()
		close(s.closed)
	})
}

func subscriptionTopic(binding SubscriptionHandleBinding) string {
	return binding.OperationIdentity + "\x00" + binding.VariableIdentity
}

func sameSubscriptionBinding(left, right SubscriptionHandleBinding) bool {
	return left.HandleID == right.HandleID && left.Principal == right.Principal && left.Tenant == right.Tenant &&
		left.OperationIdentity == right.OperationIdentity && left.VariableIdentity == right.VariableIdentity &&
		left.SchemaRevision == right.SchemaRevision && left.AuthorizationRevision == right.AuthorizationRevision
}

var _ SubscriptionBroker = (*MemorySubscriptionBroker)(nil)
var _ StreamSource = (*memorySubscriptionSource)(nil)
