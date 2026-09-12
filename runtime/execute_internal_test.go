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
