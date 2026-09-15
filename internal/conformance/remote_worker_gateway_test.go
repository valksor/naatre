package conformance_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type remoteWorkerEvidenceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type remoteWorkerGatewayEvidence struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	OwnerIssue   int    `json:"ownerIssue"`
	Dependencies []struct {
		Profile    string                     `json:"profile"`
		OwnerIssue int                        `json:"ownerIssue"`
		GitCommit  string                     `json:"gitCommit"`
		Files      []remoteWorkerEvidenceFile `json:"files"`
	} `json:"dependencies"`
	Implementation struct {
		Module    string                     `json:"module"`
		MinimumGo string                     `json:"minimumGo"`
		Files     []remoteWorkerEvidenceFile `json:"files"`
	} `json:"implementation"`
	StablePublicCodes []string `json:"stablePublicCodes"`
	Cases             []struct {
		Name   string `json:"name"`
		Class  string `json:"class"`
		Code   string `json:"code"`
		Test   string `json:"test"`
		Result string `json:"result"`
	} `json:"cases"`
	RuntimeBoundary struct {
		Unsupported []string `json:"unsupported"`
	} `json:"runtimeBoundary"`
	VerificationCommands [][]string `json:"verificationCommands"`
}

func TestRemoteWorkerGatewayEvidence(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../conformance/v1/remote-worker-gateway.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence remoteWorkerGatewayEvidence
	if err := json.Unmarshal(content, &evidence); err != nil {
		t.Fatal(err)
	}
	verifyRemoteWorkerIdentity(t, evidence)
	for _, dependency := range evidence.Dependencies {
		if len(dependency.GitCommit) != 40 {
			t.Errorf("dependency %s commit is not exact", dependency.Profile)
		}
		verifyRemoteWorkerEvidenceFiles(t, dependency.Files)
	}
	verifyRemoteWorkerEvidenceFiles(t, evidence.Implementation.Files)

	verifyRemoteWorkerCases(t, evidence)
	if len(evidence.RuntimeBoundary.Unsupported) == 0 || len(evidence.VerificationCommands) == 0 {
		t.Fatal("runtime boundary or reproducible commands are absent")
	}
}

func verifyRemoteWorkerIdentity(t *testing.T, evidence remoteWorkerGatewayEvidence) {
	t.Helper()
	if evidence.Profile != "implementation.go.remote-worker-1" || evidence.FixtureSuite != "1.0.0" || evidence.OwnerIssue != 88 ||
		evidence.Implementation.Module != "github.com/valksor/naatre" || evidence.Implementation.MinimumGo != "1.27" {
		t.Fatalf("remote-worker gateway identity = %#v", evidence)
	}
	if len(evidence.Dependencies) != 2 || evidence.Dependencies[0].Profile != "worker.remote-1" || evidence.Dependencies[0].OwnerIssue != 51 ||
		evidence.Dependencies[1].Profile != "core.streaming-1" || evidence.Dependencies[1].OwnerIssue != 23 {
		t.Fatalf("remote-worker gateway dependencies = %#v", evidence.Dependencies)
	}
}

func verifyRemoteWorkerCases(t *testing.T, evidence remoteWorkerGatewayEvidence) {
	t.Helper()
	required := []string{"unary", "streaming", "schema-mismatch", "forged-identity", "overload", "cancellation", "process-death", "duplicate-invocation", "stale-reference", "bounded-backpressure"}
	classes := make(map[string]bool)
	names := make(map[string]bool)
	for _, test := range evidence.Cases {
		classes[test.Class] = true
		names[test.Name] = true
		if test.Result != "passed" || test.Test == "" || (test.Code != "" && !slices.Contains(evidence.StablePublicCodes, test.Code)) {
			t.Errorf("invalid executed case %#v", test)
		}
	}
	for _, name := range required {
		if !names[name] {
			t.Errorf("missing acceptance case %s", name)
		}
	}
	for _, class := range []string{"positive", "negative", "malformed", "boundary", "cancellation", "limit", "security"} {
		if !classes[class] {
			t.Errorf("missing fixture class %s", class)
		}
	}
}

func verifyRemoteWorkerEvidenceFiles(t *testing.T, files []remoteWorkerEvidenceFile) {
	t.Helper()
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join("../..", filepath.FromSlash(file.Path)))
		if err != nil {
			t.Errorf("read %s: %v", file.Path, err)
			continue
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(content))
		if actual != file.SHA256 {
			t.Errorf("%s sha256 = %s, want %s", file.Path, actual, file.SHA256)
		}
	}
}
