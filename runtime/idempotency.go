package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"
)

// IdempotencyDurability states what survives a provider restart.
type IdempotencyDurability string

const (
	IdempotencyProcessLocal IdempotencyDurability = "process-local"
	IdempotencyDurable      IdempotencyDurability = "durable"
)

// IdempotencyPolicy declares whether repeating a handler invocation is safe.
// Conditional handlers require a protected idempotency key; non-idempotent
// handlers can never opt into automatic replay.
type IdempotencyPolicy string

const (
	IdempotencyNonIdempotent IdempotencyPolicy = "non-idempotent"
	IdempotencyIdempotent    IdempotencyPolicy = "idempotent"
	IdempotencyConditional   IdempotencyPolicy = "conditionally-idempotent"
)

// IdempotencyState is the lifecycle of a protected logical execution.
type IdempotencyState string

const (
	IdempotencyRunning       IdempotencyState = "running"
	IdempotencyCompleted     IdempotencyState = "completed"
	IdempotencyIndeterminate IdempotencyState = "indeterminate"
)

var (
	ErrIdempotencyConflict      = errors.New("naatre: idempotency key fingerprint conflict")
	ErrStaleIdempotencyLease    = errors.New("naatre: stale idempotency lease")
	ErrIdempotencyIndeterminate = errors.New("naatre: idempotency outcome is indeterminate")
)

// IdempotencyClaimRequest names and fingerprints one logical execution.
type IdempotencyClaimRequest struct {
	Scope         string
	Operation     string
	Group         string
	Key           string
	Fingerprint   string
	Now           time.Time
	LeaseDuration time.Duration
}

// IdempotencyClaim either grants a fenced lease or returns a retained result.
type IdempotencyClaim struct {
	State        IdempotencyState
	Fence        uint64
	Outcome      Outcome
	Deduplicated bool
}

// IdempotencyCompletion records the exact result produced by a fenced owner.
type IdempotencyCompletion struct {
	Scope       string
	Operation   string
	Group       string
	Key         string
	Fingerprint string
	Fence       uint64
	Outcome     Outcome
	Now         time.Time
	Retention   time.Duration
}

// IdempotencyIndeterminateUpdate fences a result whose effect may have occurred.
type IdempotencyIndeterminateUpdate struct {
	Scope       string
	Operation   string
	Group       string
	Key         string
	Fingerprint string
	Fence       uint64
}

// IdempotencyStore coordinates protected executions. Durable implementations
// must persist claims, fencing tokens, and results atomically at their boundary.
type IdempotencyStore interface {
	Claim(context.Context, IdempotencyClaimRequest) (IdempotencyClaim, error)
	Complete(context.Context, IdempotencyCompletion) error
	MarkIndeterminate(context.Context, IdempotencyIndeterminateUpdate) error
	Durability() IdempotencyDurability
}

type idempotencyRecordKey struct {
	scope, operation, group, key string
}

type memoryIdempotencyRecord struct {
	fingerprint string
	state       IdempotencyState
	fence       uint64
	leaseUntil  time.Time
	expiresAt   time.Time
	outcome     Outcome
	changed     chan struct{}
}

// MemoryIdempotencyStore coordinates callers only within one process. It does
// not claim crash recovery or exactly-once external effects.
type MemoryIdempotencyStore struct {
	mu      sync.Mutex
	records map[idempotencyRecordKey]*memoryIdempotencyRecord
	next    uint64
}

func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{records: make(map[idempotencyRecordKey]*memoryIdempotencyRecord)}
}

func (*MemoryIdempotencyStore) Durability() IdempotencyDurability {
	return IdempotencyProcessLocal
}

func (s *MemoryIdempotencyStore) Claim(ctx context.Context, request IdempotencyClaimRequest) (IdempotencyClaim, error) {
	key := idempotencyRecordKey{request.Scope, request.Operation, request.Group, request.Key}
	for {
		s.mu.Lock()
		record, ok := s.records[key]
		if !ok || record.state == IdempotencyCompleted && !request.Now.Before(record.expiresAt) {
			s.next++
			record = &memoryIdempotencyRecord{
				fingerprint: request.Fingerprint,
				state:       IdempotencyRunning,
				fence:       s.next,
				leaseUntil:  request.Now.Add(request.LeaseDuration),
				changed:     make(chan struct{}),
			}
			s.records[key] = record
			claim := IdempotencyClaim{State: IdempotencyRunning, Fence: record.fence}
			s.mu.Unlock()
			return claim, nil
		}
		if record.fingerprint != request.Fingerprint {
			s.mu.Unlock()
			return IdempotencyClaim{}, ErrIdempotencyConflict
		}
		switch record.state {
		case IdempotencyCompleted:
			claim := IdempotencyClaim{State: IdempotencyCompleted, Outcome: cloneOutcome(record.outcome), Deduplicated: true}
			s.mu.Unlock()
			return claim, nil
		case IdempotencyIndeterminate:
			claim := IdempotencyClaim{State: IdempotencyIndeterminate, Fence: record.fence, Deduplicated: true}
			s.mu.Unlock()
			return claim, ErrIdempotencyIndeterminate
		case IdempotencyRunning:
			if !request.Now.Before(record.leaseUntil) {
				record.state = IdempotencyIndeterminate
				close(record.changed)
				record.changed = make(chan struct{})
				claim := IdempotencyClaim{State: IdempotencyIndeterminate, Fence: record.fence, Deduplicated: true}
				s.mu.Unlock()
				return claim, ErrIdempotencyIndeterminate
			}
			changed := record.changed
			leaseUntil := record.leaseUntil
			s.mu.Unlock()
			timer := time.NewTimer(leaseUntil.Sub(request.Now))
			select {
			case <-ctx.Done():
				stopTimer(timer)
				return IdempotencyClaim{}, context.Cause(ctx)
			case <-changed:
				stopTimer(timer)
			case <-timer.C:
				request.Now = leaseUntil
			}
		}
	}
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (s *MemoryIdempotencyStore) Complete(_ context.Context, completion IdempotencyCompletion) error {
	key := idempotencyRecordKey{completion.Scope, completion.Operation, completion.Group, completion.Key}
	return s.transition(key, completion.Fingerprint, completion.Fence, func(record *memoryIdempotencyRecord) {
		record.state = IdempotencyCompleted
		record.outcome = cloneOutcome(completion.Outcome)
		record.expiresAt = completion.Now.Add(completion.Retention)
	})
}

func (s *MemoryIdempotencyStore) MarkIndeterminate(_ context.Context, update IdempotencyIndeterminateUpdate) error {
	key := idempotencyRecordKey{update.Scope, update.Operation, update.Group, update.Key}
	return s.transition(key, update.Fingerprint, update.Fence, func(record *memoryIdempotencyRecord) {
		record.state = IdempotencyIndeterminate
	})
}

func (s *MemoryIdempotencyStore) transition(key idempotencyRecordKey, fingerprint string, fence uint64, apply func(*memoryIdempotencyRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok || record.state != IdempotencyRunning || record.fingerprint != fingerprint || record.fence != fence {
		return ErrStaleIdempotencyLease
	}
	apply(record)
	close(record.changed)
	record.changed = make(chan struct{})
	return nil
}

func cloneOutcome(input Outcome) Outcome {
	output := input
	output.Data = cloneStoredValue(input.Data)
	output.Errors = slices.Clone(input.Errors)
	output.Capabilities = slices.Clone(input.Capabilities)
	output.Extensions = slices.Clone(input.Extensions)
	for index := range output.Errors {
		output.Errors[index].Path = cloneStoredValue(input.Errors[index].Path)
		output.Errors[index].Details = cloneStoredValue(input.Errors[index].Details)
	}
	recorder := &directiveAnnotationRecorder{}
	for index, annotation := range input.Annotations {
		recorder.record(annotation, index)
	}
	output.Annotations = recorder.annotations()
	return output
}

func cloneStoredValue[T any](input T) T {
	value, err := copyOutput(reflect.ValueOf(input), 0, &copyState{
		active: make(map[copyReference]bool), limits: DefaultResourceLimits(),
	})
	if err != nil {
		return input
	}
	cloned, ok := value.(T)
	if !ok {
		return input
	}
	return cloned
}
