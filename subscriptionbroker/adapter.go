// Package subscriptionbroker adapts durable broker delivery sources to the
// secure subscription-handle contract owned by package runtime.
package subscriptionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
)

const Profile = "streaming.broker-adapters-go-1"

const (
	CodeInvalidConfig     = "BROKER_ADAPTER_INVALID_CONFIG"
	CodeInvalidRecord     = "BROKER_ADAPTER_INVALID_RECORD"
	CodeUnavailable       = "BROKER_ADAPTER_UNAVAILABLE"
	CodeHistoryLost       = "BROKER_ADAPTER_HISTORY_LOST"
	CodeSlowConsumer      = "BROKER_ADAPTER_SLOW_CONSUMER"
	CodeResourceExhausted = "BROKER_ADAPTER_RESOURCE_EXHAUSTED"
	CodeCancelled         = "BROKER_ADAPTER_CANCELLED"
)

var (
	ErrDriverUnavailable  = errors.New("broker driver unavailable")
	ErrDriverHistoryLost  = errors.New("broker driver history lost")
	ErrDriverSlowConsumer = errors.New("broker driver slow consumer")
	ErrDriverRevoked      = errors.New("broker driver subscription revoked")
)

// Error is the complete public failure surface. It never retains the driver
// cause, product command, address, credential, binding, cursor, or payload.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	match   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) Unwrap() error { return e.match }

func ErrorCode(err error) string {
	public := new(Error)
	if !errors.As(err, &public) {
		return ""
	}
	return public.Code
}

type Limits struct {
	MaxBindingBytes  int
	MaxSnapshotBytes int
	MaxReplayEvents  int
	MaxReplayBytes   uint64
	MaxFrameBytes    int
	MaxPendingEvents int
}

func DefaultLimits() Limits {
	return Limits{
		MaxBindingBytes: 4096, MaxSnapshotBytes: 256 * 1024, MaxReplayEvents: 1024,
		MaxReplayBytes: 8 << 20, MaxFrameBytes: 1 << 20, MaxPendingEvents: 64,
	}
}

// SnapshotFunc is invoked by Driver.Establish inside the same exclusion
// boundary that captures Position. Drivers must invoke it exactly once.
type SnapshotFunc func(context.Context, naatreruntime.SubscriptionHandleBinding) (json.RawMessage, error)

type EstablishRequest struct {
	Binding  naatreruntime.SubscriptionHandleBinding
	Snapshot SnapshotFunc
	Limits   Limits
}

type EstablishResult struct {
	Snapshot json.RawMessage
	Position uint64
}

type AttachRequest struct {
	Binding naatreruntime.SubscriptionHandleBinding
	After   uint64
	Limits  Limits
}

type AttachResult struct {
	Source           DriverSource
	EarliestPosition uint64
	ReplayHighWater  uint64
}

type DriverItem struct {
	Frame    protocol.StreamFrame
	Position uint64
	CaughtUp bool
}

// DriverSource yields retained items through ReplayHighWater, then one
// CaughtUp marker, then live items registered before Attach returned.
type DriverSource interface {
	Next(context.Context) (DriverItem, error)
	Close() error
}

// DriverCapabilities is an explicit startup gate, not inferred from product
// identity. A product driver that cannot prove one field must not start.
type DriverCapabilities struct {
	AtomicSnapshotPosition  bool
	AtomicReplayLiveAttach  bool
	BoundedReplay           bool
	Revocation              bool
	HistoryLossDetection    bool
	SlowConsumerLimit       bool
	TerminalRetention       bool
	DuplicateClassification bool
}

// AtomicDriver is the product-client seam. Establish and Attach are atomic
// product operations, implemented respectively with a PostgreSQL transaction,
// a Redis Lua/Streams operation, or a JetStream publish/consumer barrier.
type AtomicDriver interface {
	Capabilities() DriverCapabilities
	Establish(context.Context, EstablishRequest) (EstablishResult, error)
	Attach(context.Context, AttachRequest) (AttachResult, error)
	Revoke(context.Context, naatreruntime.SubscriptionHandleBinding) error
}

type PostgreSQLDriver interface {
	AtomicDriver
	PostgreSQLAtomicDriver()
}

type RedisStreamsDriver interface {
	AtomicDriver
	RedisStreamsAtomicDriver()
}

type NATSJetStreamDriver interface {
	AtomicDriver
	NATSJetStreamAtomicDriver()
}

type Config struct {
	Profile     naatreruntime.SubscriptionDeliveryProfile
	CursorCodec *naatreruntime.StreamCursorCodec
	Snapshot    SnapshotFunc
	Limits      Limits
}

type adapter struct {
	kind   naatreruntime.SubscriptionAdapterKind
	driver AtomicDriver
	config Config
}

type PostgreSQLAdapter struct{ *adapter }
type RedisStreamsAdapter struct{ *adapter }
type NATSJetStreamAdapter struct{ *adapter }

func NewPostgreSQL(driver PostgreSQLDriver, config Config) (*PostgreSQLAdapter, error) {
	value, err := newAdapter(naatreruntime.SubscriptionAdapterPostgreSQL, driver, config)
	if err != nil {
		return nil, err
	}
	return &PostgreSQLAdapter{adapter: value}, nil
}

func NewRedisStreams(driver RedisStreamsDriver, config Config) (*RedisStreamsAdapter, error) {
	value, err := newAdapter(naatreruntime.SubscriptionAdapterRedisStreams, driver, config)
	if err != nil {
		return nil, err
	}
	return &RedisStreamsAdapter{adapter: value}, nil
}

func NewNATSJetStream(driver NATSJetStreamDriver, config Config) (*NATSJetStreamAdapter, error) {
	value, err := newAdapter(naatreruntime.SubscriptionAdapterNATSJetStream, driver, config)
	if err != nil {
		return nil, err
	}
	return &NATSJetStreamAdapter{adapter: value}, nil
}

func newAdapter(kind naatreruntime.SubscriptionAdapterKind, driver AtomicDriver, config Config) (*adapter, error) {
	if driver == nil || config.CursorCodec == nil || config.Snapshot == nil || !validLimits(config.Limits) || !validProfile(config.Profile) {
		return nil, publicError(CodeInvalidConfig, "broker adapter configuration is invalid", nil)
	}
	capabilities := driver.Capabilities()
	if !capabilities.AtomicSnapshotPosition || !capabilities.AtomicReplayLiveAttach || !capabilities.BoundedReplay ||
		!capabilities.Revocation || !capabilities.HistoryLossDetection || !capabilities.SlowConsumerLimit ||
		!capabilities.TerminalRetention || !capabilities.DuplicateClassification {
		return nil, publicError(CodeInvalidConfig, "broker driver capabilities are incomplete", naatreruntime.ErrSubscriptionCapabilityUnsupported)
	}
	return &adapter{kind: kind, driver: driver, config: config}, nil
}

func (a *adapter) DeliveryProfile() naatreruntime.SubscriptionDeliveryProfile {
	if a == nil {
		return naatreruntime.SubscriptionDeliveryProfile{}
	}
	profile := a.config.Profile
	profile.BrowserAuthentication = append([]naatreruntime.SubscriptionBrowserAuthentication(nil), profile.BrowserAuthentication...)
	return profile
}

func (a *adapter) FidelityReport() naatreruntime.SubscriptionBrokerFidelityReport {
	if a == nil {
		return naatreruntime.SubscriptionBrokerFidelityReport{}
	}
	return naatreruntime.ReportSubscriptionBrokerFidelity(naatreruntime.SubscriptionBrokerCapabilities{
		Adapter: a.kind,
		Frames: []protocol.StreamEventType{
			protocol.StreamOpen, protocol.StreamData, protocol.StreamPatch, protocol.StreamError,
			protocol.StreamComplete, protocol.StreamKeepalive, protocol.StreamResume, protocol.StreamHistoryUnavailable,
		},
		Replay: a.config.Profile.Replay, PrivateScopedDelivery: true, ReplayAuthorization: true,
		HistoryLossDetection: true, TerminalFrameRetention: true, DuplicateSuppression: true, BoundedRetry: true,
	})
}

func (a *adapter) Establish(ctx context.Context, binding naatreruntime.SubscriptionHandleBinding) (naatreruntime.SubscriptionBrokerSnapshot, error) {
	if a == nil || !validBinding(binding, a.config.Limits.MaxBindingBytes) {
		return naatreruntime.SubscriptionBrokerSnapshot{}, publicError(CodeInvalidRecord, "subscription binding is invalid", nil)
	}
	if err := contextError(ctx); err != nil {
		return naatreruntime.SubscriptionBrokerSnapshot{}, err
	}
	result, err := a.driver.Establish(ctx, EstablishRequest{Binding: binding, Snapshot: a.config.Snapshot, Limits: a.config.Limits})
	if err != nil {
		return naatreruntime.SubscriptionBrokerSnapshot{}, driverError(err)
	}
	if result.Position == 0 || len(result.Snapshot) == 0 || len(result.Snapshot) > a.config.Limits.MaxSnapshotBytes {
		return naatreruntime.SubscriptionBrokerSnapshot{}, publicError(CodeResourceExhausted, "broker snapshot is outside the configured bound", naatreruntime.ErrSubscriptionCapacity)
	}
	scope, err := cursorScope(binding)
	if err != nil {
		return naatreruntime.SubscriptionBrokerSnapshot{}, publicError(CodeInvalidRecord, "subscription binding is invalid", nil)
	}
	cursor, err := a.config.CursorCodec.Encode(scope, result.Position)
	if err != nil {
		return naatreruntime.SubscriptionBrokerSnapshot{}, publicError(CodeUnavailable, "broker delivery is unavailable", naatreruntime.ErrSubscriptionBrokerUnavailable)
	}
	return naatreruntime.SubscriptionBrokerSnapshot{Data: append(json.RawMessage(nil), result.Snapshot...), Cursor: cursor, Position: result.Position}, nil
}

func (a *adapter) Open(ctx context.Context, binding naatreruntime.SubscriptionHandleBinding, cursor string) (naatreruntime.StreamSource, error) {
	if a == nil || cursor == "" || !validBinding(binding, a.config.Limits.MaxBindingBytes) {
		return nil, historyError()
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	scope, err := cursorScope(binding)
	if err != nil {
		return nil, historyError()
	}
	position, err := a.config.CursorCodec.Decode(cursor, scope)
	if err != nil || position == 0 {
		return nil, historyError()
	}
	attached, err := a.driver.Attach(ctx, AttachRequest{Binding: binding, After: position, Limits: a.config.Limits})
	if err != nil {
		return nil, driverError(err)
	}
	if attached.Source == nil || attached.ReplayHighWater < position || (attached.EarliestPosition != 0 && position < attached.EarliestPosition) {
		if attached.Source != nil {
			_ = attached.Source.Close()
		}
		return nil, historyError()
	}
	return &source{
		driver: attached.Source, binding: binding, cursor: cursor, limits: a.config.Limits,
		replayHighWater: attached.ReplayHighWater, lastPosition: position, sequence: 1,
	}, nil
}

func (a *adapter) Revoke(ctx context.Context, binding naatreruntime.SubscriptionHandleBinding) error {
	if a == nil {
		return nil
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := a.driver.Revoke(ctx, binding); err != nil {
		return driverError(err)
	}
	return nil
}

type source struct {
	mu              sync.Mutex
	nextMu          sync.Mutex
	driver          DriverSource
	binding         naatreruntime.SubscriptionHandleBinding
	cursor          string
	limits          Limits
	replayHighWater uint64
	lastPosition    uint64
	sequence        uint64
	replayEvents    int
	replayBytes     uint64
	caughtUp        bool
	closed          bool
}

func (s *source) Next(ctx context.Context) (protocol.StreamFrame, error) {
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	if frame, ready, err := s.initialFrame(); ready || err != nil {
		return frame, err
	}
	for {
		item, err := s.driver.Next(ctx)
		if err != nil {
			if s.isClosed() {
				return protocol.StreamFrame{}, io.EOF
			}
			return protocol.StreamFrame{}, driverError(err)
		}
		frame, ready, err := s.acceptItem(item)
		if ready || err != nil {
			return frame, err
		}
	}
}

func (s *source) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *source) initialFrame() (protocol.StreamFrame, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.StreamFrame{}, false, io.EOF
	}
	if s.sequence == 1 {
		s.sequence++
		return protocol.StreamFrame{Type: protocol.StreamOpen, Stream: s.binding.HandleID, Sequence: 1, SchemaRevision: s.binding.SchemaRevision}, true, nil
	}
	if s.sequence == 2 {
		s.sequence++
		return protocol.StreamFrame{Type: protocol.StreamResume, Stream: s.binding.HandleID, Sequence: 2, Cursor: s.cursor}, true, nil
	}
	return protocol.StreamFrame{}, false, nil
}

func (s *source) acceptItem(item DriverItem) (protocol.StreamFrame, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.StreamFrame{}, false, io.EOF
	}
	if item.CaughtUp {
		if s.caughtUp || item.Position != s.replayHighWater {
			return protocol.StreamFrame{}, false, publicError(CodeInvalidRecord, "broker handoff marker is invalid", naatreruntime.ErrSubscriptionBrokerUnavailable)
		}
		s.caughtUp = true
		return protocol.StreamFrame{}, false, nil
	}
	if err := s.validateItem(item); err != nil {
		return protocol.StreamFrame{}, false, err
	}
	frame := item.Frame
	frame.Stream = s.binding.HandleID
	frame.Sequence = s.sequence
	s.sequence++
	s.lastPosition = item.Position
	if frame.Type == protocol.StreamData || frame.Type == protocol.StreamPatch || (frame.Type == protocol.StreamError && !frame.Final) {
		frame.Position = item.Position
		frame.HasPosition = true
	} else {
		frame.Position = 0
		frame.HasPosition = false
	}
	return frame, true, nil
}

func (s *source) validateItem(item DriverItem) error {
	if item.Position <= s.lastPosition || item.Position > s.replayHighWater && !s.caughtUp || item.Position <= s.replayHighWater && s.caughtUp ||
		!deliverable(item.Frame.Type) || item.Frame.Stream != "" || item.Frame.Sequence != 0 || item.Frame.HasPosition || item.Frame.Position != 0 {
		return publicError(CodeInvalidRecord, "broker delivery record is invalid", naatreruntime.ErrSubscriptionBrokerUnavailable)
	}
	candidate := item.Frame
	candidate.Stream = s.binding.HandleID
	candidate.Sequence = s.sequence
	if candidate.Type == protocol.StreamData || candidate.Type == protocol.StreamPatch || (candidate.Type == protocol.StreamError && !candidate.Final) {
		candidate.Position = item.Position
		candidate.HasPosition = true
	}
	encoded, err := protocol.MarshalStreamFrame(candidate, protocol.DefaultStreamLimits())
	if err != nil || len(encoded) > s.limits.MaxFrameBytes {
		return publicError(CodeResourceExhausted, "broker frame is outside the configured bound", naatreruntime.ErrSubscriptionCapacity)
	}
	if !s.caughtUp {
		s.replayEvents++
		encodedBytes := uint64(len(encoded))
		if s.replayEvents > s.limits.MaxReplayEvents || encodedBytes > s.limits.MaxReplayBytes-s.replayBytes {
			return publicError(CodeResourceExhausted, "broker replay is outside the configured bound", naatreruntime.ErrSubscriptionCapacity)
		}
		s.replayBytes += encodedBytes
	}
	return nil
}

func (s *source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if err := s.driver.Close(); err != nil {
		return driverError(err)
	}
	return nil
}

func deliverable(value protocol.StreamEventType) bool {
	return value == protocol.StreamData || value == protocol.StreamPatch || value == protocol.StreamError ||
		value == protocol.StreamComplete || value == protocol.StreamKeepalive
}

func cursorScope(binding naatreruntime.SubscriptionHandleBinding) (naatreruntime.StreamCursorScope, error) {
	options := naatreruntime.StreamCursorScopeOptions{
		Stream: binding.HandleID, Tenant: binding.Tenant, Principal: binding.Principal,
		AuthorizationRevision: binding.AuthorizationRevision, SchemaRevision: binding.SchemaRevision,
	}
	return naatreruntime.NewStreamCursorScope(options)
}

func validLimits(value Limits) bool {
	return value.MaxBindingBytes > 0 && value.MaxSnapshotBytes > 0 && value.MaxReplayEvents > 0 &&
		value.MaxReplayBytes > 0 && value.MaxFrameBytes > 0 && value.MaxPendingEvents > 0
}

func validProfile(value naatreruntime.SubscriptionDeliveryProfile) bool {
	if value.Name == "" || value.MediaType == "" || (value.Replay != protocol.StreamReplayBounded && value.Replay != protocol.StreamReplayDurable) ||
		value.RetentionHorizon <= 0 || !value.LossDetection || !value.TerminalFrames || value.MaxConnectionLifetime <= 0 ||
		value.ReconnectStagger <= 0 || value.ReconnectStagger >= value.MaxConnectionLifetime || value.MaxReconnectAttempts <= 0 ||
		len(value.BrowserAuthentication) == 0 {
		return false
	}
	for _, authentication := range value.BrowserAuthentication {
		if authentication != naatreruntime.SubscriptionFetchBearer && authentication != naatreruntime.SubscriptionSameOriginCookie {
			return false
		}
	}
	return true
}

func validBinding(value naatreruntime.SubscriptionHandleBinding, maximum int) bool {
	fields := []string{value.HandleID, value.Principal, value.Tenant, value.OperationIdentity, value.VariableIdentity, value.SchemaRevision, value.AuthorizationRevision}
	total := 0
	for _, field := range fields {
		if field == "" || len(field) > maximum-total {
			return false
		}
		total += len(field)
	}
	return total <= maximum
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return publicError(CodeCancelled, "broker operation was cancelled", context.Cause(ctx))
	default:
		return nil
	}
}

func driverError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return publicError(CodeCancelled, "broker operation was cancelled", err)
	}
	switch {
	case errors.Is(err, ErrDriverHistoryLost):
		return historyError()
	case errors.Is(err, ErrDriverSlowConsumer):
		return publicError(CodeSlowConsumer, "broker consumer exceeded its delivery bound", naatreruntime.ErrStreamSlowConsumer)
	case errors.Is(err, ErrDriverRevoked):
		return publicError(CodeUnavailable, "subscription handle is unavailable", naatreruntime.ErrSubscriptionHandleUnavailable)
	default:
		return publicError(CodeUnavailable, "broker delivery is unavailable", naatreruntime.ErrSubscriptionBrokerUnavailable)
	}
}

func historyError() error {
	return publicError(CodeHistoryLost, "broker history is unavailable", naatreruntime.ErrSubscriptionHistoryUnavailable)
}

func publicError(code, message string, match error) *Error {
	failure := new(Error)
	failure.Code = code
	failure.Message = message
	failure.match = match
	return failure
}

var _ naatreruntime.SubscriptionBroker = (*PostgreSQLAdapter)(nil)
var _ naatreruntime.SubscriptionBroker = (*RedisStreamsAdapter)(nil)
var _ naatreruntime.SubscriptionBroker = (*NATSJetStreamAdapter)(nil)
var _ naatreruntime.StreamSource = (*source)(nil)
