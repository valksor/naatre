package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/remoteworker"
)

func TestPHPServerHandlersRunSharedGatewayFixture(t *testing.T) {
	fixture := loadPHPRemoteWorkerFixture(t)
	for _, adapter := range []string{"neutral", "symfony", "laravel"} {
		t.Run(adapter, func(t *testing.T) {
			worker := startPHPWorker(t, "--adapter="+adapter)
			t.Cleanup(func() { worker.stop(t, true) })
			gateway := newPHPGateway(t, worker.transport, 1, phpGreetRegistration())

			result, err := gateway.Invoke(context.Background(), phpInvocation("request-1", "invocation-1", fixture.Operations[0].HandlerID, fixture.Operations[0].Input))
			if err != nil {
				t.Fatalf("Invoke success fixture: %v; stderr=%s", err, worker.stderr.String())
			}
			if !equalStreamingJSON(result.Data, fixture.Operations[0].Expected.Data) || len(result.Errors) != 0 {
				t.Fatalf("success result = %s, %#v", result.Data, result.Errors)
			}

			rejected, err := gateway.Invoke(context.Background(), phpInvocation("request-2", "invocation-2", fixture.Operations[1].HandlerID, fixture.Operations[1].Input))
			if err != nil {
				t.Fatalf("Invoke application error fixture: %v; stderr=%s", err, worker.stderr.String())
			}
			if string(rejected.Data) != "null" || len(rejected.Errors) != len(fixture.Operations[1].Expected.Errors) ||
				rejected.Errors[0].Code != fixture.Operations[1].Expected.Errors[0].Code ||
				rejected.Errors[0].Message != fixture.Operations[1].Expected.Errors[0].Message || rejected.Errors[0].Retryable {
				t.Fatalf("application error result = %s, %#v", rejected.Data, rejected.Errors)
			}
		})
	}
}

func TestPHPWorkerTerminationMakesMutationOutcomeIndeterminate(t *testing.T) {
	worker := startPHPWorker(t, "--adapter=neutral", "--terminate-mutation")
	counted := &countingPHPTransport{Transport: worker.transport}
	gateway := newPHPGateway(t, counted, 2, phpMutationRegistration())
	request := phpInvocation("request-mutation", "invocation-mutation", "fixture.mutate", json.RawMessage(`{"name":"Ada"}`))

	_, err := gateway.Invoke(context.Background(), request)
	var gatewayError *remoteworker.GatewayError
	if !errors.As(err, &gatewayError) || gatewayError.Code != remoteworker.CodeOutcomeIndeterminate {
		t.Fatalf("mutation termination error = %v; stderr=%s", err, worker.stderr.String())
	}
	if counted.invocations.Load() != 1 {
		t.Fatalf("mutation invocation attempts = %d, want 1", counted.invocations.Load())
	}
	worker.stop(t, false)
}

type phpWorkerProcess struct {
	command   *exec.Cmd
	stdin     io.WriteCloser
	transport *remoteworker.FramedTransport
	stderr    bytes.Buffer
	stopped   bool
}

func startPHPWorker(t *testing.T, arguments ...string) *phpWorkerProcess {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"sdk/php/tests/server-worker.php"}, arguments...)
	command := exec.Command("php", args...)
	command.Dir = root
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	worker := &phpWorkerProcess{command: command, stdin: stdin}
	command.Stderr = &worker.stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start PHP worker: %v", err)
	}
	transport, err := remoteworker.NewFramedTransport(stdout, stdin, 65_536)
	if err != nil {
		t.Fatal(err)
	}
	worker.transport = transport
	return worker
}

func (p *phpWorkerProcess) stop(t *testing.T, requireSuccess bool) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.stdin.Close()
	err := p.command.Wait()
	if requireSuccess && err != nil {
		t.Errorf("PHP worker shutdown: %v; stderr=%s", err, p.stderr.String())
	}
}

type countingPHPTransport struct {
	remoteworker.Transport
	invocations atomic.Int64
}

func (t *countingPHPTransport) Invoke(ctx context.Context, invocation remoteworker.WorkerInvocation) (remoteworker.WorkerResult, error) {
	t.invocations.Add(1)
	return t.Transport.Invoke(ctx, invocation)
}

func newPHPGateway(t *testing.T, transport remoteworker.Transport, attempts int, registration remoteworker.Registration) *remoteworker.ReferenceGateway {
	t.Helper()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	gateway, err := remoteworker.NewReferenceGateway(remoteworker.GatewayConfig{
		Transport: transport, Endpoint: "fixture-stdio", WorkerID: "fixture-worker",
		ServiceIdentity: "spiffe://example/fixture-worker", Audience: "naatre-gateway",
		SchemaRevision: "schema-1", SchemaDigest: strings.Repeat("a", 64),
		MaxInFlight: 1, MaxAttempts: attempts, MaxRequestBytes: 4096, MaxResponseBytes: 4096,
		MaxStreamFrames: 4, MaxStreamBytes: 16384, MaxReferences: 16, MaxSeen: 32,
		Now: func() time.Time { return now },
		VerifyDelegation: func(_ context.Context, token string, _ remoteworker.DelegationExpectation) error {
			if !strings.Contains(token, ":") {
				return errors.New("invalid fixture delegation")
			}
			return nil
		},
		Authorize: func(context.Context, remoteworker.AuthorizationRequest) error { return nil },
		ValidateInput: func(_ string, raw json.RawMessage) error {
			var input struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &input); err != nil || input.Name == "" {
				return errors.New("invalid fixture input")
			}
			return nil
		},
		ValidateOutput: func(_ string, raw json.RawMessage) error {
			var output struct {
				Greeting string `json:"greeting"`
			}
			if err := json.Unmarshal(raw, &output); err != nil || output.Greeting == "" {
				return errors.New("invalid fixture output")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Register(context.Background(), registration); err != nil {
		t.Fatalf("register PHP worker: %v", err)
	}
	return gateway
}

func phpGreetRegistration() remoteworker.Registration {
	return phpRegistration(remoteworker.Handler{ID: "fixture.greet", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: remoteworker.CodecJSON, Effect: remoteworker.EffectQuery, RequiredCapabilities: []string{}})
}

func phpMutationRegistration() remoteworker.Registration {
	return phpRegistration(remoteworker.Handler{ID: "fixture.mutate", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: remoteworker.CodecJSON, Effect: remoteworker.EffectMutation, RequiredCapabilities: []string{}})
}

func phpRegistration(handler remoteworker.Handler) remoteworker.Registration {
	return remoteworker.Registration{
		Protocol: remoteworker.ProtocolVersion, WorkerID: "fixture-worker", ServiceIdentity: "spiffe://example/fixture-worker",
		Audience: "naatre-gateway", Endpoint: "fixture-stdio", SchemaRevision: "schema-1", SchemaDigest: strings.Repeat("a", 64),
		Capabilities: []string{remoteworker.CapabilityUnary, remoteworker.CapabilityCancellationAck},
		Limits:       remoteworker.WorkerLimits{MaxInFlight: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096, MaxStreamFrames: 4, MaxStreamBytes: 16_384},
		Handlers:     []remoteworker.Handler{handler},
	}
}

func phpInvocation(requestID, invocationID, handlerID string, input json.RawMessage) remoteworker.InvokeRequest {
	return remoteworker.InvokeRequest{
		RequestID: requestID, InvocationID: invocationID, HandlerID: handlerID,
		DelegatedContext: "tenant-a:ada", Input: input,
		Deadline: time.Date(2026, 9, 15, 12, 1, 0, 0, time.UTC),
	}
}

func loadPHPRemoteWorkerFixture(t *testing.T) remoteWorkerFixture {
	t.Helper()
	fixture := loadRemoteWorkerFixture(t)
	if len(fixture.Operations) != 2 {
		t.Fatalf("remote worker operation fixtures = %d, want 2", len(fixture.Operations))
	}
	return fixture
}
