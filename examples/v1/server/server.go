// Package server assembles the reference Go runtime and HTTP transport without
// owning a router, listener, TLS, or process supervisor.
package server

import (
	"errors"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	naatrehttp "github.com/valksor/naatre/transport/http"
)

// New returns a bounded unary handler for an immutable, deny-by-default runtime
// snapshot. Callers own TLS, an ephemeral test listener, supervision, and drain.
func New(snapshot runtime.Snapshot, authenticate naatrehttp.AuthenticateFunc, shutdown <-chan struct{}) (*naatrehttp.Handler, error) {
	if authenticate == nil {
		return nil, errors.New("authentication is required")
	}
	return naatrehttp.NewHandler(naatrehttp.Config{
		Executor:        naatrehttp.RuntimeExecutor(snapshot, runtime.PrepareOptions{}, runtime.ExecuteOptions{}),
		Decode:          protocol.DecodeOptions{},
		Limits:          naatrehttp.DefaultLimits(),
		Authenticate:    authenticate,
		WWWAuthenticate: `Bearer realm="naatre"`,
		Shutdown:        shutdown,
	})
}
