package runtime

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ProcessState is the externally safe state of one runtime host.
type ProcessState string

const (
	ProcessStarting ProcessState = "starting"
	ProcessReady    ProcessState = "ready"
	ProcessDraining ProcessState = "draining"
	ProcessStopped  ProcessState = "stopped"
	ProcessForced   ProcessState = "forced"
)

// WorkKind classifies process-owned work without exposing operation names.
type WorkKind string

const (
	WorkQuery    WorkKind = "query"
	WorkMutation WorkKind = "mutation"
	WorkStream   WorkKind = "stream"
	WorkRemote   WorkKind = "remote"
	WorkDurable  WorkKind = "durable"
)

// WorkPhase lets drain policy distinguish an executing mutation from a commit.
type WorkPhase string

const (
	WorkExecuting  WorkPhase = "executing"
	WorkCommitting WorkPhase = "committing"
	WorkCleanup    WorkPhase = "cleanup"
)

// CleanupKind selects one independently bounded cleanup context.
type CleanupKind string

const (
	CleanupRollback     CleanupKind = "rollback"
	CleanupLeaseRelease CleanupKind = "lease-release"
	CleanupSourceClose  CleanupKind = "source-close"
)

// DrainAction controls accepted work when a process begins draining.
type DrainAction string

const (
	DrainFinish DrainAction = "finish"
	DrainCancel DrainAction = "cancel"
)

// DrainOutcome is the truthful terminal result of a drain attempt.
type DrainOutcome string

const (
	DrainCompleted DrainOutcome = "completed"
	DrainForced    DrainOutcome = "forced"
)

var (
	ErrProcessDraining = errors.New("process is draining")
	ErrProcessForced   = errors.New("process drain was forced")
)

// ProcessLimits bound aggregate active and queued process resources.
type ProcessLimits struct {
	MaxInFlight          uint64
	MaxQueued            uint64
	MaxQueuedBytes       uint64
	MaxReservedBytes     uint64
	MaxResultBufferBytes uint64
	MaxStreams           uint64
	MaxRemoteConnections uint64
	MaxTenantInFlight    uint64
	MaxTenantQueued      uint64
	MaxPrincipalInFlight uint64
	MaxPrincipalQueued   uint64
}

// CleanupTimeouts keep rollback, lease release, and source closure independent.
type CleanupTimeouts struct {
	Rollback     time.Duration
	LeaseRelease time.Duration
	SourceClose  time.Duration
}

// DrainPolicy decides which accepted work receives cooperative cancellation.
// A mutation already committing is always allowed to finish until forced.
type DrainPolicy struct {
	Query    DrainAction
	Mutation DrainAction
	Stream   DrainAction
	Remote   DrainAction
	Durable  DrainAction
}

// DependencyConfig declares one readiness dependency. Names are configuration
// keys only and are never emitted by ProcessEvent or ProcessHealth.
type DependencyConfig struct {
	Name      string
	Essential bool
	Ready     bool
}

// ProcessRevision is an immutable schema and configuration generation. Registry
// snapshots are already frozen; active leases retain this value across swaps.
type ProcessRevision struct {
	Revision              string
	SchemaRevision        string
	ConfigurationRevision string
	Registry              Snapshot
}

// ProcessHooks publish cardinality-safe process lifecycle facts.
type ProcessHooks struct {
	Observe func(ProcessEvent) error
	Failure func(ProcessHookFailure)
}

// ProcessHookQueueCapacity bounds asynchronous lifecycle observation. A
// stalled observer cannot block admission or drain; events beyond this queue
// are best-effort and may be discarded.
const ProcessHookQueueCapacity = 256

// ProcessHookFailure reports only the bounded event stage and panic class.
type ProcessHookFailure struct {
	Stage    ProcessEventStage `json:"stage"`
	Panicked bool              `json:"panicked,omitempty"`
}

// ProcessEventStage is a bounded process lifecycle vocabulary.
type ProcessEventStage string

const (
	ProcessEventStarted               ProcessEventStage = "started"
	ProcessEventDependencyUnavailable ProcessEventStage = "dependency-unavailable"
	ProcessEventDependencyRecovered   ProcessEventStage = "dependency-recovered"
	ProcessEventAdmissionQueued       ProcessEventStage = "admission-queued"
	ProcessEventAdmissionGranted      ProcessEventStage = "admission-granted"
	ProcessEventAdmissionRejected     ProcessEventStage = "admission-rejected"
	ProcessEventAdmissionCancelled    ProcessEventStage = "admission-cancelled"
	ProcessEventWorkReleased          ProcessEventStage = "work-released"
	ProcessEventDrainStarted          ProcessEventStage = "drain-started"
	ProcessEventDrainCompleted        ProcessEventStage = "drain-completed"
	ProcessEventDrainForced           ProcessEventStage = "drain-forced"
	ProcessEventRevisionInstalled     ProcessEventStage = "revision-installed"
	ProcessEventRevisionRolledBack    ProcessEventStage = "revision-rolled-back"
)

// ProcessEvent contains counts and bounded classifications, never caller keys,
// dependency names, schema identifiers, or configuration values.
type ProcessEvent struct {
	Sequence uint64            `json:"sequence"`
	Stage    ProcessEventStage `json:"stage"`
	State    ProcessState      `json:"state"`
	Work     WorkKind          `json:"work,omitempty"`
	Outcome  TelemetryOutcome  `json:"outcome,omitempty"`
	Code     string            `json:"code,omitempty"`
	ProcessStats
}

// ProcessStats is a safe aggregate accounting snapshot.
type ProcessStats struct {
	InFlight          uint64 `json:"inFlight"`
	Queued            uint64 `json:"queued"`
	QueuedBytes       uint64 `json:"queuedBytes"`
	ReservedBytes     uint64 `json:"reservedBytes"`
	ResultBufferBytes uint64 `json:"resultBufferBytes"`
	Streams           uint64 `json:"streams"`
	RemoteConnections uint64 `json:"remoteConnections"`
}

// ProcessHealth intentionally omits revisions, dependency names, and causes.
type ProcessHealth struct {
	State                ProcessState `json:"state"`
	Ready                bool         `json:"ready"`
	Live                 bool         `json:"live"`
	Unavailable          uint64       `json:"unavailableDependencies"`
	EssentialUnavailable uint64       `json:"essentialUnavailable"`
}

// ProcessConfig is copied and validated by NewProcessController.
type ProcessConfig struct {
	Limits                 ProcessLimits
	Cleanup                CleanupTimeouts
	Drain                  DrainPolicy
	Dependencies           []DependencyConfig
	RetryAfter             time.Duration
	MaxDrainDuration       time.Duration
	DependencyFailureGrace time.Duration
	MaxConnectionLifetime  time.Duration
	ReconnectStagger       time.Duration
	Clock                  func() time.Time
	Hooks                  ProcessHooks
}

type processDependency struct {
	essential   bool
	ready       bool
	failedSince time.Time
}

// ProcessController owns aggregate admission, lifecycle, and revision state.
type ProcessController struct {
	eventMu sync.Mutex
	mu      sync.Mutex

	config          ProcessConfig
	state           ProcessState
	revision        ProcessRevision
	dependencies    map[string]*processDependency
	stats           ProcessStats
	queue           []*admissionWaiter
	active          map[*WorkLease]struct{}
	tenantActive    map[processPartitionKey]uint64
	tenantQueued    map[processPartitionKey]uint64
	principalActive map[processPartitionKey]uint64
	principalQueued map[processPartitionKey]uint64
	lastTenant      processPartitionKey
	lastPrincipal   processPartitionKey
	changed         chan struct{}
	eventSequence   uint64
	hookEvents      chan ProcessEvent
	hookDone        chan struct{}
	hookClosed      bool
}

// DefaultProcessConfig returns finite portable reference limits.
func DefaultProcessConfig() ProcessConfig {
	return ProcessConfig{
		Limits: ProcessLimits{
			MaxInFlight: 256, MaxQueued: 1_024, MaxQueuedBytes: 64 << 20,
			MaxReservedBytes: 1 << 30, MaxResultBufferBytes: 512 << 20,
			MaxStreams: 128, MaxRemoteConnections: 128,
			MaxTenantInFlight: 32, MaxTenantQueued: 128,
			MaxPrincipalInFlight: 8, MaxPrincipalQueued: 32,
		},
		Cleanup:    CleanupTimeouts{Rollback: 10 * time.Second, LeaseRelease: 5 * time.Second, SourceClose: 5 * time.Second},
		Drain:      DrainPolicy{Query: DrainCancel, Mutation: DrainFinish, Stream: DrainCancel, Remote: DrainCancel, Durable: DrainFinish},
		RetryAfter: time.Second, MaxDrainDuration: 30 * time.Second, DependencyFailureGrace: time.Minute,
		MaxConnectionLifetime: time.Hour, ReconnectStagger: 5 * time.Minute, Clock: time.Now,
	}
}

// NewProcessController validates and snapshots process configuration.
func NewProcessController(config ProcessConfig, initial ProcessRevision) (*ProcessController, error) {
	resolved, err := resolveProcessConfig(config)
	if err != nil {
		return nil, err
	}
	if err := validateProcessRevision(initial); err != nil {
		return nil, err
	}
	dependencies := make(map[string]*processDependency, len(resolved.Dependencies))
	for _, configured := range resolved.Dependencies {
		if _, exists := dependencies[configured.Name]; exists {
			return nil, fmt.Errorf("duplicate process dependency %q", configured.Name)
		}
		dependency := &processDependency{essential: configured.Essential, ready: configured.Ready}
		if !configured.Ready {
			failedSince, clockErr := callProcessClock(resolved.Clock)
			if clockErr != nil {
				return nil, clockErr
			}
			dependency.failedSince = failedSince
		}
		dependencies[configured.Name] = dependency
	}
	controller := &ProcessController{
		config: resolved, state: ProcessStarting, revision: initial, dependencies: dependencies,
		active: make(map[*WorkLease]struct{}), tenantActive: make(map[processPartitionKey]uint64), tenantQueued: make(map[processPartitionKey]uint64),
		principalActive: make(map[processPartitionKey]uint64), principalQueued: make(map[processPartitionKey]uint64), changed: make(chan struct{}),
	}
	if resolved.Hooks.Observe != nil {
		controller.hookEvents = make(chan ProcessEvent, ProcessHookQueueCapacity)
		controller.hookDone = make(chan struct{})
		go controller.runProcessHooks()
	}
	return controller, nil
}

func resolveProcessConfig(config ProcessConfig) (ProcessConfig, error) {
	defaults := DefaultProcessConfig()
	resolveProcessLimits(&config.Limits, defaults.Limits)
	resolveCleanupTimeouts(&config.Cleanup, defaults.Cleanup)
	resolveDrainPolicy(&config.Drain, defaults.Drain)
	if config.RetryAfter == 0 {
		config.RetryAfter = defaults.RetryAfter
	}
	if config.MaxDrainDuration == 0 {
		config.MaxDrainDuration = defaults.MaxDrainDuration
	}
	if config.DependencyFailureGrace == 0 {
		config.DependencyFailureGrace = defaults.DependencyFailureGrace
	}
	if config.MaxConnectionLifetime == 0 {
		config.MaxConnectionLifetime = defaults.MaxConnectionLifetime
	}
	if config.ReconnectStagger == 0 {
		config.ReconnectStagger = defaults.ReconnectStagger
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	config.Dependencies = append([]DependencyConfig(nil), config.Dependencies...)
	if err := validateProcessConfig(config); err != nil {
		return ProcessConfig{}, err
	}
	return config, nil
}

func resolveProcessLimits(limits *ProcessLimits, defaults ProcessLimits) {
	fields := []struct {
		value    *uint64
		fallback uint64
	}{
		{&limits.MaxInFlight, defaults.MaxInFlight}, {&limits.MaxQueued, defaults.MaxQueued},
		{&limits.MaxQueuedBytes, defaults.MaxQueuedBytes}, {&limits.MaxReservedBytes, defaults.MaxReservedBytes},
		{&limits.MaxResultBufferBytes, defaults.MaxResultBufferBytes}, {&limits.MaxStreams, defaults.MaxStreams},
		{&limits.MaxRemoteConnections, defaults.MaxRemoteConnections}, {&limits.MaxTenantInFlight, defaults.MaxTenantInFlight},
		{&limits.MaxTenantQueued, defaults.MaxTenantQueued}, {&limits.MaxPrincipalInFlight, defaults.MaxPrincipalInFlight},
		{&limits.MaxPrincipalQueued, defaults.MaxPrincipalQueued},
	}
	for _, field := range fields {
		*field.value = processDefault(*field.value, field.fallback)
	}
	limits.MaxTenantInFlight = reservePartitionCapacity(limits.MaxTenantInFlight, limits.MaxInFlight)
	limits.MaxPrincipalInFlight = reservePartitionCapacity(limits.MaxPrincipalInFlight, limits.MaxTenantInFlight)
	limits.MaxTenantQueued = reservePartitionCapacity(limits.MaxTenantQueued, limits.MaxQueued)
	limits.MaxPrincipalQueued = reservePartitionCapacity(limits.MaxPrincipalQueued, limits.MaxTenantQueued)
}

func reservePartitionCapacity(limit, parent uint64) uint64 {
	if parent <= 1 {
		return 1
	}
	return min(limit, parent-1)
}

func resolveCleanupTimeouts(cleanup *CleanupTimeouts, defaults CleanupTimeouts) {
	cleanup.Rollback = processDefault(cleanup.Rollback, defaults.Rollback)
	cleanup.LeaseRelease = processDefault(cleanup.LeaseRelease, defaults.LeaseRelease)
	cleanup.SourceClose = processDefault(cleanup.SourceClose, defaults.SourceClose)
}

func resolveDrainPolicy(policy *DrainPolicy, defaults DrainPolicy) {
	policy.Query = processDefault(policy.Query, defaults.Query)
	policy.Mutation = processDefault(policy.Mutation, defaults.Mutation)
	policy.Stream = processDefault(policy.Stream, defaults.Stream)
	policy.Remote = processDefault(policy.Remote, defaults.Remote)
	policy.Durable = processDefault(policy.Durable, defaults.Durable)
}

func processDefault[T comparable](value, fallback T) T {
	var zero T
	if value == zero {
		return fallback
	}
	return value
}

func validateProcessConfig(config ProcessConfig) error {
	if config.RetryAfter < 0 || config.MaxDrainDuration <= 0 || config.DependencyFailureGrace < 0 {
		return errors.New("process retry, drain, and dependency durations must be non-negative with a positive drain duration")
	}
	if config.Cleanup.Rollback <= 0 || config.Cleanup.LeaseRelease <= 0 || config.Cleanup.SourceClose <= 0 {
		return errors.New("process cleanup durations must be positive")
	}
	if config.MaxConnectionLifetime <= 0 || config.ReconnectStagger < 0 || config.ReconnectStagger >= config.MaxConnectionLifetime {
		return errors.New("connection lifetime must be positive and exceed reconnect staggering")
	}
	for _, action := range []DrainAction{config.Drain.Query, config.Drain.Mutation, config.Drain.Stream, config.Drain.Remote, config.Drain.Durable} {
		if action != DrainFinish && action != DrainCancel {
			return fmt.Errorf("unsupported drain action %q", action)
		}
	}
	for _, dependency := range config.Dependencies {
		if dependency.Name == "" || strings.TrimSpace(dependency.Name) != dependency.Name || strings.ContainsAny(dependency.Name, "\x00\r\n") {
			return errors.New("process dependency names must be non-empty safe tokens")
		}
	}
	return nil
}

func validateProcessRevision(revision ProcessRevision) error {
	if revision.Revision == "" || revision.SchemaRevision == "" || revision.ConfigurationRevision == "" {
		return errors.New("process revision, schema revision, and configuration revision are required")
	}
	return nil
}

// Start finishes configuration validation and opens admission.
func (c *ProcessController) Start() error {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.mu.Lock()
	if c.state != ProcessStarting {
		state := c.state
		c.mu.Unlock()
		return fmt.Errorf("cannot start process from %s", state)
	}
	c.state = ProcessReady
	c.notifyLocked()
	event := c.processEventLocked(ProcessEventStarted, "", TelemetrySucceeded, "")
	c.mu.Unlock()
	c.emitProcessEvents(event)
	return nil
}

// Stats returns aggregate accounting without caller partition identifiers.
func (c *ProcessController) Stats() ProcessStats {
	c.mu.Lock()
	stats := c.stats
	c.mu.Unlock()
	return stats
}

// Health separates immediate readiness from sustained liveness failure.
func (c *ProcessController) Health() ProcessHealth {
	now, clockErr := callProcessClock(c.config.Clock)
	c.mu.Lock()
	defer c.mu.Unlock()
	health := ProcessHealth{State: c.state, Live: c.state == ProcessStarting || c.state == ProcessReady || c.state == ProcessDraining}
	health.Ready = c.state == ProcessReady
	if clockErr != nil {
		health.Ready = false
		health.Live = false
	}
	for _, dependency := range c.dependencies {
		if dependency.ready {
			continue
		}
		health.Unavailable++
		if !dependency.essential {
			continue
		}
		health.EssentialUnavailable++
		health.Ready = false
		if clockErr == nil && now.Sub(dependency.failedSince) >= c.config.DependencyFailureGrace {
			health.Live = false
		}
	}
	return health
}

// SetDependency updates one configured dependency without retaining a cause.
func (c *ProcessController) SetDependency(name string, ready bool) error {
	var failedSince time.Time
	var clockErr error
	if !ready {
		failedSince, clockErr = callProcessClock(c.config.Clock)
	}
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.mu.Lock()
	dependency, ok := c.dependencies[name]
	if !ok {
		c.mu.Unlock()
		if clockErr != nil {
			return clockErr
		}
		return fmt.Errorf("unknown process dependency %q", name)
	}
	if dependency.ready == ready {
		c.mu.Unlock()
		return clockErr
	}
	dependency.ready = ready
	stage := ProcessEventDependencyRecovered
	if ready {
		dependency.failedSince = time.Time{}
	} else {
		dependency.failedSince = failedSince
		stage = ProcessEventDependencyUnavailable
	}
	c.notifyLocked()
	events := []ProcessEvent{c.processEventLocked(stage, "", TelemetryActive, "")}
	if ready {
		events = append(events, c.grantQueuedLocked()...)
	}
	c.mu.Unlock()
	c.emitProcessEvents(events...)
	return clockErr
}

func callProcessClock(clock func() time.Time) (now time.Time, err error) {
	defer func() {
		if recover() != nil {
			now = time.Time{}
			err = errors.New("process clock failed")
		}
	}()
	return clock(), nil
}

// InstallRevision atomically selects the revision captured by future leases.
func (c *ProcessController) InstallRevision(revision ProcessRevision) error {
	return c.swapRevision(revision, ProcessEventRevisionInstalled)
}

// RollbackRevision atomically restores a previously retained revision.
func (c *ProcessController) RollbackRevision(revision ProcessRevision) error {
	return c.swapRevision(revision, ProcessEventRevisionRolledBack)
}

func (c *ProcessController) swapRevision(revision ProcessRevision, stage ProcessEventStage) error {
	if err := validateProcessRevision(revision); err != nil {
		return err
	}
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.mu.Lock()
	if c.state == ProcessDraining || c.state == ProcessStopped || c.state == ProcessForced {
		c.mu.Unlock()
		return ErrProcessDraining
	}
	c.revision = revision
	c.notifyLocked()
	event := c.processEventLocked(stage, "", TelemetrySucceeded, "")
	c.mu.Unlock()
	c.emitProcessEvents(event)
	return nil
}

func (c *ProcessController) processEventLocked(stage ProcessEventStage, work WorkKind, outcome TelemetryOutcome, code string) ProcessEvent {
	c.eventSequence++
	return ProcessEvent{
		Sequence: c.eventSequence, Stage: stage, State: c.state, Work: work, Outcome: outcome, Code: code,
		ProcessStats: c.stats,
	}
}

func (c *ProcessController) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *ProcessController) emitProcessEvents(events ...ProcessEvent) {
	if c.hookEvents == nil || c.hookClosed {
		return
	}
	for index := 0; index < len(events); index++ {
		select {
		case c.hookEvents <- events[index]:
		default:
		}
	}
}

func (c *ProcessController) runProcessHooks() {
	defer close(c.hookDone)
	for event := range c.hookEvents {
		callProcessHook(c.config.Hooks, event)
	}
}

func (c *ProcessController) stopProcessHooksLocked() {
	if c.hookEvents == nil || c.hookClosed {
		return
	}
	close(c.hookEvents)
	c.hookClosed = true
}

func callProcessHook(hooks ProcessHooks, event ProcessEvent) {
	if hooks.Observe == nil {
		return
	}
	panicked := true
	defer func() {
		if recover() != nil {
			reportProcessHookFailure(hooks.Failure, ProcessHookFailure{Stage: event.Stage, Panicked: panicked})
		}
	}()
	err := hooks.Observe(event)
	panicked = false
	if err != nil {
		reportProcessHookFailure(hooks.Failure, ProcessHookFailure{Stage: event.Stage})
	}
}

func reportProcessHookFailure(hook func(ProcessHookFailure), failure ProcessHookFailure) {
	if hook == nil {
		return
	}
	defer func() { _ = recover() }()
	hook(failure)
}
