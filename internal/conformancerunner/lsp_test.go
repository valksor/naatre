package conformancerunner

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLSPProfileUsesPinnedDependenciesAndEvidence(t *testing.T) {
	runner := lspTestRunner(t)
	result := runner.verifyLSP(context.Background(), Request{})
	if result.Status != "passed" {
		t.Fatalf("LSP status = %q, code = %q", result.Status, result.Code)
	}
	if len(result.Capabilities) != 1 || result.Capabilities[0] != lspProfile {
		t.Fatalf("LSP capabilities = %v", result.Capabilities)
	}
	if len(result.Evidence) != 9 {
		t.Fatalf("LSP evidence = %#v", result.Evidence)
	}
}

func lspTestRunner(t *testing.T) *Runner {
	t.Helper()
	runner, err := New(filepath.Join("..", "..", "conformance", "v1", "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	return runner
}
