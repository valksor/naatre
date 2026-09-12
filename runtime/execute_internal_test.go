package runtime

import "testing"

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
