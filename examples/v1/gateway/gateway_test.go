package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/valksor/naatre/examples/v1/gateway"
	"github.com/valksor/naatre/remoteworker"
)

func TestQuickStartGatewayRegistersAndInvokes(t *testing.T) {
	t.Parallel()
	var responses, requests bytes.Buffer
	writeResponse(t, &responses, "registered", remoteworker.RegistrationAck{Protocol: remoteworker.ProtocolVersion, WorkerID: "fixture-worker", SessionID: "fixture-session", SchemaRevision: "schema-1", AcceptedCapabilities: []string{remoteworker.CapabilityUnary, remoteworker.CapabilityCancellationAck}})
	writeResponse(t, &responses, "result", remoteworker.WorkerResult{Protocol: remoteworker.ProtocolVersion, InvocationID: "invoke-1", AttemptID: "invoke-1.1", SchemaRevision: "schema-1", Data: json.RawMessage(`{"greeting":"Hello, Ada"}`), Errors: []remoteworker.WorkerError{}})
	transport, err := remoteworker.NewFramedTransport(&responses, &requests, 65536)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(transport)
	if err != nil {
		t.Fatal(err)
	}
	registration := remoteworker.Registration{
		Protocol: remoteworker.ProtocolVersion, WorkerID: "fixture-worker", ServiceIdentity: "spiffe://example/fixture-worker",
		Audience: "naatre-gateway", Endpoint: "fixture-stdio", SchemaRevision: "schema-1",
		SchemaDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Capabilities: []string{remoteworker.CapabilityUnary, remoteworker.CapabilityCancellationAck},
		Limits:       remoteworker.WorkerLimits{MaxInFlight: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096, MaxStreamFrames: 4, MaxStreamBytes: 16384},
		Handlers:     []remoteworker.Handler{{ID: "fixture.greet", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: remoteworker.CodecJSON, Effect: remoteworker.EffectQuery}},
	}
	if err := g.Register(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	result, err := g.Invoke(context.Background(), remoteworker.InvokeRequest{RequestID: "request-1", InvocationID: "invoke-1", HandlerID: "fixture.greet", DelegatedContext: "valid-delegation", Deadline: time.Now().Add(time.Minute), Input: json.RawMessage(`{"name":"Ada"}`)})
	if err != nil || string(result.Data) != `{"greeting":"Hello, Ada"}` || len(result.Errors) != 0 {
		t.Fatalf("result=%s errors=%v err=%v", result.Data, result.Errors, err)
	}
}

func writeResponse(t *testing.T, output *bytes.Buffer, kind string, payload any) {
	t.Helper()
	envelope, err := json.Marshal(struct {
		Protocol string `json:"protocol"`
		Kind     string `json:"kind"`
		Payload  any    `json:"payload"`
	}{Protocol: remoteworker.ProtocolVersion, Kind: kind, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteworker.WriteFrame(output, remoteworker.FrameData, envelope, 65536); err != nil {
		t.Fatal(err)
	}
}
