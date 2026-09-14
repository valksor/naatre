package conformancerunner

import (
	"context"
	"testing"
)

func TestOperationsProfilePublishesPinnedEvidence(t *testing.T) {
	t.Parallel()
	runner, err := New("../../conformance/v1/suite.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture, _, err := runner.loadOperationsFixture()
	if err != nil {
		t.Fatalf("load operations fixture: %v", err)
	}
	if len(fixture.IntegrationCases) != 6 {
		t.Fatalf("integration cases = %d", len(fixture.IntegrationCases))
	}
	result := runner.verifyOperations(context.Background(), Request{})
	if result.Status != "passed" || len(result.Capabilities) != 1 || result.Capabilities[0] != operationsProfile || len(result.Evidence) < 2 {
		t.Fatalf("verify operations result = %#v", result)
	}
}

func TestOperationsBehaviorEvidence(t *testing.T) {
	t.Parallel()
	if err := verifyOperationsBehavior(); err != nil {
		t.Fatal(err)
	}
}
