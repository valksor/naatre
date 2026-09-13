package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// ProcessPartitionReferenceMaxBytes bounds each opaque tenant or principal
// reference before it is reduced to a fixed-size process-local key.
const ProcessPartitionReferenceMaxBytes = 256

// AdmissionRequest reserves aggregate resources before application work starts.
// Zero byte weights are charged as one byte so callers cannot create free work.
type AdmissionRequest struct {
	TenantReference    string
	PrincipalReference string
	Kind               WorkKind
	ReservedBytes      uint64
	QueueBytes         uint64
	ResultBufferBytes  uint64
}

// AdmissionError is safe for public overload mapping and carries a bounded
// Retry-After hint. The wrapped cause is process-local.
type AdmissionError struct {
	Code       string
	RetryAfter time.Duration
	cause      error
}

func (e *AdmissionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == CodeRateLimited {
		return "process admission rate limited"
	}
	return "process admission overloaded"
}

func (e *AdmissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type admissionWaiter struct {
	ctx       context.Context
	request   AdmissionRequest
	tenant    processPartitionKey
	principal processPartitionKey
	ready     chan struct{}
	lease     *WorkLease
	err       error
}

// WorkLease owns one admitted process slot until the underlying work truly
// exits. Cancellation alone never releases accounting.
type WorkLease struct {
	controller *ProcessController
	request    AdmissionRequest
	tenant     processPartitionKey
	principal  processPartitionKey
	revision   ProcessRevision
	ctx        context.Context
	cancel     context.CancelCauseFunc
	phase      WorkPhase
	released   bool
	once       sync.Once
}

// AdmittedWork exposes lifecycle operations to Run callbacks without exposing
// Release. Run alone owns the resource slot until the callback returns.
type AdmittedWork struct {
	lease *WorkLease
}

// Revision returns the immutable revision captured for this work.
func (work AdmittedWork) Revision() ProcessRevision {
	return work.lease.Revision()
}

// SetPhase updates mutation ownership state used by drain policy.
func (work AdmittedWork) SetPhase(phase WorkPhase) error {
	return work.lease.SetPhase(phase)
}

// CleanupContext returns an independently bounded cleanup context.
func (work AdmittedWork) CleanupContext(kind CleanupKind) (context.Context, context.CancelFunc, error) {
	return work.lease.CleanupContext(kind)
}

// Context is cancelled by the caller, configured drain policy, or forced drain.
func (l *WorkLease) Context() context.Context {
	ctx := context.Background()
	if l != nil {
		ctx = l.ctx
	}
	return ctx
}

// Revision is the immutable revision captured when this work was admitted.
func (l *WorkLease) Revision() ProcessRevision {
	if l == nil {
		return ProcessRevision{}
	}
	return l.revision
}

// SetPhase updates drain-visible ownership state without releasing resources.
func (l *WorkLease) SetPhase(phase WorkPhase) error {
	if l == nil || l.controller == nil {
		return errors.New("process work lease is nil")
	}
	if phase != WorkExecuting && phase != WorkCommitting && phase != WorkCleanup {
		return fmt.Errorf("unsupported work phase %q", phase)
	}
	l.controller.mu.Lock()
	defer l.controller.mu.Unlock()
	if l.released {
		return errors.New("process work lease is released")
	}
	l.phase = phase
	return nil
}

// Release returns accounting only after the owned work has actually exited.
func (l *WorkLease) Release() {
	if l == nil || l.controller == nil {
		return
	}
	l.once.Do(func() { l.controller.releaseLease(l) })
}

// Admit either reserves resources immediately, joins the bounded fair queue,
// or returns a stable overload result before business execution.
func (c *ProcessController) Admit(ctx context.Context, request AdmissionRequest) (*WorkLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := normalizeAdmissionRequest(request)
	if err != nil {
		return nil, err
	}
	tenant := processPartitionKeyFor("tenant", request.TenantReference)
	principal := processPartitionKeyFor("principal", request.TenantReference, request.PrincipalReference)
	request.TenantReference, request.PrincipalReference = "", ""
	waiter := &admissionWaiter{ctx: ctx, request: request, tenant: tenant, principal: principal, ready: make(chan struct{})}
	c.eventMu.Lock()
	c.mu.Lock()
	if cause := context.Cause(ctx); cause != nil {
		event := c.processEventLocked(ProcessEventAdmissionCancelled, request.Kind, TelemetryCancelledOutcome, CodeCancelled)
		c.mu.Unlock()
		c.emitProcessEvents(event)
		c.eventMu.Unlock()
		return nil, cause
	}
	if !c.admissionReadyLocked() {
		failure := c.admissionError(CodeOverloaded, ErrProcessDraining)
		event := c.processEventLocked(ProcessEventAdmissionRejected, request.Kind, TelemetryFailedOutcome, failure.Code)
		c.mu.Unlock()
		c.emitProcessEvents(event)
		c.eventMu.Unlock()
		return nil, failure
	}
	if c.requestExceedsLimitsLocked(waiter) {
		failure := c.admissionError(CodeOverloaded, errors.New("admission request exceeds process limits"))
		event := c.processEventLocked(ProcessEventAdmissionRejected, request.Kind, TelemetryFailedOutcome, failure.Code)
		c.mu.Unlock()
		c.emitProcessEvents(event)
		c.eventMu.Unlock()
		return nil, failure
	}
	if len(c.queue) == 0 && c.canGrantLocked(waiter) {
		lease := c.grantLocked(waiter)
		event := c.processEventLocked(ProcessEventAdmissionGranted, request.Kind, TelemetryActive, "")
		c.mu.Unlock()
		c.emitProcessEvents(event)
		c.eventMu.Unlock()
		return lease, nil
	}
	if failure := c.queueFailureLocked(waiter); failure != nil {
		event := c.processEventLocked(ProcessEventAdmissionRejected, request.Kind, TelemetryFailedOutcome, failure.Code)
		c.mu.Unlock()
		c.emitProcessEvents(event)
		c.eventMu.Unlock()
		return nil, failure
	}
	c.enqueueLocked(waiter)
	events := []ProcessEvent{c.processEventLocked(ProcessEventAdmissionQueued, request.Kind, TelemetryActive, "")}
	events = append(events, c.grantQueuedLocked()...)
	c.mu.Unlock()
	c.emitProcessEvents(events...)
	c.eventMu.Unlock()
	return c.waitForAdmission(ctx, waiter)
}

// Run admits work before invoking business code and retains its process slot
// until the callback returns. The callback receives the lease context so drain
// and caller cancellation share the same cooperative signal.
func (c *ProcessController) Run(ctx context.Context, request AdmissionRequest, business func(context.Context, AdmittedWork) error) error {
	if business == nil {
		return errors.New("process business callback is nil")
	}
	lease, err := c.Admit(ctx, request)
	if err != nil {
		return err
	}
	defer lease.Release()
	return business(lease.Context(), AdmittedWork{lease: lease})
}

func normalizeAdmissionRequest(request AdmissionRequest) (AdmissionRequest, error) {
	switch request.Kind {
	case WorkQuery, WorkMutation, WorkStream, WorkRemote, WorkDurable:
	default:
		return AdmissionRequest{}, fmt.Errorf("unsupported process work kind %q", request.Kind)
	}
	for _, reference := range []string{request.TenantReference, request.PrincipalReference} {
		if len(reference) > ProcessPartitionReferenceMaxBytes || !utf8.ValidString(reference) {
			return AdmissionRequest{}, errors.New("process partition reference is invalid or too large")
		}
	}
	if request.ReservedBytes == 0 {
		request.ReservedBytes = 1
	}
	if request.QueueBytes == 0 {
		request.QueueBytes = 1
	}
	if request.ResultBufferBytes == 0 {
		request.ResultBufferBytes = 1
	}
	return request, nil
}

type processPartitionKey [sha256.Size]byte

func processPartitionKeyFor(domain string, references ...string) processPartitionKey {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	var length [8]byte
	for _, reference := range references {
		binary.BigEndian.PutUint64(length[:], uint64(len(reference)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(reference))
	}
	var key processPartitionKey
	copy(key[:], hash.Sum(nil))
	return key
}

func (c *ProcessController) requestExceedsLimitsLocked(waiter *admissionWaiter) bool {
	limits := c.config.Limits
	request := waiter.request
	return request.ReservedBytes > limits.MaxReservedBytes || request.ResultBufferBytes > limits.MaxResultBufferBytes ||
		(request.Kind == WorkStream && limits.MaxStreams < 1) ||
		(request.Kind == WorkRemote && limits.MaxRemoteConnections < 1)
}

func (c *ProcessController) canGrantLocked(waiter *admissionWaiter) bool {
	limits := c.config.Limits
	request := waiter.request
	if exceeds(c.stats.InFlight, 1, limits.MaxInFlight) || exceeds(c.stats.ReservedBytes, request.ReservedBytes, limits.MaxReservedBytes) ||
		exceeds(c.stats.ResultBufferBytes, request.ResultBufferBytes, limits.MaxResultBufferBytes) ||
		exceeds(c.tenantActive[waiter.tenant], 1, limits.MaxTenantInFlight) ||
		exceeds(c.principalActive[waiter.principal], 1, limits.MaxPrincipalInFlight) {
		return false
	}
	if request.Kind == WorkStream && exceeds(c.stats.Streams, 1, limits.MaxStreams) {
		return false
	}
	return request.Kind != WorkRemote || !exceeds(c.stats.RemoteConnections, 1, limits.MaxRemoteConnections)
}

func exceeds(current, added, maximum uint64) bool {
	return added > maximum || current > maximum-added
}

func (c *ProcessController) queueFailureLocked(waiter *admissionWaiter) *AdmissionError {
	limits := c.config.Limits
	if c.tenantQueued[waiter.tenant] >= limits.MaxTenantQueued || c.principalQueued[waiter.principal] >= limits.MaxPrincipalQueued {
		return c.admissionError(CodeRateLimited, errors.New("caller admission partition is full"))
	}
	if c.stats.Queued >= limits.MaxQueued || exceeds(c.stats.QueuedBytes, waiter.request.QueueBytes, limits.MaxQueuedBytes) {
		return c.admissionError(CodeOverloaded, errors.New("process admission queue is full"))
	}
	return nil
}

func (c *ProcessController) admissionError(code string, cause error) *AdmissionError {
	return &AdmissionError{Code: code, RetryAfter: c.config.RetryAfter, cause: cause}
}

func (c *ProcessController) enqueueLocked(waiter *admissionWaiter) {
	c.queue = append(c.queue, waiter)
	c.stats.Queued++
	c.stats.QueuedBytes += waiter.request.QueueBytes
	c.tenantQueued[waiter.tenant]++
	c.principalQueued[waiter.principal]++
	c.notifyLocked()
}

func (c *ProcessController) removeQueuedLocked(index int) *admissionWaiter {
	waiter := c.queue[index]
	copy(c.queue[index:], c.queue[index+1:])
	c.queue[len(c.queue)-1] = nil
	c.queue = c.queue[:len(c.queue)-1]
	c.stats.Queued--
	c.stats.QueuedBytes -= waiter.request.QueueBytes
	decrementPartition(c.tenantQueued, waiter.tenant)
	decrementPartition(c.principalQueued, waiter.principal)
	c.notifyLocked()
	return waiter
}

func decrementPartition(counts map[processPartitionKey]uint64, key processPartitionKey) {
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}

func (c *ProcessController) grantQueuedLocked() []ProcessEvent {
	var events []ProcessEvent
	for {
		if !c.admissionReadyLocked() {
			return events
		}
		index := c.nextQueuedIndexLocked()
		if index < 0 {
			return events
		}
		waiter := c.removeQueuedLocked(index)
		if err := context.Cause(waiter.ctx); err != nil {
			waiter.err = err
			close(waiter.ready)
			events = append(events, c.processEventLocked(ProcessEventAdmissionCancelled, waiter.request.Kind, TelemetryCancelledOutcome, CodeCancelled))
			continue
		}
		c.grantLocked(waiter)
		close(waiter.ready)
		events = append(events, c.processEventLocked(ProcessEventAdmissionGranted, waiter.request.Kind, TelemetryActive, ""))
	}
}

func (c *ProcessController) admissionReadyLocked() bool {
	if c.state != ProcessReady {
		return false
	}
	ready := true
	for _, dependency := range c.dependencies {
		ready = ready && (!dependency.essential || dependency.ready)
	}
	return ready
}

func (c *ProcessController) nextQueuedIndexLocked() int {
	type partition struct {
		tenant    processPartitionKey
		principal processPartitionKey
	}
	seen := make(map[partition]struct{}, len(c.queue))
	bestIndex, bestPreference := -1, 3
	for index, waiter := range c.queue {
		key := partition{tenant: waiter.tenant, principal: waiter.principal}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if !c.canGrantLocked(waiter) && context.Cause(waiter.ctx) == nil {
			continue
		}
		preference := c.admissionPreference(waiter)
		if preference < bestPreference {
			bestIndex, bestPreference = index, preference
		}
	}
	return bestIndex
}

func (c *ProcessController) admissionPreference(waiter *admissionWaiter) int {
	if waiter.tenant != c.lastTenant {
		return 0
	}
	if waiter.principal != c.lastPrincipal {
		return 1
	}
	return 2
}

func (c *ProcessController) grantLocked(waiter *admissionWaiter) *WorkLease {
	leaseCtx, cancel := context.WithCancelCause(waiter.ctx)
	lease := &WorkLease{
		controller: c, request: waiter.request, tenant: waiter.tenant, principal: waiter.principal,
		revision: c.revision, ctx: leaseCtx, cancel: cancel, phase: WorkExecuting,
	}
	waiter.lease = lease
	c.active[lease] = struct{}{}
	c.stats.InFlight++
	c.stats.ReservedBytes += waiter.request.ReservedBytes
	c.stats.ResultBufferBytes += waiter.request.ResultBufferBytes
	if waiter.request.Kind == WorkStream {
		c.stats.Streams++
	}
	if waiter.request.Kind == WorkRemote {
		c.stats.RemoteConnections++
	}
	c.tenantActive[waiter.tenant]++
	c.principalActive[waiter.principal]++
	c.lastTenant = waiter.tenant
	c.lastPrincipal = waiter.principal
	c.notifyLocked()
	return lease
}

func (c *ProcessController) waitForAdmission(ctx context.Context, waiter *admissionWaiter) (*WorkLease, error) {
	select {
	case <-waiter.ready:
		if waiter.err != nil {
			return nil, waiter.err
		}
		if err := context.Cause(ctx); err != nil {
			waiter.lease.Release()
			return nil, err
		}
		return waiter.lease, nil
	case <-ctx.Done():
	}
	c.eventMu.Lock()
	c.mu.Lock()
	if waiter.err != nil {
		err := waiter.err
		c.mu.Unlock()
		c.eventMu.Unlock()
		return nil, err
	}
	if waiter.lease != nil {
		lease := waiter.lease
		c.mu.Unlock()
		c.eventMu.Unlock()
		lease.Release()
		return nil, context.Cause(ctx)
	}
	for index, queued := range c.queue {
		if queued != waiter {
			continue
		}
		c.removeQueuedLocked(index)
		break
	}
	waiter.err = context.Cause(ctx)
	events := []ProcessEvent{c.processEventLocked(ProcessEventAdmissionCancelled, waiter.request.Kind, TelemetryCancelledOutcome, CodeCancelled)}
	events = append(events, c.grantQueuedLocked()...)
	c.mu.Unlock()
	c.emitProcessEvents(events...)
	c.eventMu.Unlock()
	return nil, waiter.err
}

func (c *ProcessController) releaseLease(lease *WorkLease) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.mu.Lock()
	if lease.released {
		c.mu.Unlock()
		return
	}
	lease.released = true
	delete(c.active, lease)
	c.stats.InFlight--
	c.stats.ReservedBytes -= lease.request.ReservedBytes
	c.stats.ResultBufferBytes -= lease.request.ResultBufferBytes
	if lease.request.Kind == WorkStream {
		c.stats.Streams--
	}
	if lease.request.Kind == WorkRemote {
		c.stats.RemoteConnections--
	}
	decrementPartition(c.tenantActive, lease.tenant)
	decrementPartition(c.principalActive, lease.principal)
	lease.cancel(nil)
	events := []ProcessEvent{c.processEventLocked(ProcessEventWorkReleased, lease.request.Kind, TelemetrySucceeded, "")}
	if c.state == ProcessReady {
		events = append(events, c.grantQueuedLocked()...)
	}
	if c.state == ProcessDraining && c.stats.InFlight == 0 {
		c.state = ProcessStopped
		c.notifyLocked()
		events = append(events, c.processEventLocked(ProcessEventDrainCompleted, "", TelemetrySucceeded, ""))
	}
	c.notifyLocked()
	stopHooks := (c.state == ProcessStopped || c.state == ProcessForced) && c.stats.InFlight == 0
	c.mu.Unlock()
	c.emitProcessEvents(events...)
	if stopHooks {
		c.stopProcessHooksLocked()
	}
}
