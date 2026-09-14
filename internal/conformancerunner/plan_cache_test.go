package conformancerunner

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPlanCacheProfilePublishesPinnedEvidence(t *testing.T) {
	runner, err := New(filepath.Join("..", "..", "conformance", "v1", "suite.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixture, fixtureEvidence, err := runner.loadPlanCacheFixture()
	if err != nil {
		t.Fatalf("loadPlanCacheFixture: %v", err)
	}
	if fixtureEvidence.Fixture != "v1/plan-cache.json" || fixture.Profile != planCacheProfile || fixture.Runtime.GoVersion != "1.27.1" {
		t.Fatalf("fixture/evidence = %#v %#v", fixture, fixtureEvidence)
	}
	evidence, err := runner.verifyPlanCacheEvidence(fixture)
	if err != nil || len(evidence) < 5 {
		t.Fatalf("verifyPlanCacheEvidence = %#v, %v", evidence, err)
	}
	result := runner.verifyPlanCache(context.Background(), Request{})
	if result.Status != "passed" || len(result.Capabilities) != 1 || result.Capabilities[0] != planCacheProfile || len(result.Evidence) != len(evidence)+1 {
		t.Fatalf("verifyPlanCache result = %#v", result)
	}
}
