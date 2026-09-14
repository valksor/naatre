package conformance_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/valksor/naatre/generator"
)

type generatorFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Model        struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"model"`
	ReferenceOutput struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"referenceOutput"`
	Implementations []struct {
		Language string `json:"language"`
		Path     string `json:"path"`
		SHA256   string `json:"sha256"`
	} `json:"implementations"`
	Verifier struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"verifier"`
	SDKBehavior struct {
		ResultStates          []string          `json:"resultStates"`
		Cancellation          map[string]string `json:"cancellation"`
		Retry                 string            `json:"retry"`
		AuthRefresh           string            `json:"authRefresh"`
		RedirectCredentials   string            `json:"redirectCredentials"`
		ResponseBytes         int               `json:"responseBytes"`
		FrameBytes            int               `json:"frameBytes"`
		DecompressedBytes     int               `json:"decompressedBytes"`
		MalformedResponse     string            `json:"malformedResponse"`
		UnknownDomainVariant  string            `json:"unknownDomainVariant"`
		UnknownControlVariant string            `json:"unknownControlVariant"`
	} `json:"sdkBehavior"`
	TransportVectors []struct {
		Name           string `json:"name"`
		Transport      string `json:"transport"`
		Authentication string `json:"authentication"`
		Cancellation   string `json:"cancellation"`
		Terminal       string `json:"terminal"`
	} `json:"transportVectors"`
	HostileInputs []struct {
		Name   string `json:"name"`
		Result string `json:"result"`
		Code   string `json:"code"`
	} `json:"hostileInputs"`
}

func TestLanguageNeutralGeneratorEvidence(t *testing.T) {
	t.Parallel()
	var fixture generatorFixture
	readFixture(t, "generation.json", &fixture)
	if fixture.Profile != "sdk.generation-1" || fixture.FixtureSuite != "1.0.0" {
		t.Fatalf("generator fixture header = %#v", fixture)
	}
	root := filepath.Join("..", "..")
	model := requireGeneratorDigest(t, filepath.Join(root, "conformance", fixture.Model.Path), fixture.Model.SHA256)
	expected := requireGeneratorDigest(t, filepath.Join(root, "conformance", fixture.ReferenceOutput.Path), fixture.ReferenceOutput.SHA256)
	actual, err := generator.Generate(model)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Go generator output mismatch:\n%s\n%s", actual, expected)
	}
	if len(fixture.Implementations) != 2 || fixture.Implementations[0].Language != "go" || fixture.Implementations[1].Language != "javascript-typescript" {
		t.Fatalf("generator implementations = %#v", fixture.Implementations)
	}
	for _, implementation := range fixture.Implementations {
		requireGeneratorDigest(t, filepath.Join(root, implementation.Path), implementation.SHA256)
	}
	if fixture.Verifier.Path == "" || fixture.Verifier.SHA256 == "" {
		t.Fatal("generator verifier evidence is missing")
	}
	requireGeneratorDigest(t, filepath.Join(root, fixture.Verifier.Path), fixture.Verifier.SHA256)
	command := exec.Command("node", filepath.Join(root, fixture.Implementations[1].Path), filepath.Join(root, "conformance", fixture.Model.Path))
	command.WaitDelay = 2 * time.Second
	independent, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("independent generator: %v: %s", err, independent)
	}
	if !bytes.Equal(independent, expected) {
		t.Fatalf("independent generator output mismatch:\n%s\n%s", independent, expected)
	}
	assertGeneratorOutput(t, expected)
	assertSDKBehavior(t, fixture)
}

func requireGeneratorDigest(t *testing.T, path, expected string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	digest := sha256.Sum256(content)
	actual := hex.EncodeToString(digest[:])
	if actual == expected {
		return content
	}
	t.Fatalf("%s sha256 = %s, want %s", path, actual, expected)
	return nil
}

func assertGeneratorOutput(t *testing.T, content []byte) {
	t.Helper()
	var output struct {
		ModelVersion         string `json:"modelVersion"`
		GeneratorVersion     string `json:"generatorVersion"`
		ProtocolVersion      string `json:"protocolVersion"`
		CanonicalVersion     string `json:"canonicalVersion"`
		ForwardCompatibility struct {
			OpenEnum  string `json:"openEnum"`
			OpenUnion string `json:"openUnion"`
		} `json:"forwardCompatibility"`
		Operations []struct {
			Name      string `json:"name"`
			Artifact  string `json:"artifact"`
			Persisted struct {
				Digest string `json:"digest"`
			} `json:"persisted"`
			Request struct {
				Variables map[string]any `json:"variables"`
			} `json:"request"`
			Result struct {
				Kind string         `json:"kind"`
				Data map[string]any `json:"data"`
			} `json:"result"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(content, &output); err != nil {
		t.Fatalf("decode generator output: %v", err)
	}
	if output.ModelVersion != generator.ModelVersion || output.GeneratorVersion != generator.GeneratorVersion || output.ProtocolVersion != "1" || output.CanonicalVersion != "c14n-1" {
		t.Fatalf("generator versions = %#v", output)
	}
	if output.ForwardCompatibility.OpenEnum == "" || output.ForwardCompatibility.OpenUnion == "" {
		t.Fatalf("missing forward compatibility = %#v", output.ForwardCompatibility)
	}
	if len(output.Operations) != 1 || output.Operations[0].Name != "GetAccount" || output.Operations[0].Artifact != "operations/GetAccount.json" || len(output.Operations[0].Persisted.Digest) != 64 {
		t.Fatalf("generated operation = %#v", output.Operations)
	}
	variables := output.Operations[0].Request.Variables
	if value, present := variables["nickname"]; !present || value != nil {
		t.Fatalf("explicit null missing from request = %#v", variables)
	}
	if _, present := variables["omitted"]; present {
		t.Fatalf("absent variable became present = %#v", variables)
	}
	fields, ok := output.Operations[0].Result.Data["fields"].([]any)
	if output.Operations[0].Result.Kind != "operation-result" || !ok || len(fields) != 2 {
		t.Fatalf("operation-selected result = %#v", output.Operations[0].Result)
	}
}

func assertSDKBehavior(t *testing.T, fixture generatorFixture) {
	t.Helper()
	wantStates := []string{"present", "null", "missing", "failed", "skipped", "pending"}
	if !slices.Equal(fixture.SDKBehavior.ResultStates, wantStates) || fixture.SDKBehavior.Retry != "idempotent-only" || fixture.SDKBehavior.AuthRefresh != "single-flight-no-non-idempotent-replay" || fixture.SDKBehavior.RedirectCredentials != "strip-on-cross-origin" {
		t.Fatalf("SDK behavior = %#v", fixture.SDKBehavior)
	}
	if fixture.SDKBehavior.ResponseBytes <= 0 || fixture.SDKBehavior.FrameBytes <= 0 || fixture.SDKBehavior.DecompressedBytes <= 0 || fixture.SDKBehavior.MalformedResponse != "typed-protocol-error-no-success" || fixture.SDKBehavior.UnknownDomainVariant != "preserve-open-variant" || fixture.SDKBehavior.UnknownControlVariant != "reject" {
		t.Fatalf("SDK limits/hostile behavior = %#v", fixture.SDKBehavior)
	}
	wantTransports := []string{"http", "sse", "websocket"}
	transports := make([]string, len(fixture.TransportVectors))
	for index, vector := range fixture.TransportVectors {
		transports[index] = vector.Transport
		if vector.Cancellation == "" || vector.Terminal == "" || (vector.Transport == "sse" && vector.Authentication != "fetch-headers-and-post-body") {
			t.Fatalf("transport vector = %#v", vector)
		}
	}
	if !slices.Equal(transports, wantTransports) {
		t.Fatalf("transports = %v, want %v", transports, wantTransports)
	}
	wantHostile := []string{"malicious-description", "path-traversal", "symbol-collision", "unmapped-scalar", "oversized-response", "truncated-stream"}
	hostile := make([]string, len(fixture.HostileInputs))
	for index, vector := range fixture.HostileInputs {
		hostile[index] = vector.Name
		if vector.Result == "accepted" && vector.Name != "malicious-description" || vector.Result == "rejected" && vector.Code == "" {
			t.Fatalf("hostile vector = %#v", vector)
		}
	}
	if !slices.Equal(hostile, wantHostile) {
		t.Fatalf("hostile vectors = %v, want %v", hostile, wantHostile)
	}
}
