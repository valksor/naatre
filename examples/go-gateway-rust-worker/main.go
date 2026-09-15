// Command go-gateway-rust-worker demonstrates the bounded conformance-only
// stdio path between the Go reference gateway and the Rust worker example.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/valksor/naatre/remoteworker"
)

const maximumFrameBytes = 4096

type rustTransport struct {
	mu     sync.Mutex
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func startRustWorker(ctx context.Context, executable string) (*rustTransport, error) {
	cmd := exec.CommandContext(ctx, executable)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &rustTransport{stdin: stdin, stdout: stdout, cmd: cmd}, nil
}

func (t *rustTransport) Register(_ context.Context, registration remoteworker.Registration) (remoteworker.RegistrationAck, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ack remoteworker.RegistrationAck
	if err := writeFrame(t.stdin, registration); err != nil {
		return ack, err
	}
	return ack, readFrame(t.stdout, &ack)
}

func (t *rustTransport) Invoke(_ context.Context, invocation remoteworker.WorkerInvocation) (remoteworker.WorkerResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var result remoteworker.WorkerResult
	if err := writeFrame(t.stdin, invocation); err != nil {
		return result, err
	}
	return result, readFrame(t.stdout, &result)
}

func (*rustTransport) Cancel(_ context.Context, request remoteworker.CancelRequest) (remoteworker.CancellationAck, error) {
	if request.Protocol != remoteworker.ProtocolVersion || request.InvocationID == "" {
		return remoteworker.CancellationAck{}, errors.New("invalid example cancellation request")
	}
	return remoteworker.CancellationAck{
		Protocol:     remoteworker.ProtocolVersion,
		InvocationID: request.InvocationID,
		Disposition:  remoteworker.CancellationUnsupported,
	}, nil
}

func (t *rustTransport) close() error {
	closeErr := t.stdin.Close()
	waitErr := t.cmd.Wait()
	return errors.Join(closeErr, waitErr)
}

func writeFrame(output io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > maximumFrameBytes {
		return errors.New("worker frame exceeds example limit")
	}
	header := [5]byte{}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := output.Write(header[:]); err != nil {
		return err
	}
	_, err = output.Write(payload)
	return err
}

func readFrame(input io.Reader, target any) error {
	header := [5]byte{}
	if _, err := io.ReadFull(input, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if header[0] != 0 || size == 0 || size > maximumFrameBytes {
		return errors.New("worker returned an invalid frame")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(input, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}

func run(ctx context.Context, workerExecutable string) error {
	fixtureBytes, err := os.ReadFile("conformance/v1/remote-workers.json")
	if err != nil {
		return err
	}
	var fixture struct {
		Registration remoteworker.Registration `json:"registration"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		return err
	}
	transport, err := startRustWorker(ctx, workerExecutable)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := transport.close(); closeErr != nil {
			fmt.Fprintln(os.Stderr, "worker cleanup:", closeErr)
		}
	}()

	gateway, err := remoteworker.NewReferenceGateway(remoteworker.GatewayConfig{
		Transport: transport, Endpoint: "fixture-stdio", WorkerID: "fixture-worker",
		ServiceIdentity: "spiffe://example/fixture-worker", Audience: "naatre-gateway",
		SchemaRevision: "schema-1", SchemaDigest: fixture.Registration.SchemaDigest,
		MaxInFlight: 1, MaxAttempts: 1, MaxRequestBytes: maximumFrameBytes, MaxResponseBytes: maximumFrameBytes,
		MaxStreamFrames: 4, MaxStreamBytes: 16384, MaxReferences: 16, MaxSeen: 32,
		VerifyDelegation: func(context.Context, string, remoteworker.DelegationExpectation) error { return nil },
		Authorize:        func(context.Context, remoteworker.AuthorizationRequest) error { return nil },
		ValidateInput:    func(string, json.RawMessage) error { return nil },
		ValidateOutput:   func(string, json.RawMessage) error { return nil },
	})
	if err != nil {
		return err
	}
	if err := gateway.Register(ctx, fixture.Registration); err != nil {
		return err
	}
	result, err := gateway.Invoke(ctx, remoteworker.InvokeRequest{
		RequestID: "example-request", InvocationID: "example-invocation", HandlerID: "fixture.greet",
		DelegatedContext: "valid-delegation", Deadline: time.Now().Add(time.Minute), Input: json.RawMessage(`{"name":"Ada"}`),
	})
	if err != nil {
		return err
	}
	fmt.Println(string(result.Data))
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go-gateway-rust-worker RUST_WORKER_EXECUTABLE")
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
