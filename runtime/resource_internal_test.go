package runtime

import (
	"encoding/json"
	"math"
	"sync/atomic"
	"testing"
)

func FuzzCheckedResourceAddition(f *testing.F) {
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(math.MaxUint64), uint64(1))
	f.Add(uint64(math.MaxUint64-1), uint64(1))
	f.Fuzz(func(t *testing.T, left, right uint64) {
		value, ok := checkedResourceAdd(left, right)
		if right > math.MaxUint64-left {
			if ok {
				t.Fatalf("overflow accepted: %d + %d = %d", left, right, value)
			}
			return
		}
		if !ok || value != left+right {
			t.Fatalf("valid addition rejected: %d + %d = %d, %v", left, right, value, ok)
		}
	})
}

func FuzzResourceCounterNeverExceedsLimit(f *testing.F) {
	f.Add(uint64(10), uint64(4), uint64(6))
	f.Add(uint64(1), uint64(math.MaxUint64), uint64(1))
	f.Fuzz(func(t *testing.T, limit, first, second uint64) {
		var counter atomic.Uint64
		firstAccepted := chargeResourceCounter(&counter, first, limit)
		secondAccepted := chargeResourceCounter(&counter, second, limit)
		if counter.Load() > limit {
			t.Fatalf("counter %d exceeded limit %d", counter.Load(), limit)
		}
		if first > limit && firstAccepted {
			t.Fatalf("accepted first charge %d above limit %d", first, limit)
		}
		if firstAccepted {
			sum, ok := checkedResourceAdd(first, second)
			if secondAccepted != (ok && sum <= limit) {
				t.Fatalf("second acceptance=%v for %d + %d under %d", secondAccepted, first, second, limit)
			}
		}
	})
}

func FuzzBoundedOutcomeRemainsValidJSON(f *testing.F) {
	f.Add(uint16(0), uint16(0))
	f.Add(uint16(2_048), uint16(32))
	f.Fuzz(func(t *testing.T, payloadBytes, errorCount uint16) {
		limits := DefaultResourceLimits()
		limits.MaxOutputBytes = 512
		limits.MaxErrors = 4
		outcome := Outcome{Data: map[string]any{"value": string(make([]byte, payloadBytes))}, Effects: EffectApplied}
		for index := range int(errorCount) {
			outcome.Errors = append(outcome.Errors, ExecutionError{Code: CodeHandlerFailed, Message: "failed", Path: []any{index}})
		}
		bounded := enforceOutcomeLimits(outcome, limits)
		encoded, err := json.Marshal(bounded)
		if err != nil || len(encoded) > int(limits.MaxOutputBytes) || len(bounded.Errors) > int(limits.MaxErrors) {
			t.Fatalf("bounded outcome bytes=%d errors=%d marshal=%v", len(encoded), len(bounded.Errors), err)
		}
	})
}

func TestMinimumOutputEnvelopePreservesTruthfulResourceError(t *testing.T) {
	limits := DefaultResourceLimits()
	limits.MaxOutputBytes = minimumOutputEnvelopeBytes
	limits.MaxErrors = 1
	outcome := enforceOutcomeLimits(Outcome{
		Data:    map[string]any{"large": string(make([]byte, 1024))},
		Effects: EffectApplied,
	}, limits)
	encoded, err := json.Marshal(outcome)
	if err != nil || uint64(len(encoded)) > limits.MaxOutputBytes {
		t.Fatalf("minimum envelope bytes=%d err=%v outcome=%#v", len(encoded), err, outcome)
	}
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != CodeResourceExhausted ||
		len(outcome.Errors[0].Path) == 0 || outcome.Effects != EffectApplied {
		t.Fatalf("minimum envelope lost truthful metadata: %#v", outcome)
	}
}

func TestRawPayloadTraversalIsBounded(t *testing.T) {
	type link struct {
		Next *link
	}
	root := &link{}
	current := root
	for range 1_024 {
		current.Next = &link{}
		current = current.Next
	}
	if rawPayloadWithin(root, 64) {
		t.Fatal("deep pointer graph accepted by a smaller traversal budget")
	}
}
