package runtime

import (
	"context"
	"testing"
)

func TestCollectFederationCallResultsPreservesCompletedSiblingOnCancellation(t *testing.T) {
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan indexedFederationCallResult, 2)
		results <- indexedFederationCallResult{index: 0, result: federationCallResult{data: "completed"}}
		cancel()
		cancelled := federationContextFailure([]any{"pending"}, ctx)
		results <- indexedFederationCallResult{index: 1, result: federationCallResult{failures: []ExecutionError{cancelled}}}

		calls := []FederationCall{{Path: []any{"completed"}}, {Path: []any{"pending"}}}
		collected := collectFederationCallResults(calls, results)
		if collected[0].data != "completed" || len(collected[0].failures) != 0 {
			t.Fatalf("completed sibling was discarded: %#v", collected[0])
		}
		if len(collected[1].failures) != 1 || collected[1].failures[0].Code != CodeCancelled {
			t.Fatalf("pending sibling was not cancelled: %#v", collected[1])
		}
	}
}
