package runtime

import (
	"context"
	"testing"
)

func TestCompleteRecoveringContainsPanics(t *testing.T) {
	completed, issues, available, err := completeRecovering(func() (any, []completionIssue, bool, error) {
		panic("private completion panic")
	})
	if completed != nil || issues != nil || available || err == nil || err.Error() != "completion hook panic" {
		t.Fatalf("completion panic result = (%#v, %#v, %t, %v)", completed, issues, available, err)
	}
}

func TestTransactionStatePreservesExplicitAggregateStates(t *testing.T) {
	tests := []struct {
		name   string
		states []EffectState
		want   EffectState
	}{
		{name: "partially applied", states: []EffectState{EffectPartiallyApplied}, want: EffectPartiallyApplied},
		{name: "non-effects", states: []EffectState{EffectNotApplicable, EffectNone}, want: EffectNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &effectRecorder{}
			for _, state := range tt.states {
				recorder.recordTransactionState(state)
			}
			if got, ok := recorder.transactionState(); !ok || got != tt.want {
				t.Fatalf("transactionState = %q, %v; want %q, true", got, ok, tt.want)
			}
		})
	}
}

func TestAcquireHandlerExecutionSlotAbandonsCacheReservationOnCancellation(t *testing.T) {
	cache := &executionCache{entries: make(map[string]*cacheRecord)}
	record := &cacheRecord{ready: make(chan struct{})}
	const localKey = "0:reserved"
	cache.entries[localKey] = record
	reservation := &cacheReservation{
		cache: cache, key: CacheKey{Generation: 0}, localKey: localKey, record: record,
	}
	limiter := make(chan struct{}, 1)
	limiter <- struct{}{}
	scope := executionScope{parallel: true, limiter: limiter}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	release, err := acquireHandlerExecutionSlot(ctx, scope, reservation)
	if err == nil || release != nil {
		t.Fatalf("acquireHandlerExecutionSlot returned release=%t err=%v, want nil release and cancellation", release != nil, err)
	}
	select {
	case <-record.ready:
	default:
		t.Fatal("abandoned cache reservation did not wake waiters")
	}
	cache.mu.Lock()
	_, retained := cache.entries[localKey]
	cache.mu.Unlock()
	if retained {
		t.Fatal("abandoned cache reservation remained in request cache")
	}
}
