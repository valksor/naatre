package conformancerunner

import (
	"context"
	"testing"
)

func TestGoHTTPVerifierComponents(t *testing.T) {
	t.Parallel()
	runner, err := New("../../conformance/v1/suite.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture, _, err := runner.loadGoHTTPFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyGoHTTPBinding(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
	result := runner.verifyGoHTTP(context.Background(), Request{})
	if result.Status != "passed" || result.Profile != coreHTTPProfile || len(result.Evidence) == 0 {
		t.Fatalf("Go HTTP verifier result = %#v", result)
	}
}
