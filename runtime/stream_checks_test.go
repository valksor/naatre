package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamCheckExecutorRetainsAndReclaimsUncooperativeCallbackSlot(t *testing.T) {
	executor, err := NewStreamCheckExecutor(1)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	err = runStreamSessionCheck(context.Background(), executor, 20*time.Millisecond, func(context.Context) error {
		close(started)
		<-release
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncooperative timeout = %v", err)
	}
	<-started
	if executor.Active() != 1 {
		t.Fatalf("active callbacks = %d, want 1", executor.Active())
	}
	var secondStarted atomic.Bool
	err = runStreamSessionCheck(context.Background(), executor, 20*time.Millisecond, func(context.Context) error {
		secondStarted.Store(true)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || secondStarted.Load() {
		t.Fatalf("saturated check = %v, started=%v", err, secondStarted.Load())
	}
	close(release)
	if err := runStreamSessionCheck(context.Background(), executor, time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("reclaimed executor = %v", err)
	}
}

func TestStreamCheckExecutorCancellationAndPanicCannotSucceedOrLeak(t *testing.T) {
	executor, err := NewStreamCheckExecutor(1)
	if err != nil {
		t.Fatal(err)
	}
	err = runStreamSessionCheck(context.Background(), executor, 20*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("nil-after-cancel result = %v", err)
	}

	if err := runStreamSessionCheck(context.Background(), executor, time.Second, func(context.Context) error { panic("boom") }); !errors.Is(err, ErrStreamAuthorizationRevoked) {
		t.Fatalf("panic result = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var started atomic.Bool
	if err := runStreamSessionCheck(ctx, executor, time.Second, func(context.Context) error {
		started.Store(true)
		return nil
	}); !errors.Is(err, context.Canceled) || started.Load() {
		t.Fatalf("pre-cancelled result = %v, started=%v", err, started.Load())
	}
}
