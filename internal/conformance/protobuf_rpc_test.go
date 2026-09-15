package conformance_test

import (
	"slices"
	"testing"
)

func TestProtobufRPCProfileEvidence(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile      string            `json:"profile"`
		FixtureSuite string            `json:"fixtureSuite"`
		OwnerIssue   int               `json:"ownerIssue"`
		CoreContract string            `json:"coreContract"`
		Dependencies map[string]string `json:"dependencies"`
		Package      string            `json:"package"`
		Lifecycle    string            `json:"lifecycle"`
		Supported    struct {
			Runtime   string   `json:"runtime"`
			Platforms []string `json:"platforms"`
			Protobuf  []string `json:"protobuf"`
			GRPC      []string `json:"grpc"`
			Connect   []string `json:"connect"`
		} `json:"supported"`
		Unsupported []string          `json:"unsupported"`
		Commands    map[string]string `json:"commands"`
		Cases       []struct {
			ID       string   `json:"id"`
			Class    string   `json:"class"`
			Features []string `json:"features"`
			Expected struct {
				Status string `json:"status"`
				Code   string `json:"code"`
			} `json:"expected"`
		} `json:"cases"`
	}
	readFixture(t, "protobuf-grpc-connect.json", &fixture)
	if fixture.Profile != "core.adapters.protobuf-grpc-connect-1" || fixture.FixtureSuite != "1.0.0" || fixture.OwnerIssue != 95 || fixture.CoreContract != "core.adapters-1" || fixture.Package != "github.com/valksor/naatre/transport/protobufrpc" || fixture.Lifecycle == "" {
		t.Fatalf("profile identity = %#v", fixture)
	}
	for key, value := range map[string]string{
		"coreAdaptersCommit": "a93f33dffa5b351c04b4f229df91d098ed4df1b4",
		"protobuf":           "google.golang.org/protobuf@v1.36.11",
		"grpc":               "google.golang.org/grpc@v1.82.1",
		"connectProtocol":    "connectrpc/connect@fac060371d74da4205f28ef504d078d2d2ce286f",
	} {
		if fixture.Dependencies[key] != value {
			t.Errorf("dependency %s = %q, want %q", key, fixture.Dependencies[key], value)
		}
	}
	if fixture.Supported.Runtime != "go1.27" || len(fixture.Supported.Platforms) == 0 || !slices.Contains(fixture.Supported.GRPC, "unary-client") || !slices.Contains(fixture.Supported.GRPC, "server-streaming-client") || !slices.Contains(fixture.Supported.Connect, "unary-protobuf-post") || !slices.Contains(fixture.Supported.Connect, "server-streaming-protobuf-post") {
		t.Fatalf("supported boundary = %#v", fixture.Supported)
	}
	for _, unsupported := range []string{"client-streaming", "bidirectional-streaming", "status-details", "binary-metadata", "grpc-web", "connect-get", "connect-json-codec", "grpc-or-connect-server-runtime-exposure", "protobuf-reflection-or-remote-discovery"} {
		if !slices.Contains(fixture.Unsupported, unsupported) {
			t.Errorf("unsupported list omits %s", unsupported)
		}
	}
	for _, command := range []string{"focused", "package", "offlineQuality"} {
		if fixture.Commands[command] == "" {
			t.Errorf("commands omit %s", command)
		}
	}
	classes := map[string]bool{}
	features := map[string]bool{}
	seen := map[string]bool{}
	for _, test := range fixture.Cases {
		if test.ID == "" || seen[test.ID] || test.Expected.Status == "" {
			t.Errorf("invalid case %#v", test)
		}
		seen[test.ID] = true
		classes[test.Class] = true
		for _, feature := range test.Features {
			features[feature] = true
		}
	}
	for _, class := range []string{"positive", "negative", "boundary", "cancellation", "security", "resource-limit"} {
		if !classes[class] {
			t.Errorf("fixture class omitted: %s", class)
		}
	}
	for _, feature := range []string{"presence", "oneof", "numeric-range", "field-mask", "metadata", "deadline", "status", "server-streaming", "credential-forwarding"} {
		if !features[feature] {
			t.Errorf("acceptance feature omitted: %s", feature)
		}
	}
}
