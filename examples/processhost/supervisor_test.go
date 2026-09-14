package processhost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
)

func TestSupervisorCompletesRollingRestartOnEphemeralListener(t *testing.T) {
	t.Parallel()
	controller := startedExampleController(t, runtime.DefaultProcessConfig())
	handler, err := NewHandler(controller, fixedAdmission, func(context.Context, runtime.AdmittedWork, http.ResponseWriter, *http.Request) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	supervisor, err := NewSupervisor(controller, server, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan supervisorRun, 1)
	go func() {
		outcome, serveErr := supervisor.Serve(ctx, listener)
		result <- supervisorRun{result: outcome, err: serveErr}
	}()

	response, err := pipeClient(listener).Get("http://process.invalid/health/ready")
	if err != nil {
		t.Fatalf("read ready endpoint: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("ready status = %d", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close ready response: %v", err)
	}
	cancel()
	run := receiveSupervisorRun(t, result)
	if run.err != nil || run.result.ProcessOutcome != runtime.DrainCompleted || run.result.Forced || run.result.Code != "" {
		t.Fatalf("supervisor run = %#v, %v", run.result, run.err)
	}
	if health := controller.Health(); health.State != runtime.ProcessStopped || health.Ready || health.Live {
		t.Fatalf("stopped health = %#v", health)
	}
}

func TestSupervisorForceClosesUncooperativeHTTPWork(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller := startedExampleController(t, config)
	entered := make(chan struct{})
	release := make(chan struct{})
	handler, err := NewHandler(controller, fixedAdmission, func(context.Context, runtime.AdmittedWork, http.ResponseWriter, *http.Request) error {
		close(entered)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	supervisor, err := NewSupervisor(controller, server, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan supervisorRun, 1)
	go func() {
		outcome, serveErr := supervisor.Serve(ctx, listener)
		result <- supervisorRun{result: outcome, err: serveErr}
	}()
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := pipeClient(listener).Post("http://process.invalid/work", "application/json", strings.NewReader("{}"))
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("business callback did not start")
	}
	cancel()
	run := receiveSupervisorRun(t, result)
	if run.result.ProcessOutcome != runtime.DrainForced || !run.result.Forced || run.result.Code != runtime.CodeResourceExhausted {
		t.Fatalf("forced supervisor result = %#v", run.result)
	}
	if health := controller.Health(); health.State != runtime.ProcessForced || health.Ready || health.Live {
		t.Fatalf("forced health = %#v", health)
	}
	encoded, err := json.Marshal(run.result)
	if err != nil {
		t.Fatal(err)
	}
	for _, protected := range []string{"tenant", "principal", "r1", "schema-r1", "configuration-r1"} {
		if strings.Contains(string(encoded), protected) {
			t.Fatalf("forced result leaked %q: %s", protected, encoded)
		}
	}
	close(release)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("forced request did not exit")
	}
}

func TestSupervisorUnexpectedServeFailureUsesStablePublicCode(t *testing.T) {
	t.Parallel()
	controller := startedExampleController(t, runtime.DefaultProcessConfig())
	server := &http.Server{Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second}
	supervisor, err := NewSupervisor(controller, server, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	result, serveErr := supervisor.Serve(context.Background(), listener)
	if serveErr == nil || result.Code != runtime.CodeInternal || result.ProcessOutcome != runtime.DrainCompleted {
		t.Fatalf("unexpected serve result = %#v, %v", result, serveErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), listener.Addr().String()) || strings.Contains(string(encoded), "closed network connection") {
		t.Fatalf("public result exposed implementation detail: %s", encoded)
	}
}

func TestSupervisorServeFailurePreservesForcedDrainOutcome(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.MaxDrainDuration = 10 * time.Millisecond
	controller := startedExampleController(t, config)
	lease, err := controller.Admit(context.Background(), runtime.AdmissionRequest{
		TenantReference: "protected-tenant", PrincipalReference: "protected-principal", Kind: runtime.WorkDurable,
		ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	server := &http.Server{Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second}
	supervisor, err := NewSupervisor(controller, server, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	result, serveErr := supervisor.Serve(context.Background(), listener)
	if serveErr == nil || result.ProcessOutcome != runtime.DrainForced || !result.Forced || result.Code != runtime.CodeResourceExhausted {
		t.Fatalf("forced serve result = %#v, %v", result, serveErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "protected") || strings.Contains(string(encoded), listener.Addr().String()) {
		t.Fatalf("forced serve result exposed protected detail: %s", encoded)
	}
}

func TestNewSupervisorRejectsInvalidBoundaries(t *testing.T) {
	t.Parallel()
	controller := startedExampleController(t, runtime.DefaultProcessConfig())
	server := &http.Server{Handler: http.NewServeMux()}
	tests := []struct {
		name    string
		process *runtime.ProcessController
		server  *http.Server
		timeout time.Duration
	}{
		{name: "nil process", server: server, timeout: time.Second},
		{name: "nil server", process: controller, timeout: time.Second},
		{name: "nil handler", process: controller, server: new(http.Server), timeout: time.Second},
		{name: "zero cleanup timeout", process: controller, server: server},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewSupervisor(test.process, test.server, test.timeout); err == nil {
				t.Fatal("NewSupervisor accepted invalid boundary")
			}
		})
	}
}

type supervisorRun struct {
	result SupervisorResult
	err    error
}

func receiveSupervisorRun(t testing.TB, result <-chan supervisorRun) supervisorRun {
	t.Helper()
	select {
	case run := <-result:
		return run
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not finish")
		return supervisorRun{}
	}
}

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return pipeAddress{}
}

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.connections <- server:
		return client, nil
	case <-l.closed:
		_ = server.Close()
		_ = client.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = server.Close()
		_ = client.Close()
		return nil, ctx.Err()
	}
}

type pipeAddress struct{}

func (pipeAddress) Network() string { return "pipe" }
func (pipeAddress) String() string  { return "in-memory" }

func pipeClient(listener *pipeListener) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := listener.dial(ctx)
			if err != nil && !errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			return connection, err
		},
	}
	return &http.Client{Transport: transport, Timeout: time.Second}
}
