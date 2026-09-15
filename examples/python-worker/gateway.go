package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/valksor/naatre/remoteworker"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, "python3", "examples/python-worker/worker_service.py")
	command.Env = append(os.Environ(), "PYTHONPATH=sdk/python/src")
	workerInput, err := command.StdinPipe()
	check(err)
	workerOutput, err := command.StdoutPipe()
	check(err)
	command.Stderr = os.Stderr
	check(command.Start())

	transport, err := remoteworker.NewFramedTransport(workerOutput, workerInput, 65_536)
	check(err)
	digest := strings.Repeat("a", 64)
	gateway, err := remoteworker.NewReferenceGateway(remoteworker.GatewayConfig{
		Transport: transport, Endpoint: "python-worker-stdio", WorkerID: "python-example-worker",
		ServiceIdentity: "spiffe://example/python-worker", Audience: "naatre-gateway",
		SchemaRevision: "schema-generator-r1", SchemaDigest: digest,
		MaxInFlight: 1, MaxAttempts: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096,
		MaxStreamFrames: 4, MaxStreamBytes: 16_384, MaxReferences: 16, MaxSeen: 32,
		VerifyDelegation: func(context.Context, string, remoteworker.DelegationExpectation) error { return nil },
		Authorize:        func(context.Context, remoteworker.AuthorizationRequest) error { return nil },
		ValidateInput:    validJSON,
		ValidateOutput:   validJSON,
	})
	check(err)
	check(gateway.Register(ctx, remoteworker.Registration{
		Protocol: remoteworker.ProtocolVersion, WorkerID: "python-example-worker",
		ServiceIdentity: "spiffe://example/python-worker", Audience: "naatre-gateway",
		Endpoint: "python-worker-stdio", SchemaRevision: "schema-generator-r1", SchemaDigest: digest,
		Capabilities: []string{remoteworker.CapabilityUnary, remoteworker.CapabilityCancellationAck},
		Limits: remoteworker.WorkerLimits{
			MaxInFlight: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096,
			MaxStreamFrames: 4, MaxStreamBytes: 16_384,
		},
		Handlers: []remoteworker.Handler{{
			ID: "example.get-account", InputSchema: "GetAccountVariables",
			OutputSchema: "GetAccountServerResult", Codec: remoteworker.CodecJSON,
			Effect: remoteworker.EffectQuery, RequiredCapabilities: []string{},
		}},
	}))
	result, err := gateway.Invoke(ctx, remoteworker.InvokeRequest{
		RequestID: "go-example-request", InvocationID: "go-example-invocation",
		HandlerID: "example.get-account", DelegatedContext: "example-valid-delegation",
		Deadline: time.Now().Add(5 * time.Second), Input: json.RawMessage(`{"id":"acct-go"}`),
	})
	check(err)
	fmt.Println(string(result.Data))
	_ = workerInput.Close()
	check(command.Wait())
}

func validJSON(_ string, value json.RawMessage) error {
	if !json.Valid(value) {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
