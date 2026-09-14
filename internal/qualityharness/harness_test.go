package qualityharness_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/internal/qualityharness"
)

func TestLeakDetectionPositiveAndNegativeFixtures(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		resource qualityharness.Resource
		code     string
	}{
		{qualityharness.Goroutine, qualityharness.CodeGoroutineLeak},
		{qualityharness.Body, qualityharness.CodeBodyLeak},
		{qualityharness.Stream, qualityharness.CodeStreamLeak},
	} {
		fixture := fixture
		t.Run(string(fixture.resource), func(t *testing.T) {
			t.Parallel()
			var tracker qualityharness.Tracker
			release, err := tracker.Acquire(fixture.resource)
			if err != nil {
				t.Fatal(err)
			}
			if got := qualityharness.Code(tracker.Check()); got != fixture.code {
				t.Fatalf("leak code = %q, want %q", got, fixture.code)
			}
			release()
			release()
			if err := tracker.Check(); err != nil {
				t.Fatalf("released resource reported as leaked: %v", err)
			}
		})
	}
}

func TestLeakTrackerCancellationTerminatesWithinBudget(t *testing.T) {
	t.Parallel()
	var tracker qualityharness.Tracker
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	if err := tracker.Go(ctx, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	waitCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := tracker.Wait(waitCtx); err != nil {
		t.Fatalf("tracked goroutine did not terminate: %v", err)
	}
}

func TestLeakTrackerReportsResourceBudgetBeforeCleanup(t *testing.T) {
	t.Parallel()
	var tracker qualityharness.Tracker
	blocked := make(chan struct{})
	if err := tracker.Go(context.Background(), func(context.Context) { <-blocked }); err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithCancel(context.Background())
	cancel()
	if got := qualityharness.Code(tracker.Wait(waitContext)); got != qualityharness.CodeBudgetExceeded {
		t.Fatalf("budget code = %q, want %q", got, qualityharness.CodeBudgetExceeded)
	}
	close(blocked)
	cleanupContext, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := tracker.Wait(cleanupContext); err != nil {
		t.Fatalf("cleanup after budget failure: %v", err)
	}
}

func TestFaultInjectionIsDeterministicAndSanitized(t *testing.T) {
	t.Parallel()
	const secret = "credential-do-not-expose"
	injector, err := qualityharness.NewFaultInjector(map[string][]uint64{"transport.read": {2, 4}})
	if err != nil {
		t.Fatal(err)
	}
	for call := 1; call <= 5; call++ {
		err := injector.Inject("transport.read")
		wantFailure := call == 2 || call == 4
		if (err != nil) != wantFailure {
			t.Fatalf("call %d failure = %v, want %t", call, err, wantFailure)
		}
		if err != nil {
			if qualityharness.Code(err) != qualityharness.CodeFaultInjected {
				t.Fatalf("call %d code = %q", call, qualityharness.Code(err))
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "transport.read") {
				t.Fatalf("public fault exposed protected detail: %q", err)
			}
		}
	}
}

func TestFaultInjectionConcurrentBoundary(t *testing.T) {
	t.Parallel()
	injector, err := qualityharness.NewFaultInjector(map[string][]uint64{"runtime.invoke": {32}})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	failures := make(chan error, 64)
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := injector.Inject("runtime.invoke"); err != nil {
				failures <- err
			}
		}()
	}
	wait.Wait()
	close(failures)
	var count int
	for failure := range failures {
		count++
		if !errors.As(failure, new(*qualityharness.Failure)) {
			t.Fatalf("unexpected failure type %T", failure)
		}
	}
	if count != 1 {
		t.Fatalf("injected failures = %d, want 1", count)
	}
}

func TestInvalidHarnessConfigurationUsesStableCode(t *testing.T) {
	t.Parallel()
	for _, schedule := range []map[string][]uint64{
		{"": {1}},
		{"valid": {0}},
		{"valid": {1, 1}},
	} {
		_, err := qualityharness.NewFaultInjector(schedule)
		if qualityharness.Code(err) != qualityharness.CodeInvalidConfiguration {
			t.Fatalf("invalid configuration code = %q", qualityharness.Code(err))
		}
	}
}

func TestCodeRetainsWrappedStableFailure(t *testing.T) {
	err := fmt.Errorf("outer detail: %w", &qualityharness.Failure{Code: qualityharness.CodeStreamLeak})
	if qualityharness.Code(err) != qualityharness.CodeStreamLeak {
		t.Fatalf("wrapped code = %q", qualityharness.Code(err))
	}
}
