package processhost

import (
	"context"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
)

func TestHealthEndpointsTrackProcessLifecycleWithoutMetadata(t *testing.T) {
	t.Parallel()
	controller, err := runtime.NewProcessController(runtime.DefaultProcessConfig(), exampleRevision())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(controller, fixedAdmission, func(context.Context, runtime.AdmittedWork, http.ResponseWriter, *http.Request) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHealth(t, handler, "/health/live", http.StatusOK, `"live":true`)
	assertHealth(t, handler, "/health/ready", http.StatusServiceUnavailable, `"ready":false`)
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	assertHealth(t, handler, "/health/ready", http.StatusOK, `"ready":true`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	for _, protected := range []string{"r1", "schema-r1", "configuration-r1", "tenant", "principal"} {
		if strings.Contains(response.Body.String(), protected) {
			t.Fatalf("health response leaked %q: %s", protected, response.Body.String())
		}
	}
}

func TestHealthEndpointsTrackEssentialDependencyFailureAndRecovery(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	config := runtime.DefaultProcessConfig()
	config.Clock = func() time.Time { return now }
	config.DependencyFailureGrace = time.Second
	config.Dependencies = []runtime.DependencyConfig{{Name: "database-password-secret", Essential: true, Ready: true}}
	controller := startedExampleController(t, config)
	handler, err := NewHandler(controller, fixedAdmission, func(context.Context, runtime.AdmittedWork, http.ResponseWriter, *http.Request) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.SetDependency("database-password-secret", false); err != nil {
		t.Fatal(err)
	}
	assertHealth(t, handler, "/health/ready", http.StatusServiceUnavailable, `"ready":false`)
	assertHealth(t, handler, "/health/live", http.StatusOK, `"live":true`)
	now = now.Add(time.Second)
	assertHealth(t, handler, "/health/live", http.StatusServiceUnavailable, `"live":false`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if strings.Contains(response.Body.String(), "database-password-secret") {
		t.Fatalf("health response leaked dependency metadata: %s", response.Body.String())
	}
	if err := controller.SetDependency("database-password-secret", true); err != nil {
		t.Fatal(err)
	}
	assertHealth(t, handler, "/health/ready", http.StatusOK, `"ready":true`)
	assertHealth(t, handler, "/health/live", http.StatusOK, `"live":true`)
}

func TestAdmissionRejectsBeforeHTTPBusinessCallback(t *testing.T) {
	t.Parallel()
	config := runtime.DefaultProcessConfig()
	config.Limits.MaxInFlight = 1
	config.Limits.MaxQueued = 1
	controller := startedExampleController(t, config)
	active, err := controller.Admit(context.Background(), fixedAdmission(nil))
	if err != nil {
		t.Fatal(err)
	}
	queuedResult := make(chan *runtime.WorkLease, 1)
	go func() {
		lease, _ := controller.Admit(context.Background(), runtime.AdmissionRequest{
			TenantReference: "queued", PrincipalReference: "queued", Kind: runtime.WorkQuery,
			ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
		})
		queuedResult <- lease
	}()
	deadline := time.Now().Add(time.Second)
	for controller.Stats().Queued != 1 && time.Now().Before(deadline) {
		goruntime.Gosched()
	}
	if controller.Stats().Queued != 1 {
		t.Fatal("timed out waiting for queued admission")
	}
	var calls atomic.Int64
	handler, err := NewHandler(controller, func(*http.Request) runtime.AdmissionRequest {
		return runtime.AdmissionRequest{
			TenantReference: "overloaded", PrincipalReference: "overloaded", Kind: runtime.WorkQuery,
			ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
		}
	}, func(context.Context, runtime.AdmittedWork, http.ResponseWriter, *http.Request) error {
		calls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/work", nil))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" ||
		!strings.Contains(response.Body.String(), runtime.CodeOverloaded) {
		t.Fatalf("overload response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("business callback ran %d times", calls.Load())
	}
	active.Release()
	(<-queuedResult).Release()
}

func TestSuccessfulHTTPWorkUsesAdmittedLease(t *testing.T) {
	t.Parallel()
	controller := startedExampleController(t, runtime.DefaultProcessConfig())
	handler, err := NewHandler(controller, fixedAdmission, func(ctx context.Context, work runtime.AdmittedWork, writer http.ResponseWriter, _ *http.Request) error {
		if ctx == nil || work.Revision().Revision != "r1" {
			t.Fatal("business callback did not receive admitted lease snapshot")
		}
		writer.WriteHeader(http.StatusNoContent)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/work", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("work status = %d, body=%s", response.Code, response.Body.String())
	}
	if stats := controller.Stats(); stats.InFlight != 0 {
		t.Fatalf("completed HTTP work retained accounting: %#v", stats)
	}
}

func fixedAdmission(*http.Request) runtime.AdmissionRequest {
	return runtime.AdmissionRequest{
		TenantReference: "tenant", PrincipalReference: "principal", Kind: runtime.WorkQuery,
		ReservedBytes: 1, QueueBytes: 1, ResultBufferBytes: 1,
	}
}

func exampleRevision() runtime.ProcessRevision {
	return runtime.ProcessRevision{Revision: "r1", SchemaRevision: "schema-r1", ConfigurationRevision: "configuration-r1"}
}

func startedExampleController(t *testing.T, config runtime.ProcessConfig) *runtime.ProcessController {
	t.Helper()
	controller, err := runtime.NewProcessController(config, exampleRevision())
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	return controller
}

func assertHealth(t *testing.T, handler http.Handler, path string, status int, contains string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != status || !strings.Contains(response.Body.String(), contains) {
		t.Fatalf("%s = %d body=%s, want %d containing %q", path, response.Code, response.Body.String(), status, contains)
	}
}
