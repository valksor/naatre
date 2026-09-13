package runtime

import (
	"context"
	"errors"
	"time"
)

var ErrInvalidStreamCheckExecutor = errors.New("invalid stream check executor")

var errStreamCheckTimeout = errors.New("stream check timed out")

// StreamCheckExecutor bounds application callbacks that may outlive their
// delivery deadline. Applications share one executor across the process.
type StreamCheckExecutor struct {
	slots chan struct{}
}

func NewStreamCheckExecutor(maxConcurrent int) (*StreamCheckExecutor, error) {
	if maxConcurrent <= 0 {
		return nil, ErrInvalidStreamCheckExecutor
	}
	return &StreamCheckExecutor{slots: make(chan struct{}, maxConcurrent)}, nil
}

// Active returns callbacks that still own an executor slot.
func (e *StreamCheckExecutor) Active() int {
	if e == nil {
		return 0
	}
	return len(e.slots)
}

func (e *StreamCheckExecutor) run(ctx context.Context, timeout time.Duration, callback func(context.Context) error, panicErr error) error {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := streamContextError(checkCtx); err != nil {
		return err
	}
	select {
	case e.slots <- struct{}{}:
	case <-checkCtx.Done():
		return normalizeStreamCheckCause(ctx, context.Cause(checkCtx))
	}
	if err := streamContextError(checkCtx); err != nil {
		<-e.slots
		return normalizeStreamCheckCause(ctx, err)
	}
	returned := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recover() != nil {
				err = panicErr
			}
			returned <- err
			<-e.slots
		}()
		err = callback(checkCtx)
	}()
	select {
	case err := <-returned:
		if cause := context.Cause(checkCtx); cause != nil {
			return normalizeStreamCheckCause(ctx, cause)
		}
		return err
	case <-checkCtx.Done():
		return normalizeStreamCheckCause(ctx, context.Cause(checkCtx))
	}
}

func normalizeStreamCheckCause(parent context.Context, cause error) error {
	if parentCause := context.Cause(parent); parentCause != nil {
		return parentCause
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return errStreamCheckTimeout
	}
	return cause
}
