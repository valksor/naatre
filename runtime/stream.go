package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

var (
	ErrInvalidStreamCursor       = errors.New("invalid stream cursor configuration")
	ErrInvalidStreamSubscription = errors.New("invalid stream subscription configuration")
	ErrStreamHistoryUnavailable  = errors.New("stream history unavailable")
	ErrStreamReplayLimit         = errors.New("stream replay limit exceeded")
	ErrStreamSlowConsumer        = errors.New("stream consumer exceeded its bounded queue")
)

const (
	StreamReferenceMaxBytes = 256
	streamCursorMaxBytes    = 4096
)

// StreamCursorScopeOptions bind replay to one authorization and schema view.
type StreamCursorScopeOptions struct {
	Stream                string
	Tenant                string
	Principal             string
	AuthorizationRevision string
	SchemaRevision        string
}

// StreamCursorScope retains only the stream reference and domain-separated
// digests of protected replay boundaries.
type StreamCursorScope struct {
	valid          bool
	stream         string
	schemaRevision string
	key            string
	digest         string
}

func NewStreamCursorScope(options StreamCursorScopeOptions) (StreamCursorScope, error) {
	if !validStreamReference(options.Stream, false) || !validStreamReference(options.Tenant, true) ||
		!validStreamReference(options.Principal, false) || !validStreamReference(options.AuthorizationRevision, false) ||
		!validStreamReference(options.SchemaRevision, false) {
		return StreamCursorScope{}, ErrInvalidStreamCursor
	}
	digest := streamScopeDigest(options)
	return StreamCursorScope{
		valid: true, stream: options.Stream, schemaRevision: options.SchemaRevision,
		key: hex.EncodeToString(digest[:]), digest: hex.EncodeToString(digest[:]),
	}, nil
}

func validStreamReference(value string, optional bool) bool {
	if value == "" {
		return optional
	}
	if len(value) > StreamReferenceMaxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func streamScopeDigest(options StreamCursorScopeOptions) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write([]byte("naatre:stream-scope:v1\n"))
	for _, value := range []string{options.Stream, options.Tenant, options.Principal, options.AuthorizationRevision, options.SchemaRevision} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write([]byte(value))
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

type StreamCursorCodecConfig struct {
	ActiveKeyID string
	Keys        map[string][]byte
	TTL         time.Duration
	Now         func() time.Time
}

type StreamCursorCodec struct {
	cursorSecrets
}

type streamCursorPayload struct {
	Version  uint64 `json:"version"`
	KeyID    string `json:"keyId"`
	Expires  int64  `json:"expires"`
	Scope    string `json:"scope"`
	Position uint64 `json:"position"`
}

func NewStreamCursorCodec(config StreamCursorCodecConfig) (*StreamCursorCodec, error) {
	secrets, status := newCursorSecrets(config.ActiveKeyID, config.Keys, config.TTL, config.Now)
	if status != cursorSecretsValid {
		return nil, ErrInvalidStreamCursor
	}
	return &StreamCursorCodec{cursorSecrets: secrets}, nil
}

func (c *StreamCursorCodec) Encode(scope StreamCursorScope, position uint64) (string, error) {
	if c == nil || !scope.valid || position == 0 {
		return "", ErrInvalidStreamCursor
	}
	now, err := callProcessClock(c.now)
	if err != nil {
		return "", ErrStreamHistoryUnavailable
	}
	payload := streamCursorPayload{Version: 1, KeyID: c.activeKeyID, Expires: now.Add(c.ttl).Unix(), Scope: scope.digest, Position: position}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", ErrInvalidStreamCursor
	}
	signature := cursorSignature(c.keys[c.activeKeyID], encoded)
	cursor := base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(cursor) > streamCursorMaxBytes {
		return "", ErrInvalidStreamCursor
	}
	return cursor, nil
}

func (c *StreamCursorCodec) Decode(cursor string, scope StreamCursorScope) (uint64, error) {
	if c == nil || !scope.valid || len(cursor) > streamCursorMaxBytes {
		return 0, ErrStreamHistoryUnavailable
	}
	payloadPart, signaturePart, ok := strings.Cut(cursor, ".")
	if !ok || payloadPart == "" || signaturePart == "" || strings.Contains(signaturePart, ".") {
		return 0, ErrStreamHistoryUnavailable
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return 0, ErrStreamHistoryUnavailable
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return 0, ErrStreamHistoryUnavailable
	}
	var payload streamCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(payloadBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return 0, ErrStreamHistoryUnavailable
	}
	if err := ensureCursorEOF(decoder); err != nil {
		return 0, ErrStreamHistoryUnavailable
	}
	key, ok := c.keys[payload.KeyID]
	if !ok || !hmac.Equal(signature, cursorSignature(key, payloadBytes)) || payload.Version != 1 || payload.Position == 0 || payload.Scope != scope.digest {
		return 0, ErrStreamHistoryUnavailable
	}
	now, err := callProcessClock(c.now)
	if err != nil || now.Unix() >= payload.Expires {
		return 0, ErrStreamHistoryUnavailable
	}
	return payload.Position, nil
}

type StreamReplayConfig struct {
	MaxHistoryEvents     int
	MaxHistoryBytes      uint64
	SubscriberQueue      int
	MaxStreams           int
	MaxTotalHistoryBytes uint64
	MaxSubscribers       int
	RetentionDuration    time.Duration
	Now                  func() time.Time
}

type StreamReplayLimits struct {
	MaxEvents int
	MaxBytes  uint64
}

// StreamReplaySubscriptionOptions bounds one replay delivery and requires the
// current authorization and pinned-schema checks used before every frame.
type StreamReplaySubscriptionOptions struct {
	Limits                  StreamReplayLimits
	FirstSequence           uint64
	ProfileVersion          string
	ReauthorizeSession      StreamSessionCheck
	Reauthorize             StreamDeliveryCheck
	ValidateSchema          StreamSchemaCheck
	CheckExecutor           *StreamCheckExecutor
	AuthenticationExpiresAt time.Time
	CursorCodec             *StreamCursorCodec
	Session                 StreamSessionLimits
}

type storedStreamFrame struct {
	frame     protocol.StreamFrame
	bytes     uint64
	refs      int
	expiresAt time.Time
	inHistory bool
}

type streamHistory struct {
	events         []*storedStreamFrame
	bytes          uint64
	evictedThrough uint64
	lastPosition   uint64
	subscribers    map[*StreamReplaySubscription]struct{}
}

// StreamReplayBuffer provides an atomic replay-to-live handoff without owning
// an external broker or durable retention policy.
type StreamReplayBuffer struct {
	mu                 sync.Mutex
	config             StreamReplayConfig
	streams            map[string]*streamHistory
	totalRetainedBytes uint64
	totalSubscribers   int
	now                func() time.Time
	nextSubscriberID   uint64
}

func NewStreamReplayBuffer(config StreamReplayConfig) (*StreamReplayBuffer, error) {
	if config.MaxStreams == 0 {
		config.MaxStreams = 1024
	}
	if config.MaxTotalHistoryBytes == 0 {
		config.MaxTotalHistoryBytes = 64 << 20
	}
	if config.MaxSubscribers == 0 {
		config.MaxSubscribers = 4096
	}
	if config.RetentionDuration == 0 {
		config.RetentionDuration = 15 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxHistoryEvents <= 0 || config.MaxHistoryBytes == 0 || config.SubscriberQueue <= 0 ||
		config.MaxStreams <= 0 || config.MaxSubscribers <= 0 || config.RetentionDuration < 0 {
		return nil, ErrStreamReplayLimit
	}
	return &StreamReplayBuffer{config: config, streams: make(map[string]*streamHistory), now: config.Now}, nil
}

func (b *StreamReplayBuffer) Publish(scope StreamCursorScope, frame protocol.StreamFrame) error {
	if b == nil || !scope.valid || frame.Stream != scope.stream || !frame.HasPosition ||
		(frame.Type != protocol.StreamData && frame.Type != protocol.StreamPatch && (frame.Type != protocol.StreamError || frame.Final)) {
		return protocol.ErrInvalidStreamFrame
	}
	encoded, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits())
	if err != nil {
		return err
	}
	size := uint64(len(encoded))
	if size > b.config.MaxHistoryBytes || size > b.config.MaxTotalHistoryBytes {
		return ErrStreamReplayLimit
	}
	now, err := callProcessClock(b.now)
	if err != nil {
		return ErrStreamReplayLimit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cleanupExpiredLocked(now)
	history := b.streams[scope.key]
	if history == nil && len(b.streams) == b.config.MaxStreams {
		return ErrStreamReplayLimit
	}
	if history != nil && frame.Position <= history.lastPosition {
		return protocol.ErrStreamPositionOrder
	}
	if history == nil {
		history = &streamHistory{subscribers: make(map[*StreamReplaySubscription]struct{})}
	}
	for subscription := range history.subscribers {
		if subscription.liveQueueFull() {
			b.detachSubscriptionLocked(history, subscription, true)
		}
	}
	if history.bytes > ^uint64(0)-size {
		return ErrStreamReplayLimit
	}
	retainedBytes := history.bytes + size
	retainedEvents := len(history.events) + 1
	evictCount := 0
	evictedThrough := history.evictedThrough
	for retainedEvents > b.config.MaxHistoryEvents || retainedBytes > b.config.MaxHistoryBytes {
		retainedBytes -= history.events[evictCount].bytes
		evictedThrough = history.events[evictCount].frame.Position
		evictCount++
		retainedEvents--
	}
	projectedRetained, ok := b.projectedRetainedLocked(history, evictCount, size)
	if !ok {
		return ErrStreamReplayLimit
	}
	if projectedRetained > b.config.MaxTotalHistoryBytes {
		b.detachLaggingLocked(history, evictCount, size)
		projectedRetained, ok = b.projectedRetainedLocked(history, evictCount, size)
		if !ok {
			return ErrStreamReplayLimit
		}
	}
	if projectedRetained > b.config.MaxTotalHistoryBytes {
		return ErrStreamReplayLimit
	}
	if _, exists := b.streams[scope.key]; !exists {
		b.streams[scope.key] = history
	}
	for _, evicted := range history.events[:evictCount] {
		evicted.inHistory = false
		b.releaseStoredLocked(evicted)
	}
	stored := &storedStreamFrame{frame: cloneReplayFrame(frame), bytes: size, refs: 1, expiresAt: now.Add(b.config.RetentionDuration), inHistory: true}
	b.totalRetainedBytes += size
	remaining := copy(history.events, history.events[evictCount:])
	for index := remaining; index < len(history.events); index++ {
		history.events[index] = nil
	}
	history.events = append(history.events[:remaining], stored)
	history.bytes = retainedBytes
	history.evictedThrough = evictedThrough
	history.lastPosition = frame.Position
	for subscription := range history.subscribers {
		stored.refs++
		if !subscription.enqueueLive(stored) {
			stored.refs--
			b.detachSubscriptionLocked(history, subscription, true)
		}
	}
	return nil
}

// Finalize removes one completed logical stream and closes any subscriptions
// that the adapter has not already released. Callers must stop publishing the
// completed stream before finalizing it.
func (b *StreamReplayBuffer) Finalize(scope StreamCursorScope) error {
	if b == nil || !scope.valid {
		return ErrInvalidStreamSubscription
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	history := b.streams[scope.key]
	if history == nil {
		return nil
	}
	for subscription := range history.subscribers {
		subscription.lifecycleCancel(io.EOF)
		subscription.mu.Lock()
		subscription.finalized = true
		if !subscription.closed {
			subscription.closed = true
			close(subscription.live)
		}
		subscription.cleanupLockedHeld()
		subscription.mu.Unlock()
		delete(history.subscribers, subscription)
		b.totalSubscribers--
	}
	for _, stored := range history.events {
		stored.inHistory = false
		b.releaseStoredLocked(stored)
	}
	history.events = nil
	history.bytes = 0
	delete(b.streams, scope.key)
	return nil
}

func (b *StreamReplayBuffer) projectedRetainedLocked(history *streamHistory, evictCount int, added uint64) (uint64, bool) {
	if b.totalRetainedBytes > ^uint64(0)-added {
		return 0, false
	}
	projected := b.totalRetainedBytes + added
	for _, evicted := range history.events[:evictCount] {
		if evicted.refs == 1 {
			projected -= evicted.bytes
		}
	}
	return projected, true
}

func (b *StreamReplayBuffer) detachLaggingLocked(history *streamHistory, evictCount int, added uint64) {
	plannedEvictions := make(map[*storedStreamFrame]struct{}, evictCount)
	for _, stored := range history.events[:evictCount] {
		plannedEvictions[stored] = struct{}{}
	}
	type pressureCandidate struct {
		subscription *StreamReplaySubscription
		establishing bool
	}
	var candidates []pressureCandidate
	for _, candidateHistory := range b.streams {
		for subscription := range candidateHistory.subscribers {
			if subscription.contributesToRetentionPressure(plannedEvictions) {
				subscription.mu.Lock()
				establishing := !subscription.established
				subscription.mu.Unlock()
				candidates = append(candidates, pressureCandidate{subscription: subscription, establishing: establishing})
			}
		}
	}
	slices.SortFunc(candidates, func(left, right pressureCandidate) int {
		if left.establishing != right.establishing {
			if left.establishing {
				return -1
			}
			return 1
		}
		if left.subscription.id < right.subscription.id {
			return -1
		}
		if left.subscription.id > right.subscription.id {
			return 1
		}
		return 0
	})
	detachedEstablishing := false
	for _, candidate := range candidates {
		if !candidate.establishing && detachedEstablishing {
			return
		}
		detachedEstablishing = detachedEstablishing || candidate.establishing
		subscription := candidate.subscription
		candidateHistory := b.streams[subscription.key]
		if candidateHistory == nil {
			continue
		}
		b.detachSubscriptionLocked(candidateHistory, subscription, true)
		projected, ok := b.projectedRetainedLocked(history, evictCount, added)
		if ok && projected <= b.config.MaxTotalHistoryBytes {
			return
		}
	}
}

func (b *StreamReplayBuffer) detachSubscriptionLocked(history *streamHistory, subscription *StreamReplaySubscription, slow bool) {
	if !subscription.markUnavailableLocked(slow) {
		return
	}
	delete(history.subscribers, subscription)
	b.totalSubscribers--
	subscription.mu.Lock()
	subscription.cleanupLockedHeld()
	subscription.mu.Unlock()
}

func (b *StreamReplayBuffer) cleanupExpiredLocked(now time.Time) {
	for key, history := range b.streams {
		retained := history.events[:0]
		for _, stored := range history.events {
			if now.Before(stored.expiresAt) {
				retained = append(retained, stored)
				continue
			}
			stored.inHistory = false
			history.bytes -= stored.bytes
			history.evictedThrough = stored.frame.Position
			b.releaseStoredLocked(stored)
		}
		for index := len(retained); index < len(history.events); index++ {
			history.events[index] = nil
		}
		history.events = retained
		if len(history.subscribers) == 0 {
			if len(history.events) == 0 {
				delete(b.streams, key)
			}
		}
	}
}

func (b *StreamReplayBuffer) Subscribe(ctx context.Context, scope StreamCursorScope, cursor string, options StreamReplaySubscriptionOptions) (*StreamReplaySubscription, error) {
	options.Session = resolveStreamSessionLimits(options.Session)
	if err := protocol.NegotiateStreamProfile(options.ProfileVersion); err != nil {
		return nil, err
	}
	if b == nil || !scope.valid || options.Limits.MaxEvents <= 0 || options.Limits.MaxBytes == 0 || options.FirstSequence == 0 ||
		options.ReauthorizeSession == nil || options.Reauthorize == nil || options.ValidateSchema == nil || options.CheckExecutor == nil || options.AuthenticationExpiresAt.IsZero() {
		return nil, ErrInvalidStreamSubscription
	}
	retentionNow, err := callProcessClock(b.now)
	if err != nil {
		return nil, streamEstablishmentFailure(cursor, ErrStreamReplayLimit)
	}
	now := time.Now()
	if !options.AuthenticationExpiresAt.After(now) {
		return nil, ErrStreamAuthenticationExpired
	}
	after := uint64(0)
	if cursor != "" {
		if options.CursorCodec == nil {
			return nil, ErrStreamHistoryUnavailable
		}
		position, err := options.CursorCodec.Decode(cursor, scope)
		if err != nil {
			return nil, ErrStreamHistoryUnavailable
		}
		after = position
	}
	deadline := streamSessionDeadline(now, options.Session.MaxDuration, options.AuthenticationExpiresAt)
	deadlineCause := streamDeadlineCause(now.Add(options.Session.MaxDuration), options.AuthenticationExpiresAt)
	ctx, cancel := streamDeliveryContext(ctx, deadline, deadlineCause, nil)
	defer cancel()
	if err := streamContextError(ctx); err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.cleanupExpiredLocked(retentionNow)
	if b.totalSubscribers == b.config.MaxSubscribers {
		b.mu.Unlock()
		return nil, ErrStreamReplayLimit
	}
	history := b.streams[scope.key]
	if history == nil {
		if after != 0 {
			b.mu.Unlock()
			return nil, ErrStreamHistoryUnavailable
		}
		if len(b.streams) == b.config.MaxStreams {
			b.mu.Unlock()
			return nil, ErrStreamReplayLimit
		}
		history = &streamHistory{subscribers: make(map[*StreamReplaySubscription]struct{})}
		b.streams[scope.key] = history
	}
	if after > history.lastPosition || replayWasEvicted(history, after) {
		b.mu.Unlock()
		return nil, ErrStreamHistoryUnavailable
	}
	replayCount := 0
	var replayBytes uint64
	for _, event := range history.events {
		if event.frame.Position <= after {
			continue
		}
		if replayCount == options.Limits.MaxEvents || replayBytes > ^uint64(0)-event.bytes {
			b.mu.Unlock()
			return nil, streamEstablishmentFailure(cursor, ErrStreamReplayLimit)
		}
		replayCount++
		replayBytes += event.bytes
	}
	initialEvents := replayCount
	if cursor != "" {
		initialEvents++
	}
	if initialEvents > options.Session.MaxEvents {
		b.mu.Unlock()
		return nil, streamEstablishmentFailure(cursor, ErrStreamSessionLimit)
	}
	replay := make([]*storedStreamFrame, 0, replayCount)
	authorizationReplay := make([]*storedStreamFrame, 0, replayCount)
	pending := make(map[*storedStreamFrame]struct{}, replayCount)
	requiredSequences := uint64(replayCount)
	if cursor != "" {
		requiredSequences++
	}
	if requiredSequences != 0 && requiredSequences > ^uint64(0)-options.FirstSequence {
		b.mu.Unlock()
		return nil, streamEstablishmentFailure(cursor, ErrStreamReplayLimit)
	}
	for _, event := range history.events {
		if event.frame.Position <= after {
			continue
		}
		event.refs += 2
		replay = append(replay, event)
		authorizationReplay = append(authorizationReplay, event)
		pending[event] = struct{}{}
	}
	lifecycleCtx, lifecycleCancel := context.WithCancelCause(context.Background())
	b.nextSubscriberID++
	subscription := &StreamReplaySubscription{
		id:    b.nextSubscriberID,
		store: b, key: scope.key, stream: scope.stream, schemaRevision: scope.schemaRevision, replay: replay,
		live: make(chan *storedStreamFrame, b.config.SubscriberQueue), nextGate: make(chan struct{}, 1), nextSequence: options.FirstSequence,
		reauthorize: options.Reauthorize, validateSchema: options.ValidateSchema,
		reauthorizeSession: options.ReauthorizeSession,
		deadline:           deadline, deadlineCause: deadlineCause,
		checkTimeout: options.Session.CheckTimeout, checkExecutor: options.CheckExecutor,
		maxEvents: options.Session.MaxEvents, maxBytes: options.Session.MaxBytes,
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		pending: pending,
	}
	if cursor != "" {
		resume := protocol.StreamFrame{Type: protocol.StreamResume, Stream: scope.stream, Sequence: options.FirstSequence, Cursor: cursor}
		subscription.resume = &resume
	}
	history.subscribers[subscription] = struct{}{}
	b.totalSubscribers++
	b.mu.Unlock()
	checkCtx, checkCancel := streamDeliveryContext(ctx, deadline, deadlineCause, lifecycleCtx)
	defer checkCancel()

	defer func() {
		for _, stored := range authorizationReplay {
			if stored != nil {
				b.releaseStored(stored)
			}
		}
	}()
	checkCandidate := func(candidate protocol.StreamFrame) error {
		if err := runStreamSchemaCheck(checkCtx, options.CheckExecutor, options.Session.CheckTimeout, options.ValidateSchema, scope.schemaRevision); err != nil {
			return err
		}
		return runStreamDeliveryCheck(checkCtx, options.CheckExecutor, options.Session.CheckTimeout, options.Reauthorize, candidate)
	}
	if err := runStreamSessionCheck(checkCtx, options.CheckExecutor, options.Session.CheckTimeout, options.ReauthorizeSession); err != nil {
		normalized := normalizeStreamEstablishmentError(checkCtx, err, cursor)
		subscription.Close()
		return nil, normalized
	}
	var initialBytes uint64
	var exactReplayBytes uint64
	if cursor != "" {
		resume := protocol.StreamFrame{Type: protocol.StreamResume, Stream: scope.stream, Sequence: options.FirstSequence, Cursor: cursor}
		encoded, err := protocol.MarshalStreamFrame(resume, protocol.DefaultStreamLimits())
		if err != nil {
			subscription.Close()
			return nil, ErrStreamHistoryUnavailable
		}
		initialBytes = uint64(len(encoded))
		if initialBytes > options.Session.MaxBytes {
			subscription.Close()
			return nil, ErrStreamHistoryUnavailable
		}
		if err := checkCandidate(resume); err != nil {
			normalized := normalizeStreamEstablishmentError(checkCtx, err, cursor)
			subscription.Close()
			return nil, normalized
		}
	}
	nextSequence := options.FirstSequence
	if cursor != "" {
		nextSequence++
	}
	for index, stored := range authorizationReplay {
		candidate := cloneReplayFrame(stored.frame)
		candidate.Sequence = nextSequence
		encoded, err := protocol.MarshalStreamFrame(candidate, protocol.DefaultStreamLimits())
		candidateBytes := uint64(len(encoded))
		if err != nil {
			subscription.Close()
			return nil, streamEstablishmentFailure(cursor, ErrStreamReplayLimit)
		}
		if candidateBytes > options.Limits.MaxBytes || exactReplayBytes > options.Limits.MaxBytes-candidateBytes {
			subscription.Close()
			return nil, streamEstablishmentFailure(cursor, ErrStreamReplayLimit)
		}
		if candidateBytes > options.Session.MaxBytes || initialBytes > options.Session.MaxBytes-candidateBytes {
			subscription.Close()
			return nil, streamEstablishmentFailure(cursor, ErrStreamSessionLimit)
		}
		exactReplayBytes += candidateBytes
		initialBytes += candidateBytes
		if err := checkCandidate(candidate); err != nil {
			normalized := normalizeStreamEstablishmentError(checkCtx, err, cursor)
			subscription.Close()
			return nil, normalized
		}
		b.releaseStored(stored)
		authorizationReplay[index] = nil
		nextSequence++
	}
	subscription.mu.Lock()
	unavailable := subscription.unavailable
	closed := subscription.closed
	finalized := subscription.finalized
	if !closed {
		subscription.established = true
	}
	subscription.mu.Unlock()
	if finalized {
		subscription.Close()
		return nil, streamEstablishmentFailure(cursor, protocol.ErrStreamClosed)
	}
	if unavailable || closed {
		subscription.Close()
		return nil, normalizeStreamEstablishmentError(lifecycleCtx, ErrStreamSlowConsumer, cursor)
	}
	return subscription, nil
}

func replayWasEvicted(history *streamHistory, after uint64) bool {
	return after < history.evictedThrough
}

func normalizeStreamEstablishmentError(ctx context.Context, err error, cursor string) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if errors.Is(err, ErrStreamAuthenticationExpired) {
		return ErrStreamAuthenticationExpired
	}
	if cursor != "" {
		return ErrStreamHistoryUnavailable
	}
	return err
}

func streamEstablishmentFailure(cursor string, fresh error) error {
	if cursor != "" {
		return ErrStreamHistoryUnavailable
	}
	return fresh
}

type StreamReplaySubscription struct {
	id                 uint64
	nextGate           chan struct{}
	mu                 sync.Mutex
	store              *StreamReplayBuffer
	key                string
	schemaRevision     string
	stream             string
	resume             *protocol.StreamFrame
	replay             []*storedStreamFrame
	pending            map[*storedStreamFrame]struct{}
	live               chan *storedStreamFrame
	nextSequence       uint64
	reauthorize        StreamDeliveryCheck
	reauthorizeSession StreamSessionCheck
	validateSchema     StreamSchemaCheck
	checkExecutor      *StreamCheckExecutor
	deadline           time.Time
	deadlineCause      error
	checkTimeout       time.Duration
	maxEvents          int
	maxBytes           uint64
	usedEvents         int
	usedBytes          uint64
	lifecycleCtx       context.Context
	lifecycleCancel    context.CancelCauseFunc
	closed             bool
	slow               bool
	unavailable        bool
	finalized          bool
	established        bool
	cleaned            bool
}

// Next is safe for concurrent callers and serializes delivery sequence assignment.
func (s *StreamReplaySubscription) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if s == nil {
		return protocol.StreamFrame{}, io.EOF
	}
	ctx, cancel := streamDeliveryContext(ctx, s.deadline, s.deadlineCause, s.lifecycleCtx)
	defer cancel()
	if err := streamContextError(ctx); err != nil {
		s.Close()
		return protocol.StreamFrame{}, err
	}
	select {
	case s.nextGate <- struct{}{}:
		defer func() { <-s.nextGate }()
	case <-ctx.Done():
		s.Close()
		return protocol.StreamFrame{}, context.Cause(ctx)
	}
	if err := streamContextError(ctx); err != nil {
		s.Close()
		return protocol.StreamFrame{}, err
	}
	s.mu.Lock()
	if s.nextSequence == 0 {
		s.mu.Unlock()
		s.Close()
		return protocol.StreamFrame{}, ErrStreamReplayLimit
	}
	if s.resume != nil {
		frame := cloneReplayFrame(*s.resume)
		s.resume = nil
		s.mu.Unlock()
		return s.deliverFrame(ctx, frame)
	}
	if len(s.replay) != 0 {
		frame := s.replay[0]
		delete(s.pending, frame)
		s.replay[0] = nil
		s.replay = s.replay[1:]
		if len(s.replay) == 0 {
			s.replay = nil
		}
		s.mu.Unlock()
		return s.deliver(ctx, frame)
	}
	live := s.live
	s.mu.Unlock()
	select {
	case frame, ok := <-live:
		if ok {
			s.mu.Lock()
			delete(s.pending, frame)
			s.mu.Unlock()
			return s.deliver(ctx, frame)
		}
		s.mu.Lock()
		slow := s.slow
		unavailable := s.unavailable
		s.mu.Unlock()
		if slow {
			return protocol.StreamFrame{}, s.slowConsumerOutcome(ctx)
		}
		if unavailable {
			return protocol.StreamFrame{}, ErrStreamHistoryUnavailable
		}
		return protocol.StreamFrame{}, io.EOF
	case <-ctx.Done():
		s.Close()
		return protocol.StreamFrame{}, context.Cause(ctx)
	}
}

func (s *StreamReplaySubscription) Close() {
	if s == nil || s.store == nil {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.lifecycleCancel(io.EOF)
	history := s.store.streams[s.key]
	if history != nil {
		if _, exists := history.subscribers[s]; exists {
			delete(history.subscribers, s)
			s.store.totalSubscribers--
		}
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.live)
	}
	if !s.cleaned {
		s.cleanupLockedHeld()
	}
	s.mu.Unlock()
	if history != nil && len(history.events) == 0 && len(history.subscribers) == 0 {
		delete(s.store.streams, s.key)
	}
}

func (s *StreamReplaySubscription) markUnavailableLocked(slow bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.slow = slow
	s.unavailable = true
	s.closed = true
	if !s.established {
		s.lifecycleCancel(ErrStreamHistoryUnavailable)
	}
	close(s.live)
	return true
}

func (s *StreamReplaySubscription) liveQueueFull() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && len(s.live) == cap(s.live)
}

func (s *StreamReplaySubscription) enqueueLive(stored *storedStreamFrame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.live <- stored:
		s.pending[stored] = struct{}{}
		return true
	default:
		return false
	}
}

func (s *StreamReplaySubscription) contributesToRetentionPressure(planned map[*storedStreamFrame]struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	for stored := range s.pending {
		if !stored.inHistory {
			return true
		}
		if _, willEvict := planned[stored]; willEvict {
			return true
		}
	}
	return false
}

func (s *StreamReplaySubscription) cleanupLockedHeld() {
	if s.cleaned {
		return
	}
	s.cleaned = true
	for index, stored := range s.replay {
		s.store.releaseStoredLocked(stored)
		s.replay[index] = nil
	}
	s.replay = nil
	for stored := range s.live {
		s.store.releaseStoredLocked(stored)
	}
	s.pending = nil
}

func (s *StreamReplaySubscription) deliver(ctx context.Context, stored *storedStreamFrame) (protocol.StreamFrame, error) {
	frame := cloneReplayFrame(stored.frame)
	delivered, err := s.deliverFrame(ctx, frame)
	s.store.releaseStored(stored)
	return delivered, err
}

func (s *StreamReplaySubscription) deliverFrame(ctx context.Context, frame protocol.StreamFrame) (protocol.StreamFrame, error) {
	if err := streamContextError(ctx); err != nil {
		s.Close()
		return protocol.StreamFrame{}, err
	}
	s.mu.Lock()
	if s.closed {
		slow := s.slow
		if !slow {
			s.mu.Unlock()
			if s.unavailable {
				return protocol.StreamFrame{}, ErrStreamHistoryUnavailable
			}
			return protocol.StreamFrame{}, io.EOF
		}
	}
	if s.nextSequence == ^uint64(0) {
		s.mu.Unlock()
		s.Close()
		return protocol.StreamFrame{}, ErrStreamReplayLimit
	}
	frame.Sequence = s.nextSequence
	encoded, err := protocol.MarshalStreamFrame(frame, protocol.DefaultStreamLimits())
	if err != nil || s.usedEvents == s.maxEvents || uint64(len(encoded)) > s.maxBytes || s.usedBytes > s.maxBytes-uint64(len(encoded)) {
		s.mu.Unlock()
		s.Close()
		return protocol.StreamFrame{}, ErrStreamSessionLimit
	}
	s.usedEvents++
	s.usedBytes += uint64(len(encoded))
	s.mu.Unlock()
	if err := runStreamSchemaCheck(ctx, s.checkExecutor, s.checkTimeout, s.validateSchema, s.schemaRevision); err != nil {
		s.Close()
		return protocol.StreamFrame{}, err
	}
	if err := runStreamDeliveryCheck(ctx, s.checkExecutor, s.checkTimeout, s.reauthorize, frame); err != nil {
		s.Close()
		return protocol.StreamFrame{}, err
	}
	if err := streamContextError(ctx); err != nil {
		s.Close()
		return protocol.StreamFrame{}, err
	}
	s.mu.Lock()
	if s.closed {
		slow := s.slow
		s.mu.Unlock()
		if slow {
			s.Close()
			return protocol.StreamFrame{}, ErrStreamSlowConsumer
		}
		return protocol.StreamFrame{}, io.EOF
	}
	if err := streamContextError(ctx); err != nil {
		s.mu.Unlock()
		s.Close()
		return protocol.StreamFrame{}, err
	}
	s.nextSequence++
	s.mu.Unlock()
	return frame, nil
}

func (s *StreamReplaySubscription) slowConsumerOutcome(ctx context.Context) error {
	if err := runStreamSchemaCheck(ctx, s.checkExecutor, s.checkTimeout, s.validateSchema, s.schemaRevision); err != nil {
		s.Close()
		return err
	}
	if err := runStreamSessionCheck(ctx, s.checkExecutor, s.checkTimeout, s.reauthorizeSession); err != nil {
		s.Close()
		return err
	}
	s.Close()
	return ErrStreamSlowConsumer
}

func (b *StreamReplayBuffer) releaseStored(stored *storedStreamFrame) {
	b.mu.Lock()
	b.releaseStoredLocked(stored)
	b.mu.Unlock()
}

func (b *StreamReplayBuffer) releaseStoredLocked(stored *storedStreamFrame) {
	stored.refs--
	if stored.refs == 0 {
		b.totalRetainedBytes -= stored.bytes
	}
}

func cloneReplayFrame(frame protocol.StreamFrame) protocol.StreamFrame {
	frame.Path = append([]any(nil), frame.Path...)
	frame.Data = slices.Clone(frame.Data)
	if frame.Error != nil {
		cloned := *frame.Error
		frame.Error = &cloned
	}
	return frame
}
