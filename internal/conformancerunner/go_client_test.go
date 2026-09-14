package conformancerunner

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGoClientVerifierComponents(t *testing.T) {
	runner, err := New(filepath.Join("..", "..", "conformance", "v1", "suite.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixture, fixtureEvidence, err := runner.loadGoClientFixture()
	if err != nil {
		t.Fatalf("loadGoClientFixture: %v", err)
	}
	if fixtureEvidence.Fixture != "v1/go-client.json" {
		t.Fatalf("fixture evidence = %#v", fixtureEvidence)
	}
	evidence, contents, err := runner.verifyGoClientEvidence(fixture)
	if err != nil {
		t.Fatalf("verifyGoClientEvidence: %v", err)
	}
	if len(evidence) == 0 {
		t.Fatal("verifyGoClientEvidence returned no evidence")
	}
	if err := verifyGoClientRequest(contents[fixture.Sources.Model.Path], contents[fixture.Sources.Output.Path]); err != nil {
		t.Fatalf("verifyGoClientRequest: %v", err)
	}
	if err := verifyGoClientTransport(context.Background(), fixture); err != nil {
		t.Fatalf("verifyGoClientTransport: %v", err)
	}
	result := runner.verifyGoClient(context.Background(), Request{})
	if result.Status != "passed" || len(result.Capabilities) != 1 || result.Capabilities[0] != goClientProfile {
		t.Fatalf("verifyGoClient result = %#v", result)
	}
}
