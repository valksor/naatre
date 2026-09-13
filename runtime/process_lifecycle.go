package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

// CleanupContext returns a fresh timeout that does not inherit request
// cancellation. Callers must invoke the returned cancel function.
func (l *WorkLease) CleanupContext(kind CleanupKind) (context.Context, context.CancelFunc, error) {
	if l == nil || l.controller == nil {
		return nil, nil, errors.New("process work lease is nil")
	}
	var timeout time.Duration
	switch kind {
	case CleanupRollback:
		timeout = l.controller.config.Cleanup.Rollback
	case CleanupLeaseRelease:
		timeout = l.controller.config.Cleanup.LeaseRelease
	case CleanupSourceClose:
		timeout = l.controller.config.Cleanup.SourceClose
	default:
		return nil, nil, errors.New("unsupported process cleanup kind")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), timeout)
	return ctx, cancel, nil
}

// Drain closes admission, applies configured cooperative cancellation, and
// waits only through the configured process deadline. Forced drain cancels
// contexts but never fabricates release of still-owned work.
func (c *ProcessController) Drain(ctx context.Context) DrainOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	drainCtx, cancel := context.WithTimeoutCause(ctx, c.config.MaxDrainDuration, ErrProcessForced)
	defer cancel()
	if outcome, done := c.beginDrain(); done {
		return outcome
	}
	for {
		c.mu.Lock()
		state := c.state
		changed := c.changed
		c.mu.Unlock()
		switch state {
		case ProcessStopped:
			return DrainCompleted
		case ProcessForced:
			return DrainForced
		case ProcessStarting, ProcessReady, ProcessDraining:
		}
		select {
		case <-changed:
		case <-drainCtx.Done():
			return c.forceDrain()
		}
	}
}

func (c *ProcessController) beginDrain() (DrainOutcome, bool) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.mu.Lock()
	switch c.state {
	case ProcessStopped:
		c.mu.Unlock()
		return DrainCompleted, true
	case ProcessForced:
		c.mu.Unlock()
		return DrainForced, true
	case ProcessDraining:
		c.mu.Unlock()
		return "", false
	case ProcessStarting, ProcessReady:
	}
	c.state = ProcessDraining
	c.notifyLocked()
	events := []ProcessEvent{c.processEventLocked(ProcessEventDrainStarted, "", TelemetryActive, "")}
	events = append(events, c.rejectQueuedLocked(ErrProcessDraining)...)
	for lease := range c.active {
		if c.cancelDuringDrainLocked(lease) {
			lease.cancel(ErrProcessDraining)
		}
	}
	if c.stats.InFlight == 0 {
		c.state = ProcessStopped
		c.notifyLocked()
		events = append(events, c.processEventLocked(ProcessEventDrainCompleted, "", TelemetrySucceeded, ""))
	}
	state := c.state
	c.mu.Unlock()
	c.emitProcessEvents(events...)
	if state == ProcessStopped {
		c.stopProcessHooksLocked()
	}
	return DrainCompleted, state == ProcessStopped
}

func (c *ProcessController) rejectQueuedLocked(cause error) []ProcessEvent {
	events := make([]ProcessEvent, 0, len(c.queue))
	for len(c.queue) != 0 {
		waiter := c.removeQueuedLocked(0)
		waiter.err = c.admissionError(CodeOverloaded, cause)
		close(waiter.ready)
		events = append(events, c.processEventLocked(ProcessEventAdmissionRejected, waiter.request.Kind, TelemetryFailedOutcome, CodeOverloaded))
	}
	return events
}

func (c *ProcessController) cancelDuringDrainLocked(lease *WorkLease) bool {
	if lease.request.Kind == WorkMutation && lease.phase == WorkCommitting {
		return false
	}
	var action DrainAction
	switch lease.request.Kind {
	case WorkQuery:
		action = c.config.Drain.Query
	case WorkMutation:
		action = c.config.Drain.Mutation
	case WorkStream:
		action = c.config.Drain.Stream
	case WorkRemote:
		action = c.config.Drain.Remote
	case WorkDurable:
		action = c.config.Drain.Durable
	}
	return action == DrainCancel
}

func (c *ProcessController) forceDrain() DrainOutcome {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.mu.Lock()
	if c.state == ProcessStopped {
		c.mu.Unlock()
		return DrainCompleted
	}
	if c.state == ProcessForced {
		c.mu.Unlock()
		return DrainForced
	}
	c.state = ProcessForced
	for lease := range c.active {
		lease.cancel(ErrProcessForced)
	}
	events := c.rejectQueuedLocked(ErrProcessForced)
	c.notifyLocked()
	events = append(events, c.processEventLocked(ProcessEventDrainForced, "", TelemetryFailedOutcome, CodeResourceExhausted))
	stopHooks := c.stats.InFlight == 0
	c.mu.Unlock()
	c.emitProcessEvents(events...)
	if stopHooks {
		c.stopProcessHooksLocked()
	}
	return DrainForced
}

// ConnectionDeadline returns a finite, deterministic staggered rotation bound.
// Identity expiry shortens but never extends the configured lifetime.
func (c *ProcessController) ConnectionDeadline(reference string, established, identityExpiry time.Time) time.Time {
	maximum := established.Add(c.config.MaxConnectionLifetime)
	window := uint64(c.config.ReconnectStagger)
	if window != 0 {
		digest := sha256.Sum256([]byte(reference))
		offset := time.Duration(binary.BigEndian.Uint64(digest[:8]) % (window + 1))
		maximum = maximum.Add(-offset)
	}
	if !identityExpiry.IsZero() && identityExpiry.Before(maximum) {
		if identityExpiry.Before(established) {
			return established
		}
		return identityExpiry
	}
	return maximum
}
