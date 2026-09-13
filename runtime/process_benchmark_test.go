package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
)

func BenchmarkProcessAdmission(b *testing.B) {
	b.Run("admit-release", func(b *testing.B) {
		controller := newStartedProcessController(b, runtime.DefaultProcessConfig())
		request := processRequest("tenant-a", runtime.WorkQuery, 1)
		b.ReportAllocs()
		for b.Loop() {
			lease, err := controller.Admit(context.Background(), request)
			if err != nil {
				b.Fatal(err)
			}
			lease.Release()
		}
	})
	b.Run("reject-oversized", func(b *testing.B) {
		config := runtime.DefaultProcessConfig()
		config.Limits.MaxReservedBytes = 1
		controller := newStartedProcessController(b, config)
		request := processRequest("tenant-a", runtime.WorkQuery, 2)
		b.ReportAllocs()
		for b.Loop() {
			lease, err := controller.Admit(context.Background(), request)
			if lease != nil || err == nil {
				b.Fatalf("oversized admission = lease %#v, error %v", lease, err)
			}
		}
	})
}

func BenchmarkProcessLifecycle(b *testing.B) {
	b.Run("completed-drain", func(b *testing.B) {
		config := runtime.DefaultProcessConfig()
		b.ReportAllocs()
		for b.Loop() {
			controller, err := runtime.NewProcessController(config, processRevision("r1"))
			if err != nil {
				b.Fatal(err)
			}
			if err := controller.Start(); err != nil {
				b.Fatal(err)
			}
			if outcome := controller.Drain(context.Background()); outcome != runtime.DrainCompleted {
				b.Fatalf("drain outcome = %q", outcome)
			}
		}
	})
	b.Run("forced-drain", func(b *testing.B) {
		config := runtime.DefaultProcessConfig()
		config.MaxDrainDuration = time.Nanosecond
		b.ReportAllocs()
		for b.Loop() {
			controller := newStartedProcessController(b, config)
			lease := mustAdmit(b, controller, processRequest("tenant-a", runtime.WorkDurable, 1))
			if outcome := controller.Drain(context.Background()); outcome != runtime.DrainForced {
				b.Fatalf("drain outcome = %q", outcome)
			}
			lease.Release()
		}
	})
}

func BenchmarkConnectionDeadline(b *testing.B) {
	controller := newStartedProcessController(b, runtime.DefaultProcessConfig())
	established := time.Unix(1_800_000_000, 0)
	b.ReportAllocs()
	for b.Loop() {
		_ = controller.ConnectionDeadline("bounded-reference", established, time.Time{})
	}
}
