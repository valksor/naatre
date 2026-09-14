package conformancerunner

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGoSDKVerifierComponents(t *testing.T) {
	runner, err := New(filepath.Join("..", "..", "conformance", "v1", "suite.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := runner.verifyGoSDK(context.Background(), Request{})
	if result.Status != "passed" || len(result.Capabilities) != 1 || result.Capabilities[0] != goSDKProfile || len(result.Evidence) < 5 {
		t.Fatalf("verifyGoSDK result = %#v", result)
	}
}
