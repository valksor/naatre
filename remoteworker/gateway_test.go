package remoteworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReferenceGatewayExecutesFixtureOperation(t *testing.T) {
	t.Parallel()
	transport := &fixtureTransport{}
	gateway := newTestGateway(t, transport, 2)
	registerFixtureWorker(t, gateway)

	result, err := gateway.Invoke(context.Background(), InvokeRequest{
		RequestID: "request-1", InvocationID: "invocation-1", HandlerID: "fixture.greet",
		DelegatedContext: "valid-delegation", Input: json.RawMessage(`{"name":"Ada"}`),
		Deadline: time.Date(2026, 9, 15, 12, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(result.Data) != `{"greeting":"Hello, Ada"}` || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if transport.invocations.Load() != 1 {
		t.Fatalf("worker calls = %d, want 1", transport.invocations.Load())
	}
}

func TestFramedIndependentFixtureWorkerProducesEquivalentPublicDataAndErrors(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the independent remote-worker fixture")
	}
	for _, language := range []string{"plain-javascript", "typescript"} {
		t.Run(language, func(t *testing.T) {
			testFramedIndependentFixtureWorker(t, node, language)
		})
	}
}

func testFramedIndependentFixtureWorker(t *testing.T, node, language string) {
	t.Helper()
	command := exec.Command(node, filepath.Join("..", "conformance", "independent", "remote-worker.mjs"), "--serve")
	command.Env = append(os.Environ(), "NAATRE_HANDLER_LANGUAGE="+language)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if waitErr := command.Wait(); waitErr != nil {
			t.Errorf("independent worker: %v: %s", waitErr, stderr.String())
		}
	})
	transport, err := NewFramedTransport(stdout, stdin, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	gateway := newTestGateway(t, transport, 1)
	registration := fixtureRegistration()
	registration.Handlers = registration.Handlers[:1]
	registration.Handlers[0].RequiredCapabilities = []string{}
	if err := gateway.Register(context.Background(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}

	fixtureBytes, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "remote-workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Operations []struct {
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Expected struct {
				Data   json.RawMessage `json:"data"`
				Errors []WorkerError   `json:"errors"`
			} `json:"expected"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	tests := fixture.Operations
	for index, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			request := fixtureRequest("fixture.greet")
			request.RequestID = fmt.Sprintf("request-%d", index+10)
			request.InvocationID = fmt.Sprintf("invocation-%d", index+10)
			request.Input = test.Input
			result, invokeErr := gateway.Invoke(context.Background(), request)
			if invokeErr != nil {
				t.Fatalf("Invoke: %v", invokeErr)
			}
			if !jsonEqual(result.Data, test.Expected.Data) || !equalWorkerErrors(result.Errors, test.Expected.Errors) {
				t.Fatalf("public result = %#v, want data %s errors %#v", result, test.Expected.Data, test.Expected.Errors)
			}
		})
	}
}

func equalWorkerErrors(left, right []WorkerError) bool {
	return len(left) == len(right) && (len(left) == 0 || reflect.DeepEqual(left, right))
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}

func TestReferenceGatewayRejectsBeforeDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		registration Registration
		request      InvokeRequest
		wantCode     string
	}{
		{name: "wrong schema", registration: fixtureRegistration(), wantCode: CodeSchemaMismatch},
		{name: "forged worker identity", registration: fixtureRegistration(), wantCode: CodeUnauthenticated},
		{name: "forged delegated identity", request: fixtureRequest("fixture.greet"), wantCode: CodeUnauthorized},
		{name: "unknown handler", request: fixtureRequest("fixture.unknown"), wantCode: CodeUnknownHandler},
		{name: "unsupported transaction", registration: fixtureRegistration(), wantCode: CodeCapabilityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &fixtureTransport{}
			gateway := newTestGateway(t, transport, 1)
			if test.registration.Protocol != "" {
				registration := test.registration
				switch test.name {
				case "wrong schema":
					registration.SchemaRevision = "schema-wrong"
				case "forged worker identity":
					registration.ServiceIdentity = "spiffe://evil/worker"
				case "unsupported transaction":
					registration.Handlers = append(registration.Handlers, Handler{ID: "fixture.transaction", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: CodecJSON, Effect: EffectTransaction})
				}
				err := gateway.Register(context.Background(), registration)
				assertGatewayCode(t, err, test.wantCode)
			} else {
				registerFixtureWorker(t, gateway)
				request := test.request
				if test.name == "forged delegated identity" {
					request.DelegatedContext = "forged"
				}
				_, err := gateway.Invoke(context.Background(), request)
				assertGatewayCode(t, err, test.wantCode)
			}
			if transport.invocations.Load() != 0 {
				t.Fatalf("worker calls = %d, want 0", transport.invocations.Load())
			}
		})
	}
}

func TestReferenceGatewayRejectsMalformedWorkerOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result WorkerResult
		want   string
	}{
		{name: "malformed data", result: fixtureResult(json.RawMessage(`{"greeting":`)), want: CodeMalformedWorkerData},
		{name: "wrong schema", result: func() WorkerResult {
			value := fixtureResult(json.RawMessage(`{"greeting":"Hello"}`))
			value.SchemaRevision = "schema-old"
			return value
		}(), want: CodeSchemaMismatch},
		{name: "wrong output shape", result: fixtureResult(json.RawMessage(`{"message":"Hello"}`)), want: CodeOutputInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &fixtureTransport{result: test.result}
			gateway := newTestGateway(t, transport, 1)
			registerFixtureWorker(t, gateway)
			_, err := gateway.Invoke(context.Background(), fixtureRequest("fixture.greet"))
			assertGatewayCode(t, err, test.want)
		})
	}
}

func TestReferenceGatewayReconnectDecisions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		handler   string
		request   InvokeRequest
		failure   DeliveryPhase
		wantCalls int64
		wantCode  string
	}{
		{name: "query before write", handler: "fixture.greet", request: fixtureRequest("fixture.greet"), failure: DeliveryBeforeWrite, wantCalls: 2},
		{name: "query after write", handler: "fixture.greet", request: fixtureRequest("fixture.greet"), failure: DeliveryAfterWrite, wantCalls: 2},
		{name: "mutation before write", handler: "fixture.mutate", request: fixtureRequest("fixture.mutate"), failure: DeliveryBeforeWrite, wantCalls: 2},
		{name: "mutation after write without evidence", handler: "fixture.mutate", request: fixtureRequest("fixture.mutate"), failure: DeliveryAfterWrite, wantCalls: 1, wantCode: CodeOutcomeIndeterminate},
		{name: "mutation after write with evidence", handler: "fixture.mutate", request: func() InvokeRequest {
			value := fixtureRequest("fixture.mutate")
			value.IdempotencyKey = "caller-key"
			return value
		}(), failure: DeliveryAfterWrite, wantCalls: 2},
		{name: "transaction after write", handler: "fixture.transaction", request: fixtureRequest("fixture.transaction"), failure: DeliveryAfterWrite, wantCalls: 1, wantCode: CodeOutcomeIndeterminate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &fixtureTransport{failures: []error{&DeliveryError{Phase: test.failure, Cause: errors.New("worker process died")}}}
			gateway := newTestGateway(t, transport, 2)
			registration := fixtureRegistration()
			if test.name == "mutation after write with evidence" {
				registration.Capabilities = append(registration.Capabilities, CapabilityIdempotencyReplay)
				for index := range registration.Handlers {
					if registration.Handlers[index].ID == "fixture.mutate" {
						registration.Handlers[index].RequiredCapabilities = []string{CapabilityIdempotencyReplay}
					}
				}
			}
			if test.handler == "fixture.transaction" {
				registration.Capabilities = append(registration.Capabilities, CapabilityTransactions)
				registration.Handlers = append(registration.Handlers, Handler{ID: "fixture.transaction", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: CodecJSON, Effect: EffectTransaction, RequiredCapabilities: []string{CapabilityTransactions}})
			}
			if err := gateway.Register(context.Background(), registration); err != nil {
				t.Fatal(err)
			}
			_, err := gateway.Invoke(context.Background(), test.request)
			if test.wantCode == "" && err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if test.wantCode != "" {
				assertGatewayCode(t, err, test.wantCode)
			}
			if transport.invocations.Load() != test.wantCalls {
				t.Fatalf("worker calls = %d, want %d", transport.invocations.Load(), test.wantCalls)
			}
		})
	}
}

func TestReferenceGatewayRejectsDuplicateInvocation(t *testing.T) {
	t.Parallel()
	transport := &fixtureTransport{}
	gateway := newTestGateway(t, transport, 1)
	registerFixtureWorker(t, gateway)
	request := fixtureRequest("fixture.greet")
	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	_, err := gateway.Invoke(context.Background(), request)
	assertGatewayCode(t, err, CodeDuplicateInvocation)
	if transport.invocations.Load() != 1 {
		t.Fatalf("worker calls = %d, want 1", transport.invocations.Load())
	}
}

func TestReferenceGatewayCancellationAcknowledgement(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	transport := &fixtureTransport{started: started, release: release}
	gateway := newTestGateway(t, transport, 1)
	registerFixtureWorker(t, gateway)

	done := make(chan error, 1)
	go func() {
		_, err := gateway.Invoke(context.Background(), fixtureRequest("fixture.greet"))
		done <- err
	}()
	<-started
	ack, err := gateway.Cancel(context.Background(), CancelRequest{RequestID: "request-1", InvocationID: "invocation-1"})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if ack.Disposition != CancellationAcknowledged || ack.InvocationID != "invocation-1" {
		t.Fatalf("ack = %#v", ack)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

func TestReferenceGatewayWorkerOverload(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	transport := &fixtureTransport{started: started, release: release}
	gateway := newTestGateway(t, transport, 1)
	registerFixtureWorker(t, gateway)

	done := make(chan error, 1)
	go func() {
		_, err := gateway.Invoke(context.Background(), fixtureRequest("fixture.greet"))
		done <- err
	}()
	<-started
	second := fixtureRequest("fixture.greet")
	second.RequestID, second.InvocationID = "request-2", "invocation-2"
	_, err := gateway.Invoke(context.Background(), second)
	assertGatewayCode(t, err, CodeOverloaded)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if transport.invocations.Load() != 1 {
		t.Fatalf("worker calls = %d, want 1", transport.invocations.Load())
	}
}

func TestReferenceGatewayRejectsStaleReference(t *testing.T) {
	t.Parallel()
	transport := &fixtureTransport{}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	gateway := newTestGatewayWithClock(t, transport, 1, func() time.Time { return now })
	registerFixtureWorker(t, gateway)
	gateway.rememberReferences("invocation-owner", []ReferenceGrant{{ID: "ref-1", ExpiresAt: now.Add(-time.Second), Lifetime: ReferenceInvocation}})
	request := fixtureRequest("fixture.greet")
	request.Parent = Parent{Reference: "ref-1", OwnerInvocationID: "invocation-owner"}
	_, err := gateway.Invoke(context.Background(), request)
	assertGatewayCode(t, err, CodeStaleReference)
	if transport.invocations.Load() != 0 {
		t.Fatalf("worker calls = %d, want 0", transport.invocations.Load())
	}
}

func TestCreditWindowEnforcesBackpressure(t *testing.T) {
	t.Parallel()
	window := NewCreditWindow(2, 8)
	if err := window.Consume(4); err != nil {
		t.Fatal(err)
	}
	if err := window.Consume(4); err != nil {
		t.Fatal(err)
	}
	if err := window.Consume(1); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("Consume beyond credit = %v", err)
	}
	if err := window.Grant(1, 3); err != nil {
		t.Fatal(err)
	}
	if err := window.Consume(3); err != nil {
		t.Fatal(err)
	}
}

type fixtureTransport struct {
	mu          sync.Mutex
	failures    []error
	result      WorkerResult
	started     chan struct{}
	release     chan struct{}
	invocations atomic.Int64
}

func (f *fixtureTransport) Register(_ context.Context, registration Registration) (RegistrationAck, error) {
	return RegistrationAck{Protocol: ProtocolVersion, WorkerID: registration.WorkerID, SessionID: "session-1", SchemaRevision: registration.SchemaRevision, AcceptedCapabilities: registration.Capabilities}, nil
}

func (f *fixtureTransport) Invoke(_ context.Context, invocation WorkerInvocation) (WorkerResult, error) {
	f.invocations.Add(1)
	if f.started != nil {
		select {
		case <-f.started:
		default:
			close(f.started)
		}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.failures) != 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return WorkerResult{}, err
	}
	if f.result.Protocol != "" {
		result := f.result
		result.InvocationID, result.AttemptID = invocation.InvocationID, invocation.AttemptID
		return result, nil
	}
	if invocation.HandlerID == "fixture.error" {
		return WorkerResult{Protocol: ProtocolVersion, InvocationID: invocation.InvocationID, AttemptID: invocation.AttemptID, SchemaRevision: "schema-1", Errors: []WorkerError{{Code: "NAME_REJECTED", Message: "name was rejected"}}}, nil
	}
	return fixtureResult(json.RawMessage(`{"greeting":"Hello, Ada"}`)).forInvocation(invocation), nil
}

func (f *fixtureTransport) Cancel(_ context.Context, request CancelRequest) (CancellationAck, error) {
	return CancellationAck{Protocol: ProtocolVersion, InvocationID: request.InvocationID, Disposition: CancellationAcknowledged}, nil
}

func fixtureRegistration() Registration {
	return Registration{
		Protocol: ProtocolVersion, WorkerID: "fixture-worker", ServiceIdentity: "spiffe://example/fixture-worker",
		Audience: "naatre-gateway", Endpoint: "fixture-stdio", SchemaRevision: "schema-1",
		SchemaDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Capabilities: []string{CapabilityUnary, CapabilityCancellationAck},
		Limits:       WorkerLimits{MaxInFlight: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096, MaxStreamFrames: 4, MaxStreamBytes: 16384},
		Handlers: []Handler{
			{ID: "fixture.greet", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: CodecJSON, Effect: EffectQuery},
			{ID: "fixture.error", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: CodecJSON, Effect: EffectQuery},
			{ID: "fixture.mutate", InputSchema: "GreetInput", OutputSchema: "GreetOutput", Codec: CodecJSON, Effect: EffectMutation},
		},
	}
}

func fixtureRequest(handler string) InvokeRequest {
	return InvokeRequest{RequestID: "request-1", InvocationID: "invocation-1", HandlerID: handler, DelegatedContext: "valid-delegation", Input: json.RawMessage(`{"name":"Ada"}`), Deadline: time.Date(2026, 9, 15, 12, 1, 0, 0, time.UTC)}
}

func fixtureResult(data json.RawMessage) WorkerResult {
	return WorkerResult{Protocol: ProtocolVersion, SchemaRevision: "schema-1", Data: data}
}

func (r WorkerResult) forInvocation(invocation WorkerInvocation) WorkerResult {
	r.InvocationID, r.AttemptID = invocation.InvocationID, invocation.AttemptID
	return r
}

func newTestGateway(t *testing.T, transport Transport, attempts int) *ReferenceGateway {
	t.Helper()
	return newTestGatewayWithClock(t, transport, attempts, func() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) })
}

func newTestGatewayWithClock(t *testing.T, transport Transport, attempts int, now func() time.Time) *ReferenceGateway {
	t.Helper()
	gateway, err := NewReferenceGateway(GatewayConfig{
		Transport: transport, Endpoint: "fixture-stdio", WorkerID: "fixture-worker",
		ServiceIdentity: "spiffe://example/fixture-worker", Audience: "naatre-gateway",
		SchemaRevision: "schema-1", SchemaDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MaxInFlight: 1, MaxAttempts: attempts, MaxRequestBytes: 4096, MaxResponseBytes: 4096, Now: now,
		VerifyDelegation: func(_ context.Context, token string, _ DelegationExpectation) error {
			if token != "valid-delegation" {
				return errors.New("invalid delegation")
			}
			return nil
		},
		Authorize: func(context.Context, AuthorizationRequest) error { return nil },
		ValidateInput: func(_ string, input json.RawMessage) error {
			var value struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(input, &value) != nil || value.Name == "" {
				return errors.New("invalid input")
			}
			return nil
		},
		ValidateOutput: func(_ string, output json.RawMessage) error {
			var value struct {
				Greeting string `json:"greeting"`
			}
			if json.Unmarshal(output, &value) != nil || value.Greeting == "" {
				return errors.New("invalid output")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewReferenceGateway: %v", err)
	}
	return gateway
}

func registerFixtureWorker(t *testing.T, gateway *ReferenceGateway) {
	t.Helper()
	if err := gateway.Register(context.Background(), fixtureRegistration()); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func assertGatewayCode(t *testing.T, err error, code string) {
	t.Helper()
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != code {
		t.Fatalf("error = %v, want gateway code %s", err, code)
	}
}
