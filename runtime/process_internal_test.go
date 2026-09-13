package runtime

import (
	"context"
	"testing"
	"time"
)

func TestProcessHookWorkerStopsAtTerminalOwnershipBoundary(t *testing.T) {
	t.Parallel()
	newController := func(t *testing.T, config ProcessConfig) *ProcessController {
		t.Helper()
		controller, err := NewProcessController(config, ProcessRevision{
			Revision: "r1", SchemaRevision: "schema-r1", ConfigurationRevision: "config-r1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := controller.Start(); err != nil {
			t.Fatal(err)
		}
		return controller
	}
	t.Run("graceful", func(t *testing.T) {
		config := DefaultProcessConfig()
		config.Hooks.Observe = func(ProcessEvent) error { return nil }
		controller := newController(t, config)
		if outcome := controller.Drain(context.Background()); outcome != DrainCompleted {
			t.Fatalf("drain outcome = %q", outcome)
		}
		assertProcessHookWorkerStopped(t, controller)
	})
	t.Run("forced waits for ownership release", func(t *testing.T) {
		config := DefaultProcessConfig()
		config.MaxDrainDuration = time.Millisecond
		config.Hooks.Observe = func(ProcessEvent) error { return nil }
		controller := newController(t, config)
		lease, err := controller.Admit(context.Background(), AdmissionRequest{
			TenantReference: "tenant", PrincipalReference: "principal", Kind: WorkDurable,
			ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome := controller.Drain(context.Background()); outcome != DrainForced {
			t.Fatalf("drain outcome = %q", outcome)
		}
		select {
		case <-controller.hookDone:
			t.Fatal("hook worker stopped before forced ownership release")
		default:
		}
		lease.Release()
		assertProcessHookWorkerStopped(t, controller)
	})
}

func TestProcessLeaseDoesNotRetainRawPartitionReferences(t *testing.T) {
	t.Parallel()
	controller, err := NewProcessController(DefaultProcessConfig(), ProcessRevision{
		Revision: "r1", SchemaRevision: "schema-r1", ConfigurationRevision: "config-r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	lease, err := controller.Admit(context.Background(), AdmissionRequest{
		TenantReference: "private-tenant", PrincipalReference: "private-principal", Kind: WorkQuery,
		ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.request.TenantReference != "" || lease.request.PrincipalReference != "" {
		t.Fatalf("lease retained raw partition references: %#v", lease.request)
	}
}

func assertProcessHookWorkerStopped(t *testing.T, controller *ProcessController) {
	t.Helper()
	select {
	case <-controller.hookDone:
	case <-time.After(time.Second):
		t.Fatal("process hook worker did not stop")
	}
}
