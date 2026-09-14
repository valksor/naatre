package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AsyncOperationState is the durable lifecycle state of accepted work.
type AsyncOperationState string

const (
	AsyncOperationPending       AsyncOperationState = "pending"
	AsyncOperationRunning       AsyncOperationState = "running"
	AsyncOperationSucceeded     AsyncOperationState = "succeeded"
	AsyncOperationFailed        AsyncOperationState = "failed"
	AsyncOperationCancelling    AsyncOperationState = "cancelling"
	AsyncOperationCancelled     AsyncOperationState = "cancelled"
	AsyncOperationIndeterminate AsyncOperationState = "indeterminate"
)

// Terminal reports whether no later state transition is permitted.
func (s AsyncOperationState) Terminal() bool {
	return s == AsyncOperationSucceeded || s == AsyncOperationFailed ||
		s == AsyncOperationCancelled || s == AsyncOperationIndeterminate
}

// ValidAsyncOperationTransition reports whether a state change is permitted.
// Metadata-only revisions are not state transitions and therefore return false
// when from and to are equal.
func ValidAsyncOperationTransition(from, to AsyncOperationState) bool {
	switch from {
	case AsyncOperationPending:
		return to == AsyncOperationRunning || to == AsyncOperationCancelled
	case AsyncOperationRunning:
		return to == AsyncOperationSucceeded || to == AsyncOperationFailed ||
			to == AsyncOperationCancelling || to == AsyncOperationIndeterminate
	case AsyncOperationCancelling:
		return to == AsyncOperationSucceeded || to == AsyncOperationFailed ||
			to == AsyncOperationCancelled || to == AsyncOperationIndeterminate
	case AsyncOperationSucceeded, AsyncOperationFailed, AsyncOperationCancelled, AsyncOperationIndeterminate:
		return false
	default:
		return false
	}
}

var (
	ErrAsyncOperationNotFound                = errors.New("async operation not found")
	ErrAsyncOperationUnavailable             = errors.New("async operation unavailable")
	ErrAsyncOperationIdempotencyConflict     = errors.New("async operation idempotency conflict")
	ErrAsyncOperationBusy                    = errors.New("async operation already owned")
	ErrAsyncOperationInvalidTransition       = errors.New("invalid async operation transition")
	ErrAsyncOperationSubscriptionUnsupported = errors.New("async operation subscription unsupported")
	ErrAsyncOperationProgressInvalid         = errors.New("invalid async operation progress")
	ErrAsyncOperationWorkerCrash             = errors.New("async operation worker crashed")
)

// AsyncOperationProgress is bounded JSON progress. Sequence increases by one
// for each accepted progress revision and is independent from State revision.
type AsyncOperationProgress struct {
	Sequence uint64
	Payload  json.RawMessage
}

// AsyncOperationHandle is the authorization-filtered durable resource exposed
// to clients. Binding and work payloads remain in AsyncOperationRecord.
type AsyncOperationHandle struct {
	ID               string
	State            AsyncOperationState
	Revision         uint64
	Progress         AsyncOperationProgress
	Result           *Outcome
	Errors           []ExecutionError
	ProjectionErrors []ExecutionError
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
	ExpiresAt        time.Time
}

// AsyncOperationBinding is persisted with a handle but is not part of the
// public handle. Stores use it for authorization and idempotent acceptance.
type AsyncOperationBinding struct {
	Principal             string
	Tenant                string
	AuthorizationRevision string
	SchemaRevision        string
	Operation             string
	IdempotencyKey        string
	Fingerprint           string
}

// AsyncOperationRecord is the application-store representation of accepted
// work. Payload is opaque to the coordinator and interpreted by the worker.
type AsyncOperationRecord struct {
	Handle  AsyncOperationHandle
	Binding AsyncOperationBinding
	Payload json.RawMessage
}

// AsyncOperationStore is the queue-neutral durable ownership boundary. Accept
// atomically persists a pending record and, when requested, its idempotency
// index. CompareAndSwap atomically accepts only expected State revisions.
type AsyncOperationStore interface {
	Durability() IdempotencyDurability
	Accept(context.Context, AsyncOperationRecord, bool) (AsyncOperationRecord, bool, error)
	Load(context.Context, string) (AsyncOperationRecord, error)
	Pending(context.Context, int) ([]AsyncOperationRecord, error)
	CompareAndSwap(context.Context, string, uint64, AsyncOperationHandle) (AsyncOperationRecord, bool, error)
	DeleteExpired(context.Context, time.Time, int) (int, error)
}

// AsyncOperationChangeStore optionally supports subscription/resume without
// prescribing a broker. Notifications contain no state; Poll remains the one
// authoritative logical view after every notification.
type AsyncOperationChangeStore interface {
	WaitForRevision(context.Context, string, uint64) error
}

type AsyncOperationAction string

const (
	AsyncOperationPollAction   AsyncOperationAction = "poll"
	AsyncOperationResumeAction AsyncOperationAction = "resume"
	AsyncOperationCancelAction AsyncOperationAction = "cancel"
)

type AsyncOperationAuthorizationRequest struct {
	Action    AsyncOperationAction
	Principal Principal
	Binding   AsyncOperationBinding
	HandleID  string
}

type AsyncOperationAuthorizer interface {
	AuthorizeAsyncOperation(context.Context, AsyncOperationAuthorizationRequest) (bool, error)
}

type AsyncOperationAuthorizerFunc func(context.Context, AsyncOperationAuthorizationRequest) (bool, error)

func (f AsyncOperationAuthorizerFunc) AuthorizeAsyncOperation(ctx context.Context, request AsyncOperationAuthorizationRequest) (bool, error) {
	return f(ctx, request)
}

type AsyncCancellationDisposition string

const (
	AsyncCancellationRequested   AsyncCancellationDisposition = "requested"
	AsyncCancellationEffective   AsyncCancellationDisposition = "effective"
	AsyncCancellationTooLate     AsyncCancellationDisposition = "too-late"
	AsyncCancellationUnsupported AsyncCancellationDisposition = "unsupported"
)

type AsyncOperationSubmission struct {
	Operation          string
	SchemaRevision     string
	IdempotencyKey     string
	Fingerprint        string
	RequireIdempotency bool
	Payload            json.RawMessage
}

type AsyncOperationHTTP struct {
	Status     int
	Location   string
	RetryAfter time.Duration
	ETag       string
}

type AsyncOperationAcceptance struct {
	Handle  AsyncOperationHandle
	Created bool
	HTTP    AsyncOperationHTTP
}

type AsyncOperationPoll struct {
	Handle      AsyncOperationHandle
	NotModified bool
	HTTP        AsyncOperationHTTP
}

type AsyncOperationCancellation struct {
	Handle      AsyncOperationHandle
	Disposition AsyncCancellationDisposition
}

type AsyncOperationWork struct {
	HandleID string
	Binding  AsyncOperationBinding
	Payload  json.RawMessage
}

type AsyncOperationCompletion struct {
	State            AsyncOperationState
	Result           *Outcome
	Errors           []ExecutionError
	ProjectionErrors []ExecutionError
}

type AsyncOperationControl interface {
	UpdateProgress(context.Context, json.RawMessage) (AsyncOperationHandle, error)
	CancellationRequested(context.Context) (bool, error)
}

type AsyncOperationWorker interface {
	Execute(context.Context, AsyncOperationWork, AsyncOperationControl) AsyncOperationCompletion
}

type AsyncOperationWorkerFunc func(context.Context, AsyncOperationWork, AsyncOperationControl) AsyncOperationCompletion

func (f AsyncOperationWorkerFunc) Execute(ctx context.Context, work AsyncOperationWork, control AsyncOperationControl) AsyncOperationCompletion {
	return f(ctx, work, control)
}

type AsyncOperationConfig struct {
	Store                 AsyncOperationStore
	Authorizer            AsyncOperationAuthorizer
	Now                   func() time.Time
	NewID                 func() (string, error)
	Location              func(string) string
	Retention             time.Duration
	RetryAfter            time.Duration
	MaxProgressBytes      int
	MaxProgressTokens     int
	CancellationSupported bool
}

type AsyncOperationCoordinator struct {
	config AsyncOperationConfig
}

func NewAsyncOperationCoordinator(config AsyncOperationConfig) (*AsyncOperationCoordinator, error) {
	switch {
	case config.Store == nil:
		return nil, errors.New("async operation store is nil")
	case config.Store.Durability() != IdempotencyDurable:
		return nil, errors.New("async operation store is not durable")
	case config.Authorizer == nil:
		return nil, errors.New("async operation authorizer is nil")
	case config.Now == nil:
		return nil, errors.New("async operation clock is nil")
	case config.NewID == nil:
		return nil, errors.New("async operation ID generator is nil")
	case config.Location == nil:
		return nil, errors.New("async operation location builder is nil")
	case config.Retention <= 0:
		return nil, errors.New("async operation retention must be positive")
	case config.RetryAfter <= 0:
		return nil, errors.New("async operation Retry-After must be positive")
	case config.MaxProgressBytes <= 0 || config.MaxProgressTokens <= 0:
		return nil, errors.New("async operation progress limits must be positive")
	default:
		return &AsyncOperationCoordinator{config: config}, nil
	}
}

func (c *AsyncOperationCoordinator) Submit(ctx context.Context, submission AsyncOperationSubmission) (AsyncOperationAcceptance, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Subject == "" || strings.TrimSpace(submission.Operation) == "" || strings.TrimSpace(submission.SchemaRevision) == "" {
		return AsyncOperationAcceptance{}, ErrAsyncOperationUnavailable
	}
	if submission.RequireIdempotency && (submission.IdempotencyKey == "" || submission.Fingerprint == "") {
		return AsyncOperationAcceptance{}, errors.New("async operation idempotency key and fingerprint are required")
	}
	id, err := c.config.NewID()
	if err != nil {
		return AsyncOperationAcceptance{}, fmt.Errorf("generate async operation ID: %w", err)
	}
	if !validAsyncReference(id) {
		return AsyncOperationAcceptance{}, errors.New("generated async operation ID is invalid")
	}
	now := c.config.Now().UTC()
	record := AsyncOperationRecord{
		Handle: AsyncOperationHandle{ID: id, State: AsyncOperationPending, Revision: 1, CreatedAt: now, UpdatedAt: now},
		Binding: AsyncOperationBinding{
			Principal: principal.Subject, Tenant: principal.Tenant, AuthorizationRevision: principal.AuthorizationRevision,
			SchemaRevision: submission.SchemaRevision, Operation: submission.Operation,
			IdempotencyKey: submission.IdempotencyKey, Fingerprint: submission.Fingerprint,
		},
		Payload: bytes.Clone(submission.Payload),
	}
	stored, created, err := c.config.Store.Accept(ctx, record, submission.RequireIdempotency)
	if err != nil {
		return AsyncOperationAcceptance{}, err
	}
	handle := cloneAsyncOperationHandle(stored.Handle)
	metadata := AsyncOperationHTTP{
		Status: http.StatusAccepted, Location: c.config.Location(handle.ID),
		RetryAfter: c.config.RetryAfter, ETag: asyncOperationETag(handle.Revision),
	}
	return AsyncOperationAcceptance{Handle: handle, Created: created, HTTP: metadata}, nil
}

func (c *AsyncOperationCoordinator) Poll(ctx context.Context, id, ifNoneMatch string) (AsyncOperationPoll, error) {
	record, err := c.authorizedRecord(ctx, id, AsyncOperationPollAction)
	if err != nil {
		return AsyncOperationPoll{}, err
	}
	handle := cloneAsyncOperationHandle(record.Handle)
	etag := asyncOperationETag(handle.Revision)
	if ifNoneMatch != "" && ifNoneMatch == etag {
		return AsyncOperationPoll{NotModified: true, HTTP: AsyncOperationHTTP{Status: http.StatusNotModified, ETag: etag}}, nil
	}
	return AsyncOperationPoll{Handle: handle, HTTP: c.pollHTTP(handle)}, nil
}

func (c *AsyncOperationCoordinator) Resume(ctx context.Context, id string, afterRevision uint64) (AsyncOperationPoll, error) {
	if _, err := c.authorizedRecord(ctx, id, AsyncOperationResumeAction); err != nil {
		return AsyncOperationPoll{}, err
	}
	changes, ok := c.config.Store.(AsyncOperationChangeStore)
	if !ok {
		return AsyncOperationPoll{}, ErrAsyncOperationSubscriptionUnsupported
	}
	if err := changes.WaitForRevision(ctx, id, afterRevision); err != nil {
		if errors.Is(err, ErrAsyncOperationNotFound) {
			return AsyncOperationPoll{}, ErrAsyncOperationUnavailable
		}
		return AsyncOperationPoll{}, err
	}
	// Poll is deliberately authoritative and performs a second authorization
	// after the wait, so subscription cannot retain stale access.
	return c.Poll(ctx, id, "")
}

func (c *AsyncOperationCoordinator) Cancel(ctx context.Context, id string) (AsyncOperationCancellation, error) {
	for {
		record, err := c.authorizedRecord(ctx, id, AsyncOperationCancelAction)
		if err != nil {
			return AsyncOperationCancellation{}, err
		}
		if record.Handle.State.Terminal() {
			return AsyncOperationCancellation{Handle: cloneAsyncOperationHandle(record.Handle), Disposition: AsyncCancellationTooLate}, nil
		}
		if !c.config.CancellationSupported {
			return AsyncOperationCancellation{Handle: cloneAsyncOperationHandle(record.Handle), Disposition: AsyncCancellationUnsupported}, nil
		}
		if record.Handle.State == AsyncOperationCancelling {
			return AsyncOperationCancellation{Handle: cloneAsyncOperationHandle(record.Handle), Disposition: AsyncCancellationRequested}, nil
		}
		now := c.config.Now().UTC()
		next := cloneAsyncOperationHandle(record.Handle)
		disposition := AsyncCancellationRequested
		switch next.State {
		case AsyncOperationPending:
			next.State = AsyncOperationCancelled
			next.FinishedAt = now
			next.ExpiresAt = now.Add(c.config.Retention)
			disposition = AsyncCancellationEffective
		case AsyncOperationRunning:
			next.State = AsyncOperationCancelling
		case AsyncOperationSucceeded, AsyncOperationFailed, AsyncOperationCancelling, AsyncOperationCancelled, AsyncOperationIndeterminate:
			return AsyncOperationCancellation{}, ErrAsyncOperationInvalidTransition
		default:
			return AsyncOperationCancellation{}, ErrAsyncOperationInvalidTransition
		}
		next.Revision++
		next.UpdatedAt = now
		updated, swapped, err := c.config.Store.CompareAndSwap(ctx, id, record.Handle.Revision, next)
		if err != nil {
			return AsyncOperationCancellation{}, err
		}
		if swapped {
			return AsyncOperationCancellation{Handle: cloneAsyncOperationHandle(updated.Handle), Disposition: disposition}, nil
		}
	}
}

func (c *AsyncOperationCoordinator) Run(ctx context.Context, id string, worker AsyncOperationWorker) (handle AsyncOperationHandle, err error) {
	if worker == nil {
		return AsyncOperationHandle{}, errors.New("async operation worker is nil")
	}
	record, err := c.config.Store.Load(ctx, id)
	if err != nil {
		return AsyncOperationHandle{}, err
	}
	if record.Handle.State.Terminal() {
		return cloneAsyncOperationHandle(record.Handle), nil
	}
	if record.Handle.State != AsyncOperationPending {
		return AsyncOperationHandle{}, ErrAsyncOperationBusy
	}
	now := c.config.Now().UTC()
	running := cloneAsyncOperationHandle(record.Handle)
	running.State = AsyncOperationRunning
	running.Revision++
	running.UpdatedAt = now
	running.StartedAt = now
	claimed, swapped, err := c.config.Store.CompareAndSwap(ctx, id, record.Handle.Revision, running)
	if err != nil {
		return AsyncOperationHandle{}, err
	}
	if !swapped {
		return AsyncOperationHandle{}, ErrAsyncOperationBusy
	}

	work := AsyncOperationWork{HandleID: id, Binding: claimed.Binding, Payload: bytes.Clone(claimed.Payload)}
	control := asyncOperationControl{coordinator: c, id: id}
	completion, crashed := invokeAsyncOperationWorker(ctx, worker, work, control)
	if crashed != nil {
		completion = AsyncOperationCompletion{State: AsyncOperationIndeterminate}
	}
	handle, completeErr := c.complete(ctx, id, completion)
	if completeErr != nil {
		return handle, completeErr
	}
	if crashed != nil {
		return handle, fmt.Errorf("%w: %v", ErrAsyncOperationWorkerCrash, crashed)
	}
	return handle, nil
}

// Recover discovers accepted pending work from durable storage and executes it
// without relying on state retained by the accepting process. Stores return a
// bounded deterministic page; distributed leases and pagination are supplied
// by the adapters owned by issue #89.
func (c *AsyncOperationCoordinator) Recover(ctx context.Context, limit int, worker AsyncOperationWorker) ([]AsyncOperationHandle, error) {
	if limit <= 0 {
		return nil, errors.New("async operation recovery limit must be positive")
	}
	if worker == nil {
		return nil, errors.New("async operation worker is nil")
	}
	records, err := c.config.Store.Pending(ctx, limit)
	if err != nil {
		return nil, err
	}
	handles := make([]AsyncOperationHandle, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return handles, err
		}
		handle, err := c.Run(ctx, record.Handle.ID, worker)
		if errors.Is(err, ErrAsyncOperationBusy) {
			continue
		}
		if err != nil {
			return handles, err
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func (c *AsyncOperationCoordinator) GarbageCollect(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("async operation garbage collection limit must be positive")
	}
	return c.config.Store.DeleteExpired(ctx, c.config.Now().UTC(), limit)
}

func (c *AsyncOperationCoordinator) complete(ctx context.Context, id string, completion AsyncOperationCompletion) (AsyncOperationHandle, error) {
	if !completion.State.Terminal() || completion.State == AsyncOperationCancelled && len(completion.Errors) > 0 {
		return AsyncOperationHandle{}, ErrAsyncOperationInvalidTransition
	}
	for {
		record, err := c.config.Store.Load(ctx, id)
		if err != nil {
			return AsyncOperationHandle{}, err
		}
		if record.Handle.State.Terminal() {
			return cloneAsyncOperationHandle(record.Handle), nil
		}
		if !ValidAsyncOperationTransition(record.Handle.State, completion.State) {
			return AsyncOperationHandle{}, ErrAsyncOperationInvalidTransition
		}
		now := c.config.Now().UTC()
		next := cloneAsyncOperationHandle(record.Handle)
		next.State = completion.State
		next.Revision++
		next.UpdatedAt = now
		next.FinishedAt = now
		next.ExpiresAt = now.Add(c.config.Retention)
		next.Errors = cloneExecutionErrors(completion.Errors)
		next.ProjectionErrors = cloneExecutionErrors(completion.ProjectionErrors)
		if completion.Result != nil {
			result := cloneOutcome(*completion.Result)
			next.Result = &result
		} else {
			next.Result = nil
		}
		updated, swapped, err := c.config.Store.CompareAndSwap(ctx, id, record.Handle.Revision, next)
		if err != nil {
			return AsyncOperationHandle{}, err
		}
		if swapped {
			return cloneAsyncOperationHandle(updated.Handle), nil
		}
	}
}

func (c *AsyncOperationCoordinator) updateProgress(ctx context.Context, id string, payload json.RawMessage) (AsyncOperationHandle, error) {
	if err := validateAsyncProgress(payload, c.config.MaxProgressBytes, c.config.MaxProgressTokens); err != nil {
		return AsyncOperationHandle{}, err
	}
	for {
		record, err := c.config.Store.Load(ctx, id)
		if err != nil {
			return AsyncOperationHandle{}, err
		}
		if record.Handle.State != AsyncOperationRunning && record.Handle.State != AsyncOperationCancelling {
			return AsyncOperationHandle{}, ErrAsyncOperationInvalidTransition
		}
		next := cloneAsyncOperationHandle(record.Handle)
		next.Revision++
		next.UpdatedAt = c.config.Now().UTC()
		next.Progress.Sequence++
		next.Progress.Payload = bytes.Clone(payload)
		updated, swapped, err := c.config.Store.CompareAndSwap(ctx, id, record.Handle.Revision, next)
		if err != nil {
			return AsyncOperationHandle{}, err
		}
		if swapped {
			return cloneAsyncOperationHandle(updated.Handle), nil
		}
	}
}

func (c *AsyncOperationCoordinator) authorizedRecord(ctx context.Context, id string, action AsyncOperationAction) (AsyncOperationRecord, error) {
	record, err := c.config.Store.Load(ctx, id)
	if err != nil {
		return AsyncOperationRecord{}, ErrAsyncOperationUnavailable
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Subject != record.Binding.Principal || principal.Tenant != record.Binding.Tenant {
		return AsyncOperationRecord{}, ErrAsyncOperationUnavailable
	}
	if record.Handle.State.Terminal() && !record.Handle.ExpiresAt.After(c.config.Now().UTC()) {
		return AsyncOperationRecord{}, ErrAsyncOperationUnavailable
	}
	allowed, err := c.config.Authorizer.AuthorizeAsyncOperation(ctx, AsyncOperationAuthorizationRequest{
		Action: action, Principal: principal, Binding: record.Binding, HandleID: id,
	})
	if err != nil || !allowed {
		return AsyncOperationRecord{}, ErrAsyncOperationUnavailable
	}
	return record, nil
}

func (c *AsyncOperationCoordinator) pollHTTP(handle AsyncOperationHandle) AsyncOperationHTTP {
	metadata := AsyncOperationHTTP{Status: http.StatusOK, Location: c.config.Location(handle.ID), ETag: asyncOperationETag(handle.Revision)}
	if !handle.State.Terminal() {
		metadata.RetryAfter = c.config.RetryAfter
	}
	return metadata
}

type asyncOperationControl struct {
	coordinator *AsyncOperationCoordinator
	id          string
}

func (c asyncOperationControl) UpdateProgress(ctx context.Context, payload json.RawMessage) (AsyncOperationHandle, error) {
	return c.coordinator.updateProgress(ctx, c.id, payload)
}

func (c asyncOperationControl) CancellationRequested(ctx context.Context) (bool, error) {
	record, err := c.coordinator.config.Store.Load(ctx, c.id)
	if err != nil {
		return false, err
	}
	return record.Handle.State == AsyncOperationCancelling || record.Handle.State == AsyncOperationCancelled, nil
}

func invokeAsyncOperationWorker(ctx context.Context, worker AsyncOperationWorker, work AsyncOperationWork, control AsyncOperationControl) (completion AsyncOperationCompletion, crashed any) {
	defer func() { crashed = recover() }()
	completion = worker.Execute(ctx, work, control)
	return completion, nil
}

func validateAsyncProgress(payload json.RawMessage, maximumBytes, maximumTokens int) error {
	if len(payload) == 0 || len(payload) > maximumBytes {
		return ErrAsyncOperationProgressInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || countAsyncProgressTokens(value, maximumTokens) > maximumTokens {
		return ErrAsyncOperationProgressInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrAsyncOperationProgressInvalid
	}
	return nil
}

func countAsyncProgressTokens(value any, limit int) int {
	count := 1
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			count += countAsyncProgressTokens(item, limit-count)
			if count > limit {
				return count
			}
		}
	case map[string]any:
		for _, item := range typed {
			count++ // object member name
			count += countAsyncProgressTokens(item, limit-count)
			if count > limit {
				return count
			}
		}
	}
	return count
}

func validAsyncReference(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func asyncOperationETag(revision uint64) string {
	return `"naatre-async-` + strconv.FormatUint(revision, 10) + `"`
}

func cloneAsyncOperationHandle(input AsyncOperationHandle) AsyncOperationHandle {
	output := input
	output.Progress.Payload = bytes.Clone(input.Progress.Payload)
	output.Errors = cloneExecutionErrors(input.Errors)
	output.ProjectionErrors = cloneExecutionErrors(input.ProjectionErrors)
	if input.Result != nil {
		result := cloneOutcome(*input.Result)
		output.Result = &result
	}
	return output
}

func cloneExecutionErrors(input []ExecutionError) []ExecutionError {
	if len(input) == 0 {
		return nil
	}
	outcome := cloneOutcome(Outcome{Errors: input})
	return outcome.Errors
}
