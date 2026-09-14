// Package qualityharness provides Go-only test instrumentation for the
// language-neutral conformance suite. It is internal implementation tooling,
// not a protocol or schema authority.
package qualityharness

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
)

const (
	CodeInvalidConfiguration = "QUALITY_INVALID_CONFIGURATION"
	CodeGoroutineLeak        = "QUALITY_GOROUTINE_LEAK"
	CodeBodyLeak             = "QUALITY_BODY_LEAK"
	CodeStreamLeak           = "QUALITY_STREAM_LEAK"
	CodeFaultInjected        = "QUALITY_FAULT_INJECTED"
	CodeBudgetExceeded       = "QUALITY_BUDGET_EXCEEDED"
)

// Resource identifies a resource whose lifecycle must end before a harness
// case completes.
type Resource string

const (
	Goroutine Resource = "goroutine"
	Body      Resource = "body"
	Stream    Resource = "stream"
)

// Failure is the sanitized public shape produced by the Go quality harness.
// It intentionally retains no injected point, credential, cause, stack, or
// implementation-specific resource description.
type Failure struct {
	Code string
}

func (f *Failure) Error() string { return "Go quality harness failed" }

func (f *Failure) qualityCode() string { return f.Code }

// Tracker accounts for goroutines, response bodies, and streams owned by one
// harness case. The zero value is ready for use.
type Tracker struct {
	goroutines atomic.Int64
	bodies     atomic.Int64
	streams    atomic.Int64
	stateMu    sync.Mutex
	zero       chan struct{}
}

// Acquire records ownership and returns an idempotent release function.
func (t *Tracker) Acquire(resource Resource) (func(), error) {
	if resource == Goroutine {
		return t.acquireGoroutine(), nil
	}
	counter := t.counter(resource)
	if counter == nil {
		return nil, &Failure{Code: CodeInvalidConfiguration}
	}
	counter.Add(1)
	var once sync.Once
	return func() { once.Do(func() { counter.Add(-1) }) }, nil
}

// Go starts a tracked goroutine. Harness functions must honor cancellation;
// Wait and Check make violations observable without global goroutine counts.
func (t *Tracker) Go(ctx context.Context, function func(context.Context)) error {
	if ctx == nil || function == nil {
		return &Failure{Code: CodeInvalidConfiguration}
	}
	release := t.acquireGoroutine()
	go func() {
		defer release()
		function(ctx)
	}()
	return nil
}

// Wait blocks until tracked goroutines have returned or the supplied context
// ends. It does not create a waiter goroutine of its own.
func (t *Tracker) Wait(ctx context.Context) error {
	if ctx == nil {
		return &Failure{Code: CodeInvalidConfiguration}
	}
	t.stateMu.Lock()
	if t.goroutines.Load() == 0 {
		t.stateMu.Unlock()
		return t.Check()
	}
	done := t.zero
	t.stateMu.Unlock()
	select {
	case <-done:
		return t.Check()
	case <-ctx.Done():
		return &Failure{Code: CodeBudgetExceeded}
	}
}

func (t *Tracker) acquireGoroutine() func() {
	t.stateMu.Lock()
	if t.goroutines.Load() == 0 {
		t.zero = make(chan struct{})
	}
	t.goroutines.Add(1)
	t.stateMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.stateMu.Lock()
			if t.goroutines.Add(-1) == 0 {
				close(t.zero)
			}
			t.stateMu.Unlock()
		})
	}
}

// Check reports the first leaked resource using a stable public code.
func (t *Tracker) Check() error {
	switch {
	case t.goroutines.Load() != 0:
		return &Failure{Code: CodeGoroutineLeak}
	case t.bodies.Load() != 0:
		return &Failure{Code: CodeBodyLeak}
	case t.streams.Load() != 0:
		return &Failure{Code: CodeStreamLeak}
	default:
		return nil
	}
}

func (t *Tracker) counter(resource Resource) *atomic.Int64 {
	switch resource {
	case Goroutine:
		return &t.goroutines
	case Body:
		return &t.bodies
	case Stream:
		return &t.streams
	default:
		return nil
	}
}

// FaultInjector deterministically fails configured invocation ordinals at a
// named test seam. Point names never appear in public failures.
type FaultInjector struct {
	mu       sync.Mutex
	schedule map[string][]uint64
	calls    map[string]uint64
}

var faultPoint = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,63}$`)

// NewFaultInjector validates and copies a deterministic fault schedule.
func NewFaultInjector(schedule map[string][]uint64) (*FaultInjector, error) {
	copySchedule := make(map[string][]uint64, len(schedule))
	for point, ordinals := range schedule {
		if !faultPoint.MatchString(point) || len(ordinals) == 0 {
			return nil, &Failure{Code: CodeInvalidConfiguration}
		}
		copyOrdinals := slices.Clone(ordinals)
		slices.Sort(copyOrdinals)
		if copyOrdinals[0] == 0 {
			return nil, &Failure{Code: CodeInvalidConfiguration}
		}
		for index := 1; index < len(copyOrdinals); index++ {
			if copyOrdinals[index] == copyOrdinals[index-1] {
				return nil, &Failure{Code: CodeInvalidConfiguration}
			}
		}
		copySchedule[point] = copyOrdinals
	}
	return &FaultInjector{schedule: copySchedule, calls: make(map[string]uint64)}, nil
}

// Inject advances one named seam and returns a sanitized failure at a
// configured ordinal.
func (i *FaultInjector) Inject(point string) error {
	if i == nil || !faultPoint.MatchString(point) {
		return &Failure{Code: CodeInvalidConfiguration}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls[point]++
	if slices.Contains(i.schedule[point], i.calls[point]) {
		return &Failure{Code: CodeFaultInjected}
	}
	return nil
}

// Code returns a stable public quality code without exposing an internal
// cause. Unknown errors are classified as invalid harness configuration.
func Code(err error) string {
	var failure interface {
		error
		qualityCode() string
	}
	if errors.As(err, &failure) {
		return failure.qualityCode()
	}
	return CodeInvalidConfiguration
}
