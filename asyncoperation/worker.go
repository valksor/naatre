package asyncoperation

import (
	"context"
	"errors"
	"time"

	naatreruntime "github.com/valksor/naatre/runtime"
)

// LeaseStore is the concrete ownership capability required by WorkerAdapter
// and Dispatcher in addition to runtime.AsyncOperationStore.
type LeaseStore interface {
	naatreruntime.AsyncOperationStore
	CurrentLease(context.Context, string) (Lease, error)
	RenewLease(context.Context, Lease) (Lease, error)
	ExpireLeases(context.Context, time.Time, int) (int, error)
}

// WorkerAdapterConfig configures bounded lease renewal around one application
// worker invocation.
type WorkerAdapterConfig struct {
	Store             LeaseStore
	Worker            naatreruntime.AsyncOperationWorker
	HeartbeatInterval time.Duration
}

// WorkerAdapter adds durable lease renewal to an ordinary runtime worker. It
// owns no process-wide goroutine: renewal exists only while Execute is active.
type WorkerAdapter struct {
	config WorkerAdapterConfig
}

func NewWorkerAdapter(config WorkerAdapterConfig) (*WorkerAdapter, error) {
	if config.Store == nil || config.Worker == nil || config.HeartbeatInterval <= 0 {
		return nil, adapterError(CodeInvalidConfig, "asynchronous worker adapter configuration is invalid", nil)
	}
	return &WorkerAdapter{config: config}, nil
}

func (a *WorkerAdapter) Execute(ctx context.Context, work naatreruntime.AsyncOperationWork, control naatreruntime.AsyncOperationControl) naatreruntime.AsyncOperationCompletion {
	lease, err := a.config.Store.CurrentLease(ctx, work.HandleID)
	if err != nil {
		return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationIndeterminate}
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	failure := make(chan error, 1)
	go a.renew(workerCtx, cancel, lease, failure, done)
	completion := a.config.Worker.Execute(workerCtx, work, control)
	cancel()
	<-done
	select {
	case <-failure:
		return naatreruntime.AsyncOperationCompletion{State: naatreruntime.AsyncOperationIndeterminate}
	default:
		return completion
	}
}

func (a *WorkerAdapter) renew(ctx context.Context, cancel context.CancelFunc, lease Lease, failure chan<- error, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(a.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, err := a.config.Store.RenewLease(ctx, lease)
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
					failure <- err
				}
				cancel()
				return
			}
			lease = renewed
		}
	}
}

// Dispatcher performs one bounded recovery pass. Expired claimed work is
// fenced to indeterminate before durable pending work is claimed.
type Dispatcher struct {
	Coordinator *naatreruntime.AsyncOperationCoordinator
	Store       LeaseStore
	Worker      naatreruntime.AsyncOperationWorker
	Now         func() time.Time
	MaxBatch    int
}

// RunOnce recovers one finite page and returns the authoritative resulting
// handles. Applications own scheduling, shutdown, and retry cadence.
func (d Dispatcher) RunOnce(ctx context.Context) ([]naatreruntime.AsyncOperationHandle, error) {
	if d.Coordinator == nil || d.Store == nil || d.Worker == nil || d.Now == nil || d.MaxBatch <= 0 {
		return nil, adapterError(CodeInvalidConfig, "asynchronous dispatcher configuration is invalid", nil)
	}
	if _, err := d.Store.ExpireLeases(ctx, d.Now().UTC(), d.MaxBatch); err != nil {
		return nil, err
	}
	return d.Coordinator.Recover(ctx, d.MaxBatch, d.Worker)
}

// HintPublisher is an optional queue/broker hint. Durable pending storage,
// rather than the hint, remains the scheduling source of truth.
type HintPublisher interface {
	PublishAsyncOperation(context.Context, string) error
}

type HintPublisherFunc func(context.Context, string) error

func (f HintPublisherFunc) PublishAsyncOperation(ctx context.Context, id string) error {
	return f(ctx, id)
}

// SubmissionAdapter publishes an optional dispatch hint only after durable
// acceptance. Hint failure never rewrites a committed acceptance into failure;
// the returned boolean reports whether the hint was delivered.
type SubmissionAdapter struct {
	Coordinator *naatreruntime.AsyncOperationCoordinator
	Publisher   HintPublisher
}

func (a SubmissionAdapter) Submit(ctx context.Context, submission naatreruntime.AsyncOperationSubmission) (naatreruntime.AsyncOperationAcceptance, bool, error) {
	if a.Coordinator == nil {
		return naatreruntime.AsyncOperationAcceptance{}, false, adapterError(CodeInvalidConfig, "asynchronous submission adapter configuration is invalid", nil)
	}
	accepted, err := a.Coordinator.Submit(ctx, submission)
	if err != nil {
		return naatreruntime.AsyncOperationAcceptance{}, false, err
	}
	if a.Publisher == nil || !accepted.Created {
		return accepted, false, nil
	}
	return accepted, publishHint(ctx, a.Publisher, accepted.Handle.ID), nil
}

func publishHint(ctx context.Context, publisher HintPublisher, id string) bool {
	return publisher.PublishAsyncOperation(ctx, id) == nil
}
