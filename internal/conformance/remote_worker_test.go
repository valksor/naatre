package conformance_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type remoteWorkerFixture struct {
	Profile        string `json:"profile"`
	Protocol       string `json:"protocol"`
	FixtureVersion string `json:"fixtureVersion"`
	Schema         struct {
		SharedSchemaProfile string `json:"sharedSchemaProfile"`
		GeneratorProfile    string `json:"generatorProfile"`
		ServerArtifact      string `json:"serverInterfaceArtifact"`
	} `json:"schema"`
	Transport struct {
		Production string `json:"production"`
		Test       string `json:"test"`
		HTTP11     string `json:"http11"`
		Frame      struct {
			HeaderBytes  int    `json:"headerBytes"`
			FlagsBytes   int    `json:"flagsBytes"`
			LengthBytes  int    `json:"lengthBytes"`
			ByteOrder    string `json:"byteOrder"`
			MaximumBytes int    `json:"maximumBytes"`
		} `json:"frame"`
	} `json:"transport"`
	Registration struct {
		Capabilities []string `json:"capabilities"`
		Handlers     []struct {
			ID           string   `json:"id"`
			InputSchema  string   `json:"inputSchema"`
			OutputSchema string   `json:"outputSchema"`
			Codec        string   `json:"codec"`
			Effect       string   `json:"effect"`
			Required     []string `json:"requiredCapabilities"`
		} `json:"handlers"`
	} `json:"registration"`
	Operations []struct {
		Name      string          `json:"name"`
		HandlerID string          `json:"handlerId"`
		Input     json.RawMessage `json:"input"`
		Expected  struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"expected"`
	} `json:"operations"`
	AdmissionCases []struct {
		Name string `json:"name"`
	} `json:"admissionCases"`
	FaultCases []struct {
		Name string `json:"name"`
	} `json:"faultCases"`
	RetryPolicies []struct {
		Effect string `json:"effect"`
	} `json:"retryPolicies"`
	SupportMatrix []struct {
		Surface string `json:"surface"`
	} `json:"supportMatrix"`
	Claims struct {
		ExactlyOnce      bool  `json:"exactlyOnceEffects"`
		ProcessIsolation bool  `json:"processIsolation"`
		HardTermination  bool  `json:"hardTermination"`
		NativeRuntime    bool  `json:"nativeRuntime"`
		Production       bool  `json:"productionGateway"`
		ProductionOwner  int   `json:"productionImplementationOwner"`
		MatrixOwner      int   `json:"completeMatrixOwner"`
		LanguageOwners   []int `json:"languageWorkerOwners"`
	} `json:"claims"`
}

func TestRemoteWorkerContractFixture(t *testing.T) {
	t.Parallel()
	fixture := loadRemoteWorkerFixture(t)
	if fixture.Profile != "worker.remote-1" || fixture.Protocol != "naatre.remote-worker.v1" || fixture.FixtureVersion != "1.0.0" {
		t.Fatalf("remote-worker fixture identity = %s/%s/%s", fixture.Profile, fixture.Protocol, fixture.FixtureVersion)
	}
	if fixture.Schema.SharedSchemaProfile != "core.schema-1" || fixture.Schema.GeneratorProfile != "sdk.generation-1" || fixture.Schema.ServerArtifact != "remote-worker-server-1" {
		t.Fatalf("remote-worker schema/generator boundary = %#v", fixture.Schema)
	}
	if fixture.Transport.Production != "http2-tls-length-delimited-1" || fixture.Transport.Test != "stdio-length-delimited-1" ||
		fixture.Transport.HTTP11 != "unary-only-when-advertised" || fixture.Transport.Frame.HeaderBytes != 5 || fixture.Transport.Frame.FlagsBytes != 1 ||
		fixture.Transport.Frame.LengthBytes != 4 || fixture.Transport.Frame.ByteOrder != "big-endian" || fixture.Transport.Frame.MaximumBytes < 1 {
		t.Fatalf("remote-worker transport contract = %#v", fixture.Transport)
	}
	if !slices.Contains(fixture.Registration.Capabilities, "unary-1") || len(fixture.Registration.Handlers) != 1 {
		t.Fatalf("remote-worker registration = %#v", fixture.Registration)
	}
	handler := fixture.Registration.Handlers[0]
	if handler.ID != "fixture.greet" || handler.InputSchema != "GreetInput" || handler.OutputSchema != "GreetOutput" || handler.Codec != "naatre.json-1" || handler.Effect != "query" || len(handler.Required) != 0 {
		t.Fatalf("remote-worker handler = %#v", handler)
	}
	assertNamedRemoteCases(t, "operations", remoteOperationNames(fixture.Operations), []string{"schema-defined-success", "schema-defined-error"})
	assertNamedRemoteCases(t, "admission", remoteAdmissionNames(fixture.AdmissionCases), []string{"wrong-schema-revision", "forged-service-identity", "forged-delegated-context", "unknown-handler", "unsupported-transaction", "malformed-worker-data", "invalid-worker-output"})
	assertNamedRemoteCases(t, "fault", remoteFaultNames(fixture.FaultCases), []string{"reconnect-before-write", "process-death-after-query-write", "process-death-after-mutation-write", "duplicate-invocation", "cancellation-acknowledgement", "worker-overload", "stale-reference", "stream-backpressure"})
	if effects := remoteRetryEffects(fixture.RetryPolicies); !slices.Equal(effects, []string{"query", "mutation", "transaction", "subscription"}) {
		t.Fatalf("retry policy effects = %v", effects)
	}
	if surfaces := remoteSupportSurfaces(fixture.SupportMatrix); !slices.Equal(surfaces, []string{"client", "codec", "native-runtime", "remote-worker", "http-server", "streaming", "transaction"}) {
		t.Fatalf("support matrix surfaces = %v", surfaces)
	}
	if fixture.Claims.ExactlyOnce || fixture.Claims.ProcessIsolation || fixture.Claims.HardTermination || fixture.Claims.NativeRuntime || fixture.Claims.Production ||
		fixture.Claims.ProductionOwner != 88 || fixture.Claims.MatrixOwner != 69 || !slices.Equal(fixture.Claims.LanguageOwners, []int{58, 59, 60, 61}) {
		t.Fatalf("remote-worker claims overstate evidence: %#v", fixture.Claims)
	}
}

func loadRemoteWorkerFixture(t testing.TB) remoteWorkerFixture {
	t.Helper()
	fixtureFile, err := os.Open("../../conformance/v1/remote-workers.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fixtureFile.Close(); err != nil {
			t.Errorf("close remote-worker fixture: %v", err)
		}
	}()
	var fixture remoteWorkerFixture
	if err := json.NewDecoder(fixtureFile).Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestRemoteWorkerSchemaIsClosedAndLanguageNeutral(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../spec/v1/remote-worker.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	if string(document["$schema"]) != `"https://json-schema.org/draft/2020-12/schema"` || len(document["oneOf"]) == 0 || len(document["$defs"]) == 0 {
		t.Fatal("remote-worker schema requires draft 2020-12 variants and definitions")
	}
	var definitions map[string]json.RawMessage
	if err := json.Unmarshal(document["$defs"], &definitions); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"registration", "invocation", "result", "cancellation"} {
		var shape struct {
			AdditionalProperties bool     `json:"additionalProperties"`
			Required             []string `json:"required"`
		}
		if err := json.Unmarshal(definitions[name], &shape); err != nil || shape.AdditionalProperties || len(shape.Required) == 0 {
			t.Fatalf("remote-worker schema definition %s is not closed and required: %#v, %v", name, shape, err)
		}
	}
}

func assertNamedRemoteCases(t *testing.T, label string, actual, required []string) {
	t.Helper()
	for _, name := range required {
		if !slices.Contains(actual, name) {
			t.Errorf("%s cases omit %q", label, name)
		}
	}
}

func remoteOperationNames(values []struct {
	Name      string          `json:"name"`
	HandlerID string          `json:"handlerId"`
	Input     json.RawMessage `json:"input"`
	Expected  struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"expected"`
}) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].Name
	}
	return result
}

func remoteAdmissionNames(values []struct {
	Name string `json:"name"`
}) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].Name
	}
	return result
}
func remoteFaultNames(values []struct {
	Name string `json:"name"`
}) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].Name
	}
	return result
}
func remoteRetryEffects(values []struct {
	Effect string `json:"effect"`
}) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].Effect
	}
	return result
}
func remoteSupportSurfaces(values []struct {
	Surface string `json:"surface"`
}) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].Surface
	}
	return result
}
