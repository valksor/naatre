package qualityharness_test

import (
	"testing"

	"github.com/valksor/naatre/internal/qualityharness"
)

func FuzzFaultSchedule(f *testing.F) {
	f.Add("transport.read", uint64(1), uint64(2))
	f.Add("runtime.invoke", uint64(32), uint64(64))
	f.Fuzz(func(t *testing.T, point string, first, second uint64) {
		if first == 0 || second == 0 || first == second {
			return
		}
		injector, err := qualityharness.NewFaultInjector(map[string][]uint64{point: {first, second}})
		if err != nil {
			if qualityharness.Code(err) != qualityharness.CodeInvalidConfiguration {
				t.Fatalf("invalid schedule code = %q", qualityharness.Code(err))
			}
			return
		}
		maximum := min(max(first, second), 1024)
		for range maximum {
			failure := injector.Inject(point)
			if failure != nil && qualityharness.Code(failure) != qualityharness.CodeFaultInjected {
				t.Fatalf("injected code = %q", qualityharness.Code(failure))
			}
		}
	})
}

func FuzzResourceLifecycle(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 4096 {
			return
		}
		var tracker qualityharness.Tracker
		releases := make([]func(), 0, len(operations))
		for _, operation := range operations {
			resource := []qualityharness.Resource{qualityharness.Goroutine, qualityharness.Body, qualityharness.Stream}[operation%3]
			release, err := tracker.Acquire(resource)
			if err != nil {
				t.Fatal(err)
			}
			releases = append(releases, release)
		}
		for _, release := range releases {
			release()
		}
		if err := tracker.Check(); err != nil {
			t.Fatalf("balanced lifecycle reported a leak: %v", err)
		}
	})
}
