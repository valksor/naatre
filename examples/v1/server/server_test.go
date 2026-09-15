package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valksor/naatre/examples/v1/server"
	"github.com/valksor/naatre/runtime"
	naatrehttp "github.com/valksor/naatre/transport/http"
)

func TestQuickStartBuildsAuthenticatedBoundedHandler(t *testing.T) {
	t.Parallel()
	handler, err := server.New(runtime.Snapshot{}, func(ctx context.Context, _ *http.Request) (context.Context, error) {
		return ctx, nil
	}, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, naatrehttp.DefaultPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d headers=%v", response.Code, response.Header())
	}
}

func TestQuickStartRequiresAuthentication(t *testing.T) {
	t.Parallel()
	if _, err := server.New(runtime.Snapshot{}, nil, nil); err == nil {
		t.Fatal("server accepted a missing authenticator")
	}
}
