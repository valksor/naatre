package conformancerunner

import (
	"context"
	"path/filepath"
	"testing"
)

func TestToolingProfileUsesPinnedFixture(t *testing.T) {
	t.Parallel()
	runner, err := New(filepath.Join("..", "..", "conformance", "v1", "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := runner.verifyTooling(context.Background(), Request{})
	if result.Status != "passed" || len(result.Capabilities) != 1 || result.Capabilities[0] != toolingProfile || len(result.Evidence) != 1 {
		t.Fatalf("tooling result = %#v", result)
	}
	if result.Evidence[0].Fixture != "v1/tooling.json" {
		t.Fatalf("tooling evidence = %#v", result.Evidence)
	}
}
