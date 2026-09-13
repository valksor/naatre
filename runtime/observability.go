package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
)

// TelemetryKind identifies one bounded lifecycle category.
type TelemetryKind string

const (
	TelemetryRequest      TelemetryKind = "request"
	TelemetryPlanning     TelemetryKind = "planning"
	TelemetryOperation    TelemetryKind = "operation"
	TelemetryHandler      TelemetryKind = "handler"
	TelemetryBatch        TelemetryKind = "batch"
	TelemetryRetry        TelemetryKind = "retry"
	TelemetryTransaction  TelemetryKind = "transaction"
	TelemetrySubscription TelemetryKind = "subscription"
)

// TelemetryStage identifies one lifecycle transition. Package-defined stages
// form a closed vocabulary suitable for bounded metric labels.
type TelemetryStage string

const (
	TelemetryStarted   TelemetryStage = "started"
	TelemetryCompleted TelemetryStage = "completed"
	TelemetryFailed    TelemetryStage = "failed"
	TelemetryCancelled TelemetryStage = "cancelled"
)

// TelemetryOutcome is a bounded result classification.
type TelemetryOutcome string

const (
	TelemetryActive               TelemetryOutcome = "active"
	TelemetrySucceeded            TelemetryOutcome = "completed"
	TelemetryFailedOutcome        TelemetryOutcome = "failed"
	TelemetryCancelledOutcome     TelemetryOutcome = "cancelled"
	TelemetryDeniedOutcome        TelemetryOutcome = "denied"
	TelemetryHistoryUnavailable   TelemetryOutcome = "history-unavailable"
	TelemetryAuthorizationExpired TelemetryOutcome = "authorization-expired"
	TelemetrySlowConsumer         TelemetryOutcome = "slow-consumer"
	TelemetryBrokerFailure        TelemetryOutcome = "broker-failure"
	TelemetryForcedDrain          TelemetryOutcome = "forced-drain"
)

// TelemetryEvent is safe for traces and structured logs. It intentionally has
// no raw variables, arguments, results, tokens, principal, cursor, topic, or
// subscription handle fields. References are application-supplied opaque IDs.
type TelemetryEvent struct {
	Kind               TelemetryKind          `json:"kind"`
	Stage              TelemetryStage         `json:"stage"`
	ID                 string                 `json:"id"`
	ParentID           string                 `json:"parentId,omitempty"`
	Links              []string               `json:"links,omitempty"`
	RequestID          string                 `json:"requestId,omitempty"`
	OperationID        string                 `json:"operationId,omitempty"`
	OperationName      string                 `json:"operationName,omitempty"`
	OperationKind      protocol.OperationKind `json:"operationKind,omitempty"`
	Handler            string                 `json:"handler,omitempty"`
	MemberKind         MemberKind             `json:"memberKind,omitempty"`
	SchemaRevision     string                 `json:"schemaRevision,omitempty"`
	PersistedHash      string                 `json:"persistedHash,omitempty"`
	PrincipalReference string                 `json:"principalReference,omitempty"`
	StreamReference    string                 `json:"streamReference,omitempty"`
	ConnectionAttempt  uint32                 `json:"connectionAttempt,omitempty"`
	ReplayAttempt      uint32                 `json:"replayAttempt,omitempty"`
	Attempt            uint32                 `json:"attempt,omitempty"`
	BatchSize          int                    `json:"batchSize,omitempty"`
	ActiveStreams      int64                  `json:"activeStreams,omitempty"`
	ReplayEvents       uint64                 `json:"replayEvents,omitempty"`
	ReplayBytes        uint64                 `json:"replayBytes,omitempty"`
	ScannedCandidates  uint64                 `json:"scannedCandidates,omitempty"`
	DrainRemaining     uint64                 `json:"drainRemaining,omitempty"`
	Cost               uint64                 `json:"cost"`
	Duration           time.Duration          `json:"duration,omitempty"`
	Outcome            TelemetryOutcome       `json:"outcome,omitempty"`
	ErrorCode          string                 `json:"errorCode,omitempty"`
}

// MetricEvent contains bounded dimensions plus numeric measurements.
// Correlation IDs, names, tenant values, principal references, and handler
// identifiers are excluded.
type MetricEvent struct {
	Kind              TelemetryKind          `json:"kind"`
	Stage             TelemetryStage         `json:"stage"`
	OperationKind     protocol.OperationKind `json:"operationKind,omitempty"`
	Outcome           TelemetryOutcome       `json:"outcome,omitempty"`
	ErrorCode         string                 `json:"errorCode,omitempty"`
	Duration          time.Duration          `json:"duration,omitempty"`
	Cost              uint64                 `json:"cost"`
	BatchSize         int                    `json:"batchSize,omitempty"`
	Attempt           uint32                 `json:"attempt,omitempty"`
	ConnectionAttempt uint32                 `json:"connectionAttempt,omitempty"`
	ReplayAttempt     uint32                 `json:"replayAttempt,omitempty"`
	ActiveStreams     int64                  `json:"activeStreams,omitempty"`
	ReplayEvents      uint64                 `json:"replayEvents,omitempty"`
	ReplayBytes       uint64                 `json:"replayBytes,omitempty"`
	ScannedCandidates uint64                 `json:"scannedCandidates,omitempty"`
	DrainRemaining    uint64                 `json:"drainRemaining,omitempty"`
}

type TraceHook func(TelemetryEvent) error
type MetricHook func(MetricEvent) error
type LogHook func(TelemetryEvent) error

// TelemetryFailure reports an isolated application-hook failure.
type TelemetryFailure struct {
	Pillar   string         `json:"pillar"`
	Kind     TelemetryKind  `json:"kind"`
	Stage    TelemetryStage `json:"stage"`
	Message  string         `json:"message"`
	Panicked bool           `json:"panicked,omitempty"`
}

type TelemetryFailureHook func(TelemetryFailure)

// TelemetryHooks are dependency-free adapters for traces, metrics, and logs.
// Hooks must be concurrency-safe. Their errors and panics never alter results.
type TelemetryHooks struct {
	Trace   TraceHook
	Metric  MetricHook
	Log     LogHook
	Failure TelemetryFailureHook
}

// TelemetryOptions supplies safe correlation metadata and application hooks.
type TelemetryOptions struct {
	RequestID          string
	OperationID        string
	SchemaRevision     string
	PersistedHash      string
	PrincipalReference string
	Hooks              TelemetryHooks
}

type executionTelemetry struct {
	options       TelemetryOptions
	operationName string
	operationKind protocol.OperationKind
	staticCost    uint64
	requestSpan   string
	operationSpan string
	sequence      atomic.Uint64
	parentMu      sync.RWMutex
	parent        string
}

type planningTelemetry struct {
	execution *executionTelemetry
	id        string
	started   time.Time
}

var telemetryFallbackSequence atomic.Uint64

func startPlanningTelemetry(options TelemetryOptions) *planningTelemetry {
	execution := newExecutionTelemetry(options, "", "", 0)
	if execution == nil {
		return nil
	}
	planning := &planningTelemetry{
		execution: execution,
		id:        telemetryID("planning", execution.requestSpan),
		started:   time.Now(),
	}
	event := execution.baseEvent(TelemetryPlanning, TelemetryStarted)
	event.ID = planning.id
	emitTelemetry(execution.options, event)
	return planning
}

func (p *planningTelemetry) setOperation(name string, kind protocol.OperationKind) {
	if p != nil {
		p.execution.operationName, p.execution.operationKind = name, kind
	}
}

func (p *planningTelemetry) setCost(cost uint64) {
	if p != nil {
		p.execution.staticCost = cost
	}
}

func (p *planningTelemetry) finish(err error) {
	if p == nil {
		return
	}
	stage, outcome, code := TelemetryCompleted, TelemetrySucceeded, ""
	if err != nil {
		stage, outcome, code = TelemetryFailed, TelemetryFailedOutcome, CodeValidationFailed
	}
	event := p.execution.baseEvent(TelemetryPlanning, stage)
	event.ID = p.id
	event.Duration, event.Outcome, event.ErrorCode = time.Since(p.started), outcome, code
	emitTelemetry(p.execution.options, event)
}

func newExecutionTelemetry(options TelemetryOptions, operationName string, operationKind protocol.OperationKind, staticCost uint64) *executionTelemetry {
	if options.Hooks.Trace == nil && options.Hooks.Metric == nil && options.Hooks.Log == nil && options.Hooks.Failure == nil {
		return nil
	}
	requestSpan := uniqueTelemetryID("request", options.RequestID)
	return &executionTelemetry{
		options: options, operationName: operationName, operationKind: operationKind, staticCost: staticCost,
		requestSpan: requestSpan, operationSpan: telemetryID("operation", options.OperationID, requestSpan),
	}
}

func (t *executionTelemetry) baseEvent(kind TelemetryKind, stage TelemetryStage) TelemetryEvent {
	if t == nil {
		return TelemetryEvent{}
	}
	return TelemetryEvent{
		Kind: kind, Stage: stage, RequestID: t.options.RequestID, OperationID: t.options.OperationID,
		OperationName: t.operationName, OperationKind: t.operationKind,
		SchemaRevision: t.options.SchemaRevision, PersistedHash: t.options.PersistedHash,
		PrincipalReference: t.options.PrincipalReference, Cost: t.staticCost,
	}
}

func (t *executionTelemetry) nextID(kind string, parts ...string) string {
	sequence := t.sequence.Add(1)
	return telemetryID(append([]string{kind, t.operationSpan}, append(parts, fmt.Sprint(sequence))...)...)
}

func (t *executionTelemetry) parentID() string {
	if t == nil {
		return ""
	}
	t.parentMu.RLock()
	defer t.parentMu.RUnlock()
	if t.parent != "" {
		return t.parent
	}
	return t.operationSpan
}

func (t *executionTelemetry) pushParent(parent string) func() {
	t.parentMu.Lock()
	previous := t.parent
	t.parent = parent
	t.parentMu.Unlock()
	return func() {
		t.parentMu.Lock()
		t.parent = previous
		t.parentMu.Unlock()
	}
}

func emitTelemetry(options TelemetryOptions, event TelemetryEvent) {
	event = normalizeTelemetryEvent(event)
	traceEvent := cloneTelemetryEvent(event)
	runTelemetryHook("trace", options.Hooks.Trace, traceEvent, traceEvent, options.Hooks.Failure)
	runTelemetryHook("metric", options.Hooks.Metric, MetricEvent{
		Kind: event.Kind, Stage: event.Stage, OperationKind: event.OperationKind,
		Outcome: event.Outcome, ErrorCode: boundedTelemetryCode(event.ErrorCode), Duration: event.Duration,
		Cost: event.Cost, BatchSize: event.BatchSize, Attempt: event.Attempt,
		ConnectionAttempt: event.ConnectionAttempt, ReplayAttempt: event.ReplayAttempt,
		ActiveStreams: event.ActiveStreams, ReplayEvents: event.ReplayEvents, ReplayBytes: event.ReplayBytes,
		ScannedCandidates: event.ScannedCandidates, DrainRemaining: event.DrainRemaining,
	}, event, options.Hooks.Failure)
	logEvent := cloneTelemetryEvent(event)
	runTelemetryHook("log", options.Hooks.Log, logEvent, logEvent, options.Hooks.Failure)
}

func cloneTelemetryEvent(event TelemetryEvent) TelemetryEvent {
	event.Links = slices.Clone(event.Links)
	return event
}

func normalizeTelemetryEvent(event TelemetryEvent) TelemetryEvent {
	stage, validStage := boundedTelemetryStage(event.Kind, event.Stage)
	outcome, validOutcome := boundedTelemetryOutcome(event.Outcome)
	event.Stage, event.Outcome = stage, outcome
	if !validStage || !validOutcome {
		event.ErrorCode = CodeInternal
	} else {
		event.ErrorCode = boundedTelemetryCode(event.ErrorCode)
	}
	return event
}

func boundedTelemetryStage(kind TelemetryKind, stage TelemetryStage) (TelemetryStage, bool) {
	switch kind {
	case TelemetryTransaction:
		switch stage {
		case TelemetryStage(MutationAttempted), TelemetryStage(MutationDenied), TelemetryStage(MutationCommitted),
			TelemetryStage(MutationRolledBack), TelemetryStage(MutationCompensated), TelemetryStage(MutationIndeterminate):
			return stage, true
		case TelemetryStarted, TelemetryCompleted, TelemetryFailed, TelemetryCancelled:
		default:
		}
	case TelemetrySubscription:
		switch stage {
		case TelemetryStage(SubscriptionEstablished), TelemetryStage(SubscriptionReconnected), TelemetryStage(SubscriptionResumed),
			TelemetryStage(SubscriptionReplayStarted), TelemetryStage(SubscriptionHistoryLost), TelemetryStage(SubscriptionRefetchRequired),
			TelemetryStage(SubscriptionReauthorized), TelemetryStage(SubscriptionBackpressure), TelemetryStage(SubscriptionDeliveryFailed),
			TelemetryStage(SubscriptionDrainStarted), TelemetryStage(SubscriptionDrainForced), TelemetryStage(SubscriptionClosed):
			return stage, true
		case TelemetryStarted, TelemetryCompleted, TelemetryFailed, TelemetryCancelled:
		default:
		}
	case TelemetryRequest, TelemetryPlanning, TelemetryOperation, TelemetryHandler, TelemetryBatch, TelemetryRetry:
		switch stage {
		case TelemetryStarted, TelemetryCompleted, TelemetryFailed, TelemetryCancelled:
			return stage, true
		default:
		}
	}
	return TelemetryFailed, false
}

func boundedTelemetryOutcome(outcome TelemetryOutcome) (TelemetryOutcome, bool) {
	switch outcome {
	case "", TelemetryActive, TelemetrySucceeded, TelemetryFailedOutcome, TelemetryCancelledOutcome,
		TelemetryDeniedOutcome, TelemetryHistoryUnavailable, TelemetryAuthorizationExpired,
		TelemetrySlowConsumer, TelemetryBrokerFailure, TelemetryForcedDrain:
		return outcome, true
	default:
		return TelemetryFailedOutcome, false
	}
}

func runTelemetryHook[Event any](pillar string, hook func(Event) error, payload Event, event TelemetryEvent, failure TelemetryFailureHook) {
	if hook == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			observeSafely(failure, TelemetryFailure{Pillar: pillar, Kind: event.Kind, Stage: event.Stage, Message: pillar + " hook panicked", Panicked: true})
		}
	}()
	if err := hook(payload); err != nil {
		observeSafely(failure, TelemetryFailure{Pillar: pillar, Kind: event.Kind, Stage: event.Stage, Message: pillar + " hook failed"})
	}
}

func telemetryID(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:8])
}

func uniqueTelemetryID(parts ...string) string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return telemetryID(append(parts, hex.EncodeToString(random[:]))...)
	}
	sequence := telemetryFallbackSequence.Add(1)
	return telemetryID(append(parts, fmt.Sprint(time.Now().UnixNano()), fmt.Sprint(sequence))...)
}

func boundedTelemetryCode(code string) string {
	switch code {
	case "", CodeCancelled, CodeHandlerFailed, CodeInternal, CodeValidationFailed, CodeOutputCompletion,
		CodeInvalidCollection, CodeInvalidCursor, CodeResultMissing, CodeResultNull,
		CodeResultSkipped, CodeResultScope, CodeResultUnavailable, CodeUnauthorized, CodeResourceExhausted,
		CodeTransactionBeginFailed, CodeTransactionCommitFailed, CodeTransactionCommitUnknown,
		CodeTransactionRollbackFailed, CodeSavepointBeginFailed, CodeSavepointReleaseFailed,
		CodeSavepointRollbackFailed, CodeOutboxPersistFailed, CodeAfterCommitFailed,
		CodeExternalEffectUncoordinated, CodeCompensationFailed, CodeRetryBudgetExhausted,
		CodeIdempotencyConflict, CodeIdempotencyIndeterminate, CodeIdempotencyNotAllowed,
		CodeIdempotencyStoreUnavailable, CodeCacheInvalidationFailed, CodeOverloaded, CodeRateLimited:
		return code
	default:
		return CodeInternal
	}
}

func outcomeTelemetry(outcome Outcome) (TelemetryStage, TelemetryOutcome, string) {
	if len(outcome.Errors) == 0 {
		return TelemetryCompleted, TelemetrySucceeded, ""
	}
	code := outcome.Errors[0].Code
	for _, failure := range outcome.Errors {
		if failure.Code == CodeCancelled {
			return TelemetryCancelled, TelemetryCancelledOutcome, CodeCancelled
		}
	}
	return TelemetryFailed, TelemetryFailedOutcome, code
}

// SubscriptionTelemetry is supplied by a stream adapter. It has no raw
// handle, cursor, topic, principal, or variables fields by construction.
type SubscriptionTelemetry struct {
	Stage             SubscriptionTelemetryStage
	StreamReference   string
	SignalReference   string
	ParentReference   string
	Links             []string
	ConnectionAttempt uint32
	ReplayAttempt     uint32
	Outcome           TelemetryOutcome
	ErrorCode         string
	Duration          time.Duration
	Cost              uint64
	BatchSize         int
	Attempt           uint32
	ActiveStreams     int64
	ReplayEvents      uint64
	ReplayBytes       uint64
	ScannedCandidates uint64
	DrainRemaining    uint64
}

type SubscriptionTelemetryStage string

const (
	SubscriptionEstablished     SubscriptionTelemetryStage = "established"
	SubscriptionReconnected     SubscriptionTelemetryStage = "reconnected"
	SubscriptionResumed         SubscriptionTelemetryStage = "resumed"
	SubscriptionReplayStarted   SubscriptionTelemetryStage = "replay-started"
	SubscriptionHistoryLost     SubscriptionTelemetryStage = "history-lost"
	SubscriptionRefetchRequired SubscriptionTelemetryStage = "refetch-required"
	SubscriptionReauthorized    SubscriptionTelemetryStage = "reauthorized"
	SubscriptionBackpressure    SubscriptionTelemetryStage = "backpressure"
	SubscriptionDeliveryFailed  SubscriptionTelemetryStage = "delivery-failed"
	SubscriptionDrainStarted    SubscriptionTelemetryStage = "drain-started"
	SubscriptionDrainForced     SubscriptionTelemetryStage = "drain-forced"
	SubscriptionClosed          SubscriptionTelemetryStage = "closed"
)

// EmitSubscriptionTelemetry lets stream adapters report the core portable
// lifecycle without importing a telemetry SDK.
func EmitSubscriptionTelemetry(options TelemetryOptions, signal SubscriptionTelemetry) {
	id := signal.SignalReference
	if id == "" {
		id = uniqueTelemetryID("subscription", signal.StreamReference, string(signal.Stage))
	}
	event := TelemetryEvent{
		Kind: TelemetrySubscription, Stage: TelemetryStage(signal.Stage),
		ID: id, ParentID: signal.ParentReference, Links: slices.Clone(signal.Links), RequestID: options.RequestID,
		OperationID: options.OperationID, StreamReference: signal.StreamReference,
		ConnectionAttempt: signal.ConnectionAttempt, ReplayAttempt: signal.ReplayAttempt,
		Outcome: signal.Outcome, ErrorCode: signal.ErrorCode, Duration: signal.Duration, Cost: signal.Cost,
		BatchSize: signal.BatchSize, Attempt: signal.Attempt, ActiveStreams: signal.ActiveStreams,
		ReplayEvents: signal.ReplayEvents, ReplayBytes: signal.ReplayBytes, ScannedCandidates: signal.ScannedCandidates,
		DrainRemaining: signal.DrainRemaining,
	}
	emitTelemetry(options, event)
}

type telemetryContextKey struct{}

type executionTelemetryContext struct {
	options   TelemetryOptions
	execution *executionTelemetry
}

func telemetryContextFromContext(ctx context.Context) (executionTelemetryContext, bool) {
	value, ok := ctx.Value(telemetryContextKey{}).(executionTelemetryContext)
	return value, ok
}
