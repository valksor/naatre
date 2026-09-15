// Package gateway is the executable Go-gateway quick start for a remote worker.
// It is not an independent native runtime in the worker's language.
package gateway

import (
	"context"
	"encoding/json"

	"github.com/valksor/naatre/remoteworker"
)

const (
	schemaRevision = "schema-1"
	schemaDigest   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// New creates a bounded, deny-by-default gateway for the shared fixture schema.
func New(transport remoteworker.Transport) (*remoteworker.ReferenceGateway, error) {
	return remoteworker.NewReferenceGateway(remoteworker.GatewayConfig{
		Transport: transport, Endpoint: "fixture-stdio", WorkerID: "fixture-worker",
		ServiceIdentity: "spiffe://example/fixture-worker", Audience: "naatre-gateway",
		SchemaRevision: schemaRevision, SchemaDigest: schemaDigest,
		MaxInFlight: 1, MaxAttempts: 1, MaxRequestBytes: 4096, MaxResponseBytes: 4096,
		MaxStreamFrames: 4, MaxStreamBytes: 16384, MaxReferences: 16, MaxSeen: 32,
		VerifyDelegation: func(_ context.Context, token string, _ remoteworker.DelegationExpectation) error {
			if token != "valid-delegation" {
				return errDenied
			}
			return nil
		},
		Authorize: func(_ context.Context, request remoteworker.AuthorizationRequest) error {
			if request.HandlerID != "fixture.greet" || request.Effect != remoteworker.EffectQuery {
				return errDenied
			}
			return nil
		},
		ValidateInput: func(schemaID string, input json.RawMessage) error {
			if schemaID != "GreetInput" || !json.Valid(input) {
				return errDenied
			}
			return nil
		},
		ValidateOutput: func(schemaID string, output json.RawMessage) error {
			if schemaID != "GreetOutput" || !json.Valid(output) {
				return errDenied
			}
			return nil
		},
	})
}

type deniedError struct{}

func (deniedError) Error() string { return "denied" }

var errDenied deniedError
