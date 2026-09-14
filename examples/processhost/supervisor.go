package processhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/valksor/naatre/runtime"
)

// SupervisorResult is the public, cardinality-safe outcome of one host run.
// Detailed server errors are returned separately for private operator logging.
type SupervisorResult struct {
	ProcessOutcome runtime.DrainOutcome `json:"processOutcome"`
	Forced         bool                 `json:"forced"`
	Code           string               `json:"code,omitempty"`
}

// Supervisor binds an HTTP server lifecycle to a ProcessController. The
// caller supplies a cancellation context derived from its service manager or
// signal policy; the supervisor does not claim ownership of either policy.
type Supervisor struct {
	process        *runtime.ProcessController
	server         *http.Server
	cleanupTimeout time.Duration
}

// NewSupervisor validates a bounded HTTP supervisor integration.
func NewSupervisor(process *runtime.ProcessController, server *http.Server, cleanupTimeout time.Duration) (*Supervisor, error) {
	if process == nil {
		return nil, errors.New("process controller is nil")
	}
	if server == nil {
		return nil, errors.New("HTTP server is nil")
	}
	if server.Handler == nil {
		return nil, errors.New("HTTP server handler is nil")
	}
	if cleanupTimeout <= 0 {
		return nil, errors.New("HTTP server cleanup timeout must be positive")
	}
	return &Supervisor{process: process, server: server, cleanupTimeout: cleanupTimeout}, nil
}

// Serve runs until the server fails or the supervisor context is cancelled.
// Cancellation closes process admission and the listener concurrently, drains
// accepted work, and force-closes HTTP connections when either bounded phase
// cannot finish. Any returned error is private and must not be serialized to a
// client; SupervisorResult contains the complete stable public failure shape.
func (s *Supervisor) Serve(ctx context.Context, listener net.Listener) (SupervisorResult, error) {
	if ctx == nil {
		return SupervisorResult{}, errors.New("supervisor context is nil")
	}
	if listener == nil {
		return SupervisorResult{}, errors.New("HTTP listener is nil")
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.server.Serve(listener)
	}()

	select {
	case serveErr := <-serveDone:
		outcome := s.process.Drain(context.WithoutCancel(ctx))
		result := SupervisorResult{ProcessOutcome: outcome, Forced: outcome == runtime.DrainForced, Code: runtime.CodeInternal}
		if result.Forced {
			result.Code = runtime.CodeResourceExhausted
		}
		return result, fmt.Errorf("serve process host: %w", serveErr)
	case <-ctx.Done():
		return s.shutdown(context.WithoutCancel(ctx), serveDone)
	}
}

func (s *Supervisor) shutdown(ctx context.Context, serveDone <-chan error) (SupervisorResult, error) {
	drainDone := make(chan runtime.DrainOutcome, 1)
	shutdownDone := make(chan error, 1)
	cleanupCtx, cancelCleanup := context.WithTimeout(ctx, s.cleanupTimeout)
	defer cancelCleanup()

	go func() {
		drainDone <- s.process.Drain(ctx)
	}()
	go func() {
		shutdownDone <- s.server.Shutdown(cleanupCtx)
	}()

	outcome := <-drainDone
	shutdownErr := <-shutdownDone
	forced := outcome == runtime.DrainForced || shutdownErr != nil
	var closeErr error
	if forced {
		closeErr = s.server.Close()
	}
	serveErr := <-serveDone
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if errors.Is(closeErr, http.ErrServerClosed) {
		closeErr = nil
	}

	result := SupervisorResult{ProcessOutcome: outcome, Forced: forced}
	if forced {
		result.Code = runtime.CodeResourceExhausted
	}
	return result, errors.Join(shutdownErr, closeErr, serveErr)
}
