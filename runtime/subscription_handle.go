package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
)

var (
	ErrSubscriptionHandleUnavailable     = errors.New("subscription handle unavailable")
	ErrSubscriptionHistoryUnavailable    = errors.New("subscription history unavailable")
	ErrSubscriptionCapabilityUnsupported = errors.New("subscription delivery capability unsupported")
	ErrSubscriptionIdempotencyConflict   = errors.New("subscription establishment idempotency conflict")
	ErrSubscriptionCapacity              = errors.New("subscription handle capacity exceeded")
	ErrSubscriptionBrokerUnavailable     = errors.New("subscription broker unavailable")
	ErrInvalidSubscriptionHandleConfig   = errors.New("invalid subscription handle configuration")
)

// SubscriptionBrowserAuthentication is a browser-safe delivery credential path.
// Neither path places a credential, operation, variable, cursor, or sufficient
// handle authority in a URL.
type SubscriptionBrowserAuthentication string

const (
	SubscriptionFetchBearer      SubscriptionBrowserAuthentication = "fetch-bearer"
	SubscriptionSameOriginCookie SubscriptionBrowserAuthentication = "same-origin-cookie"
)

// SubscriptionHandleState is the externally observable lifecycle state.
type SubscriptionHandleState string

const (
	SubscriptionHandleActive    SubscriptionHandleState = "active"
	SubscriptionHandleCancelled SubscriptionHandleState = "cancelled"
	SubscriptionHandleRevoked   SubscriptionHandleState = "revoked"
	SubscriptionHandleDraining  SubscriptionHandleState = "draining"
)

// SubscriptionHandleAction names an authorization boundary. Establishment is
// authorized by Prepare; every later protected action passes through Authorizer.
type SubscriptionHandleAction string

const (
	SubscriptionHandleOpenAction    SubscriptionHandleAction = "open"
	SubscriptionHandleResumeAction  SubscriptionHandleAction = "resume"
	SubscriptionHandleRenewAction   SubscriptionHandleAction = "renew"
	SubscriptionHandleCancelAction  SubscriptionHandleAction = "cancel"
	SubscriptionHandleObserveAction SubscriptionHandleAction = "observe"
	SubscriptionHandleDeliverAction SubscriptionHandleAction = "deliver"
)

// SubscriptionDeliveryProfile is the exact broker and browser behavior bound
// to a handle at establishment. All durations and retry counts are finite.
type SubscriptionDeliveryProfile struct {
	Name                  string
	MediaType             string
	Replay                protocol.StreamReplayCapability
	RetentionHorizon      time.Duration
	LossDetection         bool
	TerminalFrames        bool
	MaxConnectionLifetime time.Duration
	ReconnectStagger      time.Duration
	MaxReconnectAttempts  int
	BrowserAuthentication []SubscriptionBrowserAuthentication
}

// SubscriptionHandleLimits are immutable per-handle resource limits.
type SubscriptionHandleLimits struct {
	Session          StreamSessionLimits
	MaxSnapshotBytes int
}

// SubscriptionHandleBinding is retained internally with the opaque identifier.
// It deliberately contains identities, never raw documents, variables, tokens,
// or reusable browser credentials.
type SubscriptionHandleBinding struct {
	HandleID              string
	Principal             string
	Tenant                string
	OperationIdentity     string
	VariableIdentity      string
	SchemaRevision        string
	AuthorizationRevision string
	CreatedAt             time.Time
	ExpiresAt             time.Time
	Limits                SubscriptionHandleLimits
	DeliveryProfile       SubscriptionDeliveryProfile
}

// SubscriptionHandle is the authorization-filtered public state.
type SubscriptionHandle struct {
	ID              string
	State           SubscriptionHandleState
	CreatedAt       time.Time
	ExpiresAt       time.Time
	DeliveryProfile SubscriptionDeliveryProfile
}

// SubscriptionHandleEstablishment is returned only to the authenticated
// establishing principal. Snapshot and cursor are protected no-store data.
type SubscriptionHandleEstablishment struct {
	Handle               SubscriptionHandle
	Snapshot             json.RawMessage
	ReconciliationCursor string
	Created              bool
}

// SubscriptionHandleRequest is transient establishment input. Prepare must
// fully validate and authorize Request, including variables, schema, cost, and
// requested delivery capabilities. The coordinator never stores Request.
type SubscriptionHandleRequest struct {
	Request                 *protocol.Request
	IdempotencyKey          string
	Fingerprint             string
	RequestedAuthentication []SubscriptionBrowserAuthentication
}

// SubscriptionHandlePreparation contains only canonical, bounded identities
// and pinned revisions produced by successful validation.
type SubscriptionHandlePreparation struct {
	OperationIdentity       string
	VariableIdentity        string
	SchemaRevision          string
	AuthorizationRevision   string
	AuthenticationExpiresAt time.Time
	Limits                  SubscriptionHandleLimits
}

type SubscriptionHandlePrepareFunc func(context.Context, SubscriptionHandleRequest) (SubscriptionHandlePreparation, error)

// SubscriptionHandleAuthorizationDecision is the current protected-delivery
// decision. Revision drift requires re-establishment rather than reinterpretation.
type SubscriptionHandleAuthorizationDecision struct {
	Allowed                 bool
	SchemaRevision          string
	AuthorizationRevision   string
	AuthenticationExpiresAt time.Time
}

type SubscriptionHandleAuthorizationRequest struct {
	Action    SubscriptionHandleAction
	Principal Principal
	Binding   SubscriptionHandleBinding
}

type SubscriptionHandleAuthorizer interface {
	AuthorizeSubscriptionHandle(context.Context, SubscriptionHandleAuthorizationRequest) (SubscriptionHandleAuthorizationDecision, error)
}

type SubscriptionHandleAuthorizerFunc func(context.Context, SubscriptionHandleAuthorizationRequest) (SubscriptionHandleAuthorizationDecision, error)

func (f SubscriptionHandleAuthorizerFunc) AuthorizeSubscriptionHandle(ctx context.Context, request SubscriptionHandleAuthorizationRequest) (SubscriptionHandleAuthorizationDecision, error) {
	return f(ctx, request)
}

// SubscriptionBrokerSnapshot is captured atomically with the broker's source
// high-water position. Open must replay strictly after its cursor through one
// captured bound before switching to already-registered live delivery.
type SubscriptionBrokerSnapshot struct {
	Data     json.RawMessage
	Cursor   string
	Position uint64
}

// SubscriptionBroker is protocol-neutral. Implementations retain Naatre frame,
// authorization, replay, history-loss, and terminal semantics even when an
// external hub supplies storage or fan-out.
type SubscriptionBroker interface {
	DeliveryProfile() SubscriptionDeliveryProfile
	Establish(context.Context, SubscriptionHandleBinding) (SubscriptionBrokerSnapshot, error)
	Open(context.Context, SubscriptionHandleBinding, string) (StreamSource, error)
	Revoke(context.Context, SubscriptionHandleBinding) error
}

type SubscriptionHandleConfig struct {
	Broker                 SubscriptionBroker
	Prepare                SubscriptionHandlePrepareFunc
	Authorizer             SubscriptionHandleAuthorizer
	CheckExecutor          *StreamCheckExecutor
	StreamLimits           protocol.StreamLimits
	HandleTTL              time.Duration
	MaxHandles             int
	MaxAttachments         int
	MaxTotalSnapshotBytes  uint64
	GarbageCollectionBatch int
	Now                    func() time.Time
	NewHandleID            func() (string, error)
}

type subscriptionHandleRecord struct {
	handle      SubscriptionHandle
	binding     SubscriptionHandleBinding
	snapshot    json.RawMessage
	cursor      string
	position    uint64
	fingerprint string
	idemIndex   string
	attachments map[*managedSubscriptionSource]struct{}
}

type pendingSubscriptionEstablishment struct {
	done chan struct{}
}

// SubscriptionHandleCoordinator owns bounded process-local handle lifecycle.
// Production durability and external broker rollout are separate adapter work.
type SubscriptionHandleCoordinator struct {
	mu            sync.Mutex
	config        SubscriptionHandleConfig
	profile       SubscriptionDeliveryProfile
	handles       map[string]*subscriptionHandleRecord
	idempotency   map[string]string
	pending       map[string]*pendingSubscriptionEstablishment
	attachments   int
	establishing  int
	snapshotBytes uint64
	draining      bool
}

func NewSubscriptionHandleCoordinator(config SubscriptionHandleConfig) (*SubscriptionHandleCoordinator, error) {
	if config.Broker == nil || config.Prepare == nil || config.Authorizer == nil || config.CheckExecutor == nil {
		return nil, ErrInvalidSubscriptionHandleConfig
	}
	if config.HandleTTL == 0 {
		config.HandleTTL = 15 * time.Minute
	}
	if config.MaxHandles == 0 {
		config.MaxHandles = 1024
	}
	if config.MaxAttachments == 0 {
		config.MaxAttachments = 4096
	}
	if config.MaxTotalSnapshotBytes == 0 {
		config.MaxTotalSnapshotBytes = 64 << 20
	}
	if config.GarbageCollectionBatch == 0 {
		config.GarbageCollectionBatch = 128
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.NewHandleID == nil {
		config.NewHandleID = randomSubscriptionHandleID
	}
	profile := config.Broker.DeliveryProfile()
	if config.HandleTTL <= 0 || config.MaxHandles <= 0 || config.MaxAttachments <= 0 || config.MaxTotalSnapshotBytes == 0 || config.GarbageCollectionBatch <= 0 || validateSubscriptionDeliveryProfile(profile) != nil {
		return nil, ErrInvalidSubscriptionHandleConfig
	}
	return &SubscriptionHandleCoordinator{
		config: config, profile: cloneSubscriptionDeliveryProfile(profile),
		handles: make(map[string]*subscriptionHandleRecord), idempotency: make(map[string]string),
		pending: make(map[string]*pendingSubscriptionEstablishment),
	}, nil
}

func (c *SubscriptionHandleCoordinator) DeliveryProfile() SubscriptionDeliveryProfile {
	if c == nil {
		return SubscriptionDeliveryProfile{}
	}
	return cloneSubscriptionDeliveryProfile(c.profile)
}

func (c *SubscriptionHandleCoordinator) Establish(ctx context.Context, request SubscriptionHandleRequest) (SubscriptionHandleEstablishment, error) {
	if c == nil || ctx == nil || request.Request == nil || !validSubscriptionIdentity(request.Fingerprint) || !validSubscriptionOptionalIdentity(request.IdempotencyKey) {
		return SubscriptionHandleEstablishment{}, ErrInvalidSubscriptionHandleConfig
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok || !validSubscriptionIdentity(principal.Subject) || !validSubscriptionOptionalIdentity(principal.Tenant) {
		return SubscriptionHandleEstablishment{}, ErrSubscriptionHandleUnavailable
	}
	requested := slices.Clone(request.RequestedAuthentication)
	slices.Sort(requested)
	if !validRequestedSubscriptionAuthentication(requested, c.profile.BrowserAuthentication) {
		return SubscriptionHandleEstablishment{}, ErrSubscriptionCapabilityUnsupported
	}
	idemIndex := subscriptionIdempotencyIndex(principal, request.IdempotencyKey)
	c.CollectExpired(ctx, c.config.GarbageCollectionBatch)
	for idemIndex != "" {
		c.mu.Lock()
		if existing, conflict := c.idempotentResultLocked(idemIndex, request.Fingerprint); existing != nil || conflict {
			if conflict {
				c.mu.Unlock()
				return SubscriptionHandleEstablishment{}, ErrSubscriptionIdempotencyConflict
			}
			result := cloneSubscriptionEstablishment(existing, false)
			c.mu.Unlock()
			if _, err := c.authorize(ctx, result.Handle.ID, SubscriptionHandleObserveAction); err != nil {
				return SubscriptionHandleEstablishment{}, err
			}
			return result, nil
		}
		if pending := c.pending[idemIndex]; pending != nil {
			done := pending.done
			c.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return SubscriptionHandleEstablishment{}, ctx.Err()
			}
		}
		c.pending[idemIndex] = &pendingSubscriptionEstablishment{done: make(chan struct{})}
		c.mu.Unlock()
		break
	}
	if idemIndex != "" {
		defer c.finishPendingEstablishment(idemIndex)
	}
	c.mu.Lock()
	if c.draining || len(c.handles)+c.establishing >= c.config.MaxHandles {
		c.mu.Unlock()
		return SubscriptionHandleEstablishment{}, ErrSubscriptionCapacity
	}
	c.establishing++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.establishing--
		c.mu.Unlock()
	}()

	preparation, err := c.config.Prepare(ctx, SubscriptionHandleRequest{
		Request: request.Request, IdempotencyKey: request.IdempotencyKey, Fingerprint: request.Fingerprint,
		RequestedAuthentication: requested,
	})
	if err != nil {
		return SubscriptionHandleEstablishment{}, err
	}
	now, err := callProcessClock(c.config.Now)
	if err != nil {
		return SubscriptionHandleEstablishment{}, ErrSubscriptionHandleUnavailable
	}
	if err := validateSubscriptionPreparation(preparation, now); err != nil {
		return SubscriptionHandleEstablishment{}, err
	}
	id, err := c.config.NewHandleID()
	if err != nil || !validSubscriptionIdentity(id) {
		return SubscriptionHandleEstablishment{}, ErrSubscriptionCapacity
	}
	expiresAt := now.Add(c.config.HandleTTL)
	if preparation.AuthenticationExpiresAt.Before(expiresAt) {
		expiresAt = preparation.AuthenticationExpiresAt
	}
	binding := SubscriptionHandleBinding{
		HandleID: id, Principal: principal.Subject, Tenant: principal.Tenant,
		OperationIdentity: preparation.OperationIdentity, VariableIdentity: preparation.VariableIdentity,
		SchemaRevision: preparation.SchemaRevision, AuthorizationRevision: preparation.AuthorizationRevision,
		CreatedAt: now, ExpiresAt: expiresAt, Limits: preparation.Limits,
		DeliveryProfile: cloneSubscriptionDeliveryProfile(c.profile),
	}
	snapshot, err := c.config.Broker.Establish(ctx, binding)
	if err != nil {
		return SubscriptionHandleEstablishment{}, normalizeSubscriptionBrokerError(err)
	}
	if len(snapshot.Data) == 0 || len(snapshot.Data) > preparation.Limits.MaxSnapshotBytes || snapshot.Cursor == "" || len(snapshot.Cursor) > streamCursorMaxBytes || snapshot.Position == 0 {
		_ = c.config.Broker.Revoke(context.WithoutCancel(ctx), binding)
		return SubscriptionHandleEstablishment{}, ErrSubscriptionBrokerUnavailable
	}
	record := &subscriptionHandleRecord{
		handle:  SubscriptionHandle{ID: id, State: SubscriptionHandleActive, CreatedAt: now, ExpiresAt: expiresAt, DeliveryProfile: cloneSubscriptionDeliveryProfile(c.profile)},
		binding: binding, snapshot: slices.Clone(snapshot.Data), cursor: snapshot.Cursor, position: snapshot.Position,
		fingerprint: request.Fingerprint, idemIndex: idemIndex, attachments: make(map[*managedSubscriptionSource]struct{}),
	}
	c.mu.Lock()
	if c.draining || len(c.handles) >= c.config.MaxHandles || uint64(len(record.snapshot)) > c.config.MaxTotalSnapshotBytes || c.snapshotBytes > c.config.MaxTotalSnapshotBytes-uint64(len(record.snapshot)) {
		c.mu.Unlock()
		_ = c.config.Broker.Revoke(context.WithoutCancel(ctx), binding)
		return SubscriptionHandleEstablishment{}, ErrSubscriptionCapacity
	}
	if _, collision := c.handles[id]; collision {
		c.mu.Unlock()
		_ = c.config.Broker.Revoke(context.WithoutCancel(ctx), binding)
		return SubscriptionHandleEstablishment{}, ErrSubscriptionCapacity
	}
	c.handles[id] = record
	c.snapshotBytes += uint64(len(record.snapshot))
	if idemIndex != "" {
		c.idempotency[idemIndex] = id
	}
	result := cloneSubscriptionEstablishment(record, true)
	c.mu.Unlock()
	return result, nil
}

func (c *SubscriptionHandleCoordinator) Observe(ctx context.Context, id string) (SubscriptionHandle, error) {
	record, err := c.authorize(ctx, id, SubscriptionHandleObserveAction)
	if err != nil {
		return SubscriptionHandle{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handles[id] != record || record.handle.State != SubscriptionHandleActive {
		return SubscriptionHandle{}, ErrSubscriptionHandleUnavailable
	}
	return cloneSubscriptionHandle(record.handle), nil
}

func (c *SubscriptionHandleCoordinator) Renew(ctx context.Context, id string) (SubscriptionHandle, error) {
	record, decision, err := c.authorizeDecision(ctx, id, SubscriptionHandleRenewAction)
	if err != nil {
		return SubscriptionHandle{}, err
	}
	now, err := callProcessClock(c.config.Now)
	if err != nil {
		return SubscriptionHandle{}, ErrSubscriptionHandleUnavailable
	}
	expiresAt := now.Add(c.config.HandleTTL)
	if decision.AuthenticationExpiresAt.Before(expiresAt) {
		expiresAt = decision.AuthenticationExpiresAt
	}
	c.mu.Lock()
	current := c.handles[id]
	if current != record || current.handle.State != SubscriptionHandleActive || !current.handle.ExpiresAt.After(now) {
		c.mu.Unlock()
		return SubscriptionHandle{}, ErrSubscriptionHandleUnavailable
	}
	current.handle.ExpiresAt = expiresAt
	current.binding.ExpiresAt = expiresAt
	handle := cloneSubscriptionHandle(current.handle)
	c.mu.Unlock()
	return handle, nil
}

func (c *SubscriptionHandleCoordinator) Cancel(ctx context.Context, id string) error {
	record, err := c.authorize(ctx, id, SubscriptionHandleCancelAction)
	if err != nil {
		return err
	}
	return c.removeRecord(context.WithoutCancel(ctx), record, SubscriptionHandleCancelled)
}

// Revoke is an application-owned control-plane action. It never discloses
// whether an identifier existed and is therefore idempotent.
func (c *SubscriptionHandleCoordinator) Revoke(ctx context.Context, id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	record := c.handles[id]
	c.mu.Unlock()
	if record != nil {
		_ = c.removeRecord(context.WithoutCancel(ctx), record, SubscriptionHandleRevoked)
	}
}

// Open authenticates the handle and returns a source that reauthorizes every
// protected delivery. An empty cursor means the establishment cursor; URLs do
// not need to carry cursor authority for an initial native EventSource attach.
func (c *SubscriptionHandleCoordinator) Open(ctx context.Context, id, cursor string) (StreamSource, error) {
	action := SubscriptionHandleOpenAction
	if cursor != "" {
		action = SubscriptionHandleResumeAction
	}
	record, decision, err := c.authorizeDecision(ctx, id, action)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.handles[id] != record || record.handle.State != SubscriptionHandleActive {
		c.mu.Unlock()
		return nil, ErrSubscriptionHandleUnavailable
	}
	binding := cloneSubscriptionBinding(record.binding)
	snapshot := slices.Clone(record.snapshot)
	position := record.position
	initialCursor := record.cursor
	handleExpiry := record.handle.ExpiresAt
	c.mu.Unlock()
	if cursor == "" {
		cursor = initialCursor
	}
	source, err := c.config.Broker.Open(ctx, binding, cursor)
	if err != nil {
		return nil, normalizeSubscriptionBrokerError(err)
	}
	deadline := handleExpiry
	if decision.AuthenticationExpiresAt.Before(deadline) {
		deadline = decision.AuthenticationExpiresAt
	}
	connectionDeadline := time.Now().Add(binding.DeliveryProfile.MaxConnectionLifetime)
	if connectionDeadline.Before(deadline) {
		deadline = connectionDeadline
	}
	principal, _ := PrincipalFromContext(ctx)
	session, err := NewStreamSourceSession(StreamSourceSessionConfig{
		Stream: id, ProfileVersion: protocol.StreamProfileVersion, SchemaRevision: binding.SchemaRevision,
		Advertisement: subscriptionAdvertisement(binding.DeliveryProfile), AuthenticationExpiresAt: deadline,
		Source: source, CheckExecutor: c.config.CheckExecutor, Limits: c.config.StreamLimits, Session: binding.Limits.Session,
		ResumeSnapshot: snapshot, ResumePosition: position,
		ValidateSchema: func(checkCtx context.Context, revision string) error {
			if revision != binding.SchemaRevision {
				return ErrStreamSchemaRetired
			}
			return c.validateDeliveryBinding(checkCtx, binding, principal)
		},
		Reauthorize: func(checkCtx context.Context, _ protocol.StreamFrame) error {
			return c.validateDeliveryBinding(checkCtx, binding, principal)
		},
	})
	if err != nil {
		_ = source.Close()
		return nil, normalizeSubscriptionBrokerError(err)
	}
	managed := &managedSubscriptionSource{coordinator: c, record: record, source: session}
	c.mu.Lock()
	current := c.handles[id]
	capacityExceeded := c.attachments >= c.config.MaxAttachments
	if current != record || current.handle.State != SubscriptionHandleActive || capacityExceeded {
		c.mu.Unlock()
		_ = session.Close()
		if capacityExceeded {
			return nil, ErrSubscriptionCapacity
		}
		return nil, ErrSubscriptionHandleUnavailable
	}
	record.attachments[managed] = struct{}{}
	c.attachments++
	c.mu.Unlock()
	return managed, nil
}

// CollectExpired removes at most limit expired records. Zero uses the bounded
// configured batch. Removed records are revoked outside the coordinator lock.
func (c *SubscriptionHandleCoordinator) CollectExpired(ctx context.Context, limit int) int {
	if c == nil {
		return 0
	}
	if limit <= 0 || limit > c.config.GarbageCollectionBatch {
		limit = c.config.GarbageCollectionBatch
	}
	now, err := callProcessClock(c.config.Now)
	if err != nil {
		return 0
	}
	c.mu.Lock()
	removed := c.collectExpiredLocked(now, limit)
	attachments := make([][]*managedSubscriptionSource, len(removed))
	for index, record := range removed {
		attachments[index] = c.takeAttachmentsLocked(record)
	}
	c.mu.Unlock()
	for index, record := range removed {
		closeSubscriptionAttachments(attachments[index])
		_ = c.config.Broker.Revoke(context.WithoutCancel(ctx), record.binding)
	}
	return len(removed)
}

// Drain prevents establishment, revokes all handles, and releases every
// attachment. Client reconnect deadlines are already staggered by the bound
// delivery profile, so graceful rotations do not require a synchronized cliff.
func (c *SubscriptionHandleCoordinator) Drain(ctx context.Context) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	c.draining = true
	records := make([]*subscriptionHandleRecord, 0, len(c.handles))
	for _, record := range c.handles {
		record.handle.State = SubscriptionHandleDraining
		records = append(records, record)
	}
	attachments := make([][]*managedSubscriptionSource, len(records))
	for index, record := range records {
		attachments[index] = c.takeAttachmentsLocked(record)
	}
	c.handles = make(map[string]*subscriptionHandleRecord)
	c.idempotency = make(map[string]string)
	c.snapshotBytes = 0
	c.mu.Unlock()
	for index, record := range records {
		closeSubscriptionAttachments(attachments[index])
		_ = c.config.Broker.Revoke(context.WithoutCancel(ctx), record.binding)
	}
	return len(records)
}

func (c *SubscriptionHandleCoordinator) authorize(ctx context.Context, id string, action SubscriptionHandleAction) (*subscriptionHandleRecord, error) {
	record, _, err := c.authorizeDecision(ctx, id, action)
	return record, err
}

func (c *SubscriptionHandleCoordinator) authorizeDecision(ctx context.Context, id string, action SubscriptionHandleAction) (*subscriptionHandleRecord, SubscriptionHandleAuthorizationDecision, error) {
	if c == nil || ctx == nil || !validSubscriptionIdentity(id) {
		return nil, SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	now, err := callProcessClock(c.config.Now)
	if err != nil {
		return nil, SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	c.mu.Lock()
	record := c.handles[id]
	if record == nil || record.handle.State != SubscriptionHandleActive || principal.Subject != record.binding.Principal || principal.Tenant != record.binding.Tenant {
		c.mu.Unlock()
		return nil, SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	if !record.handle.ExpiresAt.After(now) {
		delete(c.handles, id)
		if record.idemIndex != "" {
			delete(c.idempotency, record.idemIndex)
		}
		c.snapshotBytes -= uint64(len(record.snapshot))
		attachments := c.takeAttachmentsLocked(record)
		c.mu.Unlock()
		closeSubscriptionAttachments(attachments)
		_ = c.config.Broker.Revoke(context.WithoutCancel(ctx), record.binding)
		return nil, SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	binding := cloneSubscriptionBinding(record.binding)
	c.mu.Unlock()
	decision, err := c.authorizeBinding(ctx, binding, principal, action)
	if err != nil {
		return nil, SubscriptionHandleAuthorizationDecision{}, err
	}
	return record, decision, nil
}

func (c *SubscriptionHandleCoordinator) authorizeBinding(ctx context.Context, binding SubscriptionHandleBinding, principal Principal, action SubscriptionHandleAction) (SubscriptionHandleAuthorizationDecision, error) {
	if principal.Subject != binding.Principal || principal.Tenant != binding.Tenant {
		return SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	decision, err := c.config.Authorizer.AuthorizeSubscriptionHandle(ctx, SubscriptionHandleAuthorizationRequest{Action: action, Principal: principal, Binding: cloneSubscriptionBinding(binding)})
	now, clockErr := callProcessClock(c.config.Now)
	if err != nil || clockErr != nil || !decision.Allowed || decision.SchemaRevision != binding.SchemaRevision || decision.AuthorizationRevision != binding.AuthorizationRevision || decision.AuthenticationExpiresAt.IsZero() || !decision.AuthenticationExpiresAt.After(now) {
		return SubscriptionHandleAuthorizationDecision{}, ErrSubscriptionHandleUnavailable
	}
	return decision, nil
}

func (c *SubscriptionHandleCoordinator) validateDeliveryBinding(ctx context.Context, binding SubscriptionHandleBinding, principal Principal) error {
	if principal.Subject != binding.Principal || principal.Tenant != binding.Tenant {
		return ErrStreamAuthorizationRevoked
	}
	decision, err := c.config.Authorizer.AuthorizeSubscriptionHandle(ctx, SubscriptionHandleAuthorizationRequest{
		Action: SubscriptionHandleDeliverAction, Principal: principal, Binding: cloneSubscriptionBinding(binding),
	})
	now, clockErr := callProcessClock(c.config.Now)
	if err != nil || clockErr != nil || !decision.Allowed || decision.AuthorizationRevision != binding.AuthorizationRevision ||
		decision.AuthenticationExpiresAt.IsZero() || !decision.AuthenticationExpiresAt.After(now) {
		return ErrStreamAuthorizationRevoked
	}
	if decision.SchemaRevision != binding.SchemaRevision {
		return ErrStreamSchemaRetired
	}
	return nil
}

func (c *SubscriptionHandleCoordinator) removeRecord(ctx context.Context, record *subscriptionHandleRecord, state SubscriptionHandleState) error {
	c.mu.Lock()
	current := c.handles[record.handle.ID]
	if current != record {
		c.mu.Unlock()
		return ErrSubscriptionHandleUnavailable
	}
	record.handle.State = state
	delete(c.handles, record.handle.ID)
	if record.idemIndex != "" {
		delete(c.idempotency, record.idemIndex)
	}
	c.snapshotBytes -= uint64(len(record.snapshot))
	attachments := c.takeAttachmentsLocked(record)
	c.mu.Unlock()
	closeSubscriptionAttachments(attachments)
	if err := c.config.Broker.Revoke(ctx, record.binding); err != nil {
		return normalizeSubscriptionBrokerError(err)
	}
	return nil
}

func (c *SubscriptionHandleCoordinator) detach(source *managedSubscriptionSource) {
	c.mu.Lock()
	if _, ok := source.record.attachments[source]; ok {
		delete(source.record.attachments, source)
		c.attachments--
	}
	c.mu.Unlock()
}

func (c *SubscriptionHandleCoordinator) collectExpiredLocked(now time.Time, limit int) []*subscriptionHandleRecord {
	removed := make([]*subscriptionHandleRecord, 0, limit)
	for id, record := range c.handles {
		if len(removed) == limit {
			break
		}
		if record.handle.ExpiresAt.After(now) {
			continue
		}
		delete(c.handles, id)
		if record.idemIndex != "" {
			delete(c.idempotency, record.idemIndex)
		}
		c.snapshotBytes -= uint64(len(record.snapshot))
		removed = append(removed, record)
	}
	return removed
}

func (c *SubscriptionHandleCoordinator) idempotentResultLocked(index, fingerprint string) (*subscriptionHandleRecord, bool) {
	id := c.idempotency[index]
	record := c.handles[id]
	if record == nil {
		return nil, false
	}
	if record.fingerprint != fingerprint {
		return nil, true
	}
	return record, false
}

func (c *SubscriptionHandleCoordinator) finishPendingEstablishment(index string) {
	c.mu.Lock()
	if pending := c.pending[index]; pending != nil {
		delete(c.pending, index)
		close(pending.done)
	}
	c.mu.Unlock()
}

type managedSubscriptionSource struct {
	closeOnce   sync.Once
	coordinator *SubscriptionHandleCoordinator
	record      *subscriptionHandleRecord
	source      StreamSource
}

func (s *managedSubscriptionSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	frame, err := s.source.Next(ctx)
	if err != nil || frame.Type == protocol.StreamComplete || (frame.Type == protocol.StreamError && frame.Final) {
		_ = s.Close()
	}
	return frame, err
}

func (s *managedSubscriptionSource) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		closeErr = s.source.Close()
		s.coordinator.detach(s)
	})
	return closeErr
}

func (c *SubscriptionHandleCoordinator) takeAttachmentsLocked(record *subscriptionHandleRecord) []*managedSubscriptionSource {
	attachments := make([]*managedSubscriptionSource, 0, len(record.attachments))
	for attachment := range record.attachments {
		attachments = append(attachments, attachment)
		delete(record.attachments, attachment)
		c.attachments--
	}
	return attachments
}

func closeSubscriptionAttachments(attachments []*managedSubscriptionSource) {
	for _, attachment := range attachments {
		_ = attachment.Close()
	}
}

func validateSubscriptionPreparation(preparation SubscriptionHandlePreparation, now time.Time) error {
	preparation.Limits.Session = resolveStreamSessionLimits(preparation.Limits.Session)
	if !validSubscriptionIdentity(preparation.OperationIdentity) || !validSubscriptionIdentity(preparation.VariableIdentity) ||
		!validSubscriptionIdentity(preparation.SchemaRevision) || !validSubscriptionIdentity(preparation.AuthorizationRevision) ||
		preparation.AuthenticationExpiresAt.IsZero() || !preparation.AuthenticationExpiresAt.After(now) || preparation.Limits.MaxSnapshotBytes <= 0 ||
		preparation.Limits.Session.MaxDuration <= 0 || preparation.Limits.Session.CheckTimeout <= 0 || preparation.Limits.Session.MaxEvents <= 0 || preparation.Limits.Session.MaxBytes == 0 {
		return ErrInvalidSubscriptionHandleConfig
	}
	return nil
}

func validateSubscriptionDeliveryProfile(profile SubscriptionDeliveryProfile) error {
	if !validSubscriptionIdentity(profile.Name) || profile.MediaType != "text/event-stream" || profile.MaxConnectionLifetime <= 0 || profile.ReconnectStagger < 0 || profile.ReconnectStagger >= profile.MaxConnectionLifetime || profile.MaxReconnectAttempts <= 0 || len(profile.BrowserAuthentication) == 0 {
		return ErrInvalidSubscriptionHandleConfig
	}
	if profile.Replay != protocol.StreamReplayNone && profile.Replay != protocol.StreamReplayBounded && profile.Replay != protocol.StreamReplayDurable {
		return ErrInvalidSubscriptionHandleConfig
	}
	if profile.Replay == protocol.StreamReplayNone {
		if profile.RetentionHorizon != 0 || profile.LossDetection {
			return ErrInvalidSubscriptionHandleConfig
		}
	} else if profile.RetentionHorizon <= 0 || !profile.LossDetection {
		return ErrInvalidSubscriptionHandleConfig
	}
	seen := make(map[SubscriptionBrowserAuthentication]bool, len(profile.BrowserAuthentication))
	for _, path := range profile.BrowserAuthentication {
		if (path != SubscriptionFetchBearer && path != SubscriptionSameOriginCookie) || seen[path] {
			return ErrInvalidSubscriptionHandleConfig
		}
		seen[path] = true
	}
	return nil
}

func validRequestedSubscriptionAuthentication(requested, supported []SubscriptionBrowserAuthentication) bool {
	if len(requested) == 0 {
		return false
	}
	available := make(map[SubscriptionBrowserAuthentication]bool, len(supported))
	for _, path := range supported {
		available[path] = true
	}
	seen := make(map[SubscriptionBrowserAuthentication]bool, len(requested))
	for _, path := range requested {
		if seen[path] || !available[path] {
			return false
		}
		seen[path] = true
	}
	return true
}

func subscriptionAdvertisement(profile SubscriptionDeliveryProfile) protocol.StreamSourceAdvertisement {
	retention := profile.Name
	maxReplayEvents := 1
	maxReplayBytes := uint64(1)
	if profile.Replay == protocol.StreamReplayNone {
		retention = "none"
		maxReplayEvents = 0
		maxReplayBytes = 0
	}
	return protocol.StreamSourceAdvertisement{
		ProfileVersion: protocol.StreamProfileVersion, Replay: profile.Replay,
		Consistency: protocol.StreamSnapshotStable, RetentionPolicy: retention,
		MaxReplayEvents: maxReplayEvents, MaxReplayBytes: maxReplayBytes, DisclosesEarliestPosition: false,
		HistoryRecovery: protocol.StreamRecoveryRefetch,
	}
}

func subscriptionIdempotencyIndex(principal Principal, key string) string {
	if key == "" {
		return ""
	}
	return principal.Tenant + "\x00" + principal.Subject + "\x00" + key
}

func cloneSubscriptionEstablishment(record *subscriptionHandleRecord, created bool) SubscriptionHandleEstablishment {
	return SubscriptionHandleEstablishment{Handle: cloneSubscriptionHandle(record.handle), Snapshot: slices.Clone(record.snapshot), ReconciliationCursor: record.cursor, Created: created}
}

func cloneSubscriptionHandle(handle SubscriptionHandle) SubscriptionHandle {
	handle.DeliveryProfile = cloneSubscriptionDeliveryProfile(handle.DeliveryProfile)
	return handle
}

func cloneSubscriptionBinding(binding SubscriptionHandleBinding) SubscriptionHandleBinding {
	binding.DeliveryProfile = cloneSubscriptionDeliveryProfile(binding.DeliveryProfile)
	return binding
}

func cloneSubscriptionDeliveryProfile(profile SubscriptionDeliveryProfile) SubscriptionDeliveryProfile {
	profile.BrowserAuthentication = slices.Clone(profile.BrowserAuthentication)
	return profile
}

func validSubscriptionIdentity(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validSubscriptionOptionalIdentity(value string) bool {
	return value == "" || validSubscriptionIdentity(value)
}

func normalizeSubscriptionBrokerError(err error) error {
	switch {
	case errors.Is(err, ErrSubscriptionHistoryUnavailable), errors.Is(err, ErrStreamHistoryUnavailable):
		return ErrSubscriptionHistoryUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.EOF):
		return err
	default:
		return ErrSubscriptionBrokerUnavailable
	}
}

func randomSubscriptionHandleID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("subscription handle entropy: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
