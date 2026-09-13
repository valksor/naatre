package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
)

var (
	ErrInvalidStreamSource         = errors.New("invalid stream source configuration")
	ErrStreamAuthenticationExpired = errors.New("stream authentication expired")
	ErrStreamAuthorizationRevoked  = errors.New("stream authorization revoked")
	ErrStreamSchemaMismatch        = errors.New("stream schema revision does not match its pinned revision")
	ErrStreamSchemaRetired         = errors.New("stream schema revision retired")
	ErrStreamSessionLimit          = errors.New("stream session limit exceeded")
)

// StreamSource is the transport-independent contract implemented by a
// subscription handler. Next transfers ownership of the returned frame to the
// runtime and must not mutate it during or after return. Next must observe
// context cancellation, and Close must be concurrency-safe and unblock any
// pending Next call.
type StreamSource interface {
	Next(context.Context) (protocol.StreamFrame, error)
	Close() error
}

// StreamDeliveryCheck runs immediately before a protected frame is exposed.
// Applications use it to re-evaluate authentication and authorization. The
// callback must observe context cancellation and return before its deadline.
// A process-shared StreamCheckExecutor retains bounded ownership of a callback
// that remains active after its session stops waiting.
type StreamDeliveryCheck func(context.Context, protocol.StreamFrame) error

// StreamSchemaCheck confirms that the session's pinned schema remains
// available. It must observe context cancellation. Retirement must return
// ErrStreamSchemaRetired.
type StreamSchemaCheck func(context.Context, string) error

// StreamSessionCheck re-evaluates current authentication and authorization
// without fabricating an application frame.
type StreamSessionCheck func(context.Context) error

// StreamSessionLimits bound total ownership and each security/schema check.
type StreamSessionLimits struct {
	MaxDuration  time.Duration
	CheckTimeout time.Duration
	MaxEvents    int
	MaxBytes     uint64
}

// DefaultStreamSessionLimits returns finite defaults for untrusted streams.
func DefaultStreamSessionLimits() StreamSessionLimits {
	return StreamSessionLimits{
		MaxDuration:  15 * time.Minute,
		CheckTimeout: 5 * time.Second,
		MaxEvents:    10_000,
		MaxBytes:     64 << 20,
	}
}

type StreamSourceSessionConfig struct {
	Stream                  string
	ProfileVersion          string
	SchemaRevision          string
	Advertisement           protocol.StreamSourceAdvertisement
	AuthenticationExpiresAt time.Time
	Source                  StreamSource
	Reauthorize             StreamDeliveryCheck
	ValidateSchema          StreamSchemaCheck
	CheckExecutor           *StreamCheckExecutor
	Limits                  protocol.StreamLimits
	Session                 StreamSessionLimits
}

// StreamSourceSession binds source ownership, validation, reauthorization,
// schema pinning, and terminal detection to one pull-based stream.
// Next has a single-consumer contract; Close may be called concurrently.
type StreamSourceSession struct {
	source           StreamSource
	schemaRevision   string
	reauthorize      StreamDeliveryCheck
	validateSchema   StreamSchemaCheck
	advertisement    protocol.StreamSourceAdvertisement
	receiver         *protocol.StreamReceiver
	limits           protocol.StreamLimits
	deadline         time.Time
	deadlineCause    error
	checkTimeout     time.Duration
	checkExecutor    *StreamCheckExecutor
	sessionMaxEvents int
	sessionMaxBytes  uint64
	usedEvents       int
	usedBytes        uint64
	lifecycleCtx     context.Context
	lifecycleCancel  context.CancelCauseFunc
	deliveryMu       sync.Mutex
	closed           atomic.Bool
	closeOnce        sync.Once
	closeErr         error
}

func NewStreamSourceSession(config StreamSourceSessionConfig) (*StreamSourceSession, error) {
	config.Session = resolveStreamSessionLimits(config.Session)
	now := time.Now()
	if err := protocol.NegotiateStreamProfile(config.ProfileVersion); err != nil {
		return nil, err
	}
	if err := config.Advertisement.Validate(config.Limits); err != nil {
		if errors.Is(err, protocol.ErrUnsupportedStreamVersion) {
			return nil, err
		}
		return nil, ErrInvalidStreamSource
	}
	if config.Advertisement.ProfileVersion != config.ProfileVersion {
		return nil, ErrInvalidStreamSource
	}
	if config.Source == nil || config.SchemaRevision == "" || config.Reauthorize == nil || config.ValidateSchema == nil || config.CheckExecutor == nil || config.AuthenticationExpiresAt.IsZero() {
		return nil, ErrInvalidStreamSource
	}
	if !config.AuthenticationExpiresAt.After(now) {
		return nil, ErrStreamAuthenticationExpired
	}
	probe := protocol.StreamFrame{Type: protocol.StreamOpen, Stream: config.Stream, Sequence: 1, SchemaRevision: config.SchemaRevision}
	if err := probe.Validate(config.Limits); err != nil {
		return nil, ErrInvalidStreamSource
	}
	receiver, err := protocol.NewStreamReceiver(config.Stream, config.Limits)
	if err != nil {
		return nil, ErrInvalidStreamSource
	}
	lifecycleCtx, lifecycleCancel := context.WithCancelCause(context.Background())
	return &StreamSourceSession{
		source: config.Source, schemaRevision: config.SchemaRevision,
		reauthorize: config.Reauthorize, validateSchema: config.ValidateSchema,
		advertisement: config.Advertisement, receiver: receiver, limits: protocol.ResolveStreamLimits(config.Limits),
		deadline:      streamSessionDeadline(now, config.Session.MaxDuration, config.AuthenticationExpiresAt),
		deadlineCause: streamDeadlineCause(now.Add(config.Session.MaxDuration), config.AuthenticationExpiresAt),
		checkTimeout:  config.Session.CheckTimeout, checkExecutor: config.CheckExecutor,
		sessionMaxEvents: config.Session.MaxEvents, sessionMaxBytes: config.Session.MaxBytes,
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
	}, nil
}

// Advertisement returns the immutable source capabilities bound to the session.
func (s *StreamSourceSession) Advertisement() protocol.StreamSourceAdvertisement {
	if s == nil {
		return protocol.StreamSourceAdvertisement{}
	}
	return s.advertisement
}

func (s *StreamSourceSession) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if s == nil || s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	ctx, cancel := streamDeliveryContext(ctx, s.deadline, s.deadlineCause, s.lifecycleCtx)
	defer cancel()
	frame, err := s.source.Next(ctx)
	if err != nil {
		cause := context.Cause(ctx)
		var finishErr error
		if cause == nil && errors.Is(err, io.EOF) {
			finishErr = s.receiver.Finish()
		}
		s.closeSource()
		if cause != nil {
			return protocol.StreamFrame{}, cause
		}
		if finishErr != nil {
			return protocol.StreamFrame{}, finishErr
		}
		return protocol.StreamFrame{}, err
	}
	if len(frame.Path) > s.limits.MaxPathDepth || len(frame.Data) > s.limits.MaxDataBytes {
		s.closeSource()
		return protocol.StreamFrame{}, protocol.ErrInvalidStreamFrame
	}
	frame = cloneReplayFrame(frame)
	if err := frame.Validate(s.limits); err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	encoded, err := protocol.MarshalStreamFrame(frame, s.limits)
	if err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	if s.usedEvents == s.sessionMaxEvents || uint64(len(encoded)) > s.sessionMaxBytes || s.usedBytes > s.sessionMaxBytes-uint64(len(encoded)) {
		s.closeSource()
		return protocol.StreamFrame{}, ErrStreamSessionLimit
	}
	s.usedEvents++
	s.usedBytes += uint64(len(encoded))
	if err := streamContextError(ctx); err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	if frame.Type == protocol.StreamOpen && frame.SchemaRevision != s.schemaRevision {
		s.closeSource()
		return protocol.StreamFrame{}, ErrStreamSchemaMismatch
	}
	if s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	if err := runStreamSchemaCheck(ctx, s.checkExecutor, s.checkTimeout, s.validateSchema, s.schemaRevision); err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	if protectedStreamFrame(frame) {
		if err := runStreamDeliveryCheck(ctx, s.checkExecutor, s.checkTimeout, s.reauthorize, frame); err != nil {
			s.closeSource()
			return protocol.StreamFrame{}, err
		}
	}
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if err := streamContextError(ctx); err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	if s.closed.Load() {
		return protocol.StreamFrame{}, io.EOF
	}
	if _, err := s.receiver.Accept(frame); err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	if frame.Type == protocol.StreamComplete || frame.Type == protocol.StreamHistoryUnavailable || (frame.Type == protocol.StreamError && frame.Final) {
		s.closeSource()
		return frame, nil
	}
	if err := streamContextError(ctx); err != nil {
		s.closeSource()
		return protocol.StreamFrame{}, err
	}
	return frame, nil
}

func (s *StreamSourceSession) Close() error {
	if s == nil {
		return nil
	}
	s.deliveryMu.Lock()
	s.lifecycleCancel(io.EOF)
	s.closeSource()
	s.deliveryMu.Unlock()
	return s.closeErr
}

func (s *StreamSourceSession) closeSource() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.lifecycleCancel(io.EOF)
		s.closeErr = s.source.Close()
	})
}

func protectedStreamFrame(frame protocol.StreamFrame) bool {
	return frame.Type == protocol.StreamData || frame.Type == protocol.StreamPatch || frame.Type == protocol.StreamError || frame.Type == protocol.StreamResume
}

func resolveStreamSessionLimits(limits StreamSessionLimits) StreamSessionLimits {
	defaults := DefaultStreamSessionLimits()
	if limits.MaxDuration <= 0 {
		limits.MaxDuration = defaults.MaxDuration
	}
	if limits.CheckTimeout <= 0 {
		limits.CheckTimeout = defaults.CheckTimeout
	}
	if limits.MaxEvents <= 0 {
		limits.MaxEvents = defaults.MaxEvents
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	return limits
}

func streamDeliveryContext(ctx context.Context, deadline time.Time, cause error, lifecycle context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	merged, cancelMerged := context.WithCancelCause(ctx)
	stop := func() bool { return false }
	if lifecycle != nil {
		stop = context.AfterFunc(lifecycle, func() { cancelMerged(context.Cause(lifecycle)) })
		if lifecycleCause := context.Cause(lifecycle); lifecycleCause != nil {
			cancelMerged(lifecycleCause)
		}
	}
	bounded, cancelBounded := context.WithDeadlineCause(merged, deadline, cause)
	return bounded, func() {
		stop()
		cancelBounded()
		cancelMerged(context.Canceled)
	}
}

func streamSessionDeadline(now time.Time, duration time.Duration, authenticationExpiresAt time.Time) time.Time {
	deadline := now.Add(duration)
	if authenticationExpiresAt.Before(deadline) {
		return authenticationExpiresAt
	}
	return deadline
}

func streamDeadlineCause(durationDeadline, authenticationExpiresAt time.Time) error {
	if !authenticationExpiresAt.After(durationDeadline) {
		return ErrStreamAuthenticationExpired
	}
	return context.DeadlineExceeded
}

func streamContextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
		return nil
	}
}

func runStreamDeliveryCheck(ctx context.Context, executor *StreamCheckExecutor, timeout time.Duration, check StreamDeliveryCheck, frame protocol.StreamFrame) error {
	err := executor.run(ctx, timeout, func(checkCtx context.Context) error {
		return check(checkCtx, cloneReplayFrame(frame))
	}, ErrStreamAuthorizationRevoked)
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if errors.Is(err, errStreamCheckTimeout) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrStreamAuthenticationExpired) || errors.Is(err, ErrStreamAuthorizationRevoked) {
		return err
	}
	return ErrStreamAuthorizationRevoked
}

func runStreamSchemaCheck(ctx context.Context, executor *StreamCheckExecutor, timeout time.Duration, check StreamSchemaCheck, revision string) error {
	err := executor.run(ctx, timeout, func(checkCtx context.Context) error {
		return check(checkCtx, revision)
	}, ErrStreamSchemaRetired)
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if errors.Is(err, errStreamCheckTimeout) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrStreamAuthenticationExpired) || errors.Is(err, ErrStreamSchemaRetired) {
		return err
	}
	return ErrStreamSchemaRetired
}

func runStreamSessionCheck(ctx context.Context, executor *StreamCheckExecutor, timeout time.Duration, check StreamSessionCheck) error {
	err := executor.run(ctx, timeout, check, ErrStreamAuthorizationRevoked)
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if errors.Is(err, errStreamCheckTimeout) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrStreamAuthenticationExpired) || errors.Is(err, ErrStreamAuthorizationRevoked) {
		return err
	}
	return ErrStreamAuthorizationRevoked
}
