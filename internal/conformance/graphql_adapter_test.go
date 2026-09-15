package conformance_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestGraphQLAdapterEvidence(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "..", "conformance", "v1", "graphql-adapter.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Profile               string   `json:"profile"`
		FixtureSuite          string   `json:"fixtureSuite"`
		UpstreamSpecification string   `json:"upstreamSpecification"`
		Directions            []string `json:"directions"`
		Dependencies          []struct {
			Profile string `json:"profile"`
			Path    string `json:"path"`
			SHA256  string `json:"sha256"`
		} `json:"dependencies"`
		Cases []struct {
			Name   string `json:"name"`
			Class  string `json:"class"`
			Code   string `json:"code"`
			Result string `json:"result"`
		} `json:"cases"`
		Unsupported []string `json:"unsupported"`
		Commands    []string `json:"commands"`
	}
	if err := json.Unmarshal(content, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Profile != "core.adapters.graphql-1" || evidence.FixtureSuite != "1.0.0" || evidence.UpstreamSpecification != "https://spec.graphql.org/September2025/" {
		t.Fatalf("profile identity = %#v", evidence)
	}
	if !slices.Equal(evidence.Directions, []string{"schema-import", "schema-export", "runtime-consume", "runtime-expose"}) {
		t.Fatalf("directions = %v", evidence.Directions)
	}
	for _, dependency := range evidence.Dependencies {
		dependencyBytes, readErr := os.ReadFile(filepath.Join("..", "..", dependency.Path))
		if readErr != nil {
			t.Fatalf("read dependency %s: %v", dependency.Path, readErr)
		}
		digest := sha256.Sum256(dependencyBytes)
		if hex.EncodeToString(digest[:]) != dependency.SHA256 {
			t.Errorf("dependency %s digest drifted", dependency.Path)
		}
	}
	classes := make(map[string]bool)
	for _, testCase := range evidence.Cases {
		classes[testCase.Class] = true
		if testCase.Name == "" || testCase.Result != "passed" {
			t.Errorf("invalid GraphQL evidence case %#v", testCase)
		}
	}
	for _, required := range []string{"positive", "negative", "boundary", "cancellation", "resource-limit", "security"} {
		if !classes[required] {
			t.Errorf("missing %s GraphQL evidence", required)
		}
	}
	for _, unsupported := range []string{"graphql-http-transport", "graphql-websocket-transport", "native-mobile-runtime", "framework-bindings"} {
		if !slices.Contains(evidence.Unsupported, unsupported) {
			t.Errorf("unsupported capability %q is not published", unsupported)
		}
	}
	if !slices.Contains(evidence.Commands, "node conformance/independent/verify-graphql-adapter.mjs") {
		t.Fatal("reproducible independent verifier command is absent")
	}
}
