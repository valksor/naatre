// Package remoteworker contains the transport-neutral v1 remote-worker wire
// model and a deliberately small reference gateway. It is conformance
// scaffolding, not the production HTTP/2 connection manager owned by issue
// #88.
package remoteworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	ProtocolVersion = "naatre.remote-worker.v1"
	CodecJSON       = "naatre.json-1"

	CapabilityUnary              = "unary-1"
	CapabilityClientStreaming    = "client-streaming-1"
	CapabilityServerStreaming    = "server-streaming-1"
	CapabilityBidirectional      = "bidirectional-streaming-1"
	CapabilityCancellationAck    = "cancellation-ack-1"
	CapabilityIdempotencyReplay  = "idempotency-replay-1"
	CapabilityTransactions       = "transaction-provider-1"
	CapabilitySubscriptionResume = "subscription-resume-1"
)

const (
	CodeInvalidRegistration  = "REMOTE_REGISTRATION_INVALID"
	CodeSchemaMismatch       = "REMOTE_SCHEMA_MISMATCH"
	CodeUnauthenticated      = "REMOTE_UNAUTHENTICATED"
	CodeUnauthorized         = "UNAUTHORIZED"
	CodeUnknownHandler       = "REMOTE_HANDLER_UNKNOWN"
	CodeCapabilityMismatch   = "REMOTE_CAPABILITY_MISMATCH"
	CodeInvalidInvocation    = "REMOTE_INVOCATION_INVALID"
	CodeDuplicateInvocation  = "REMOTE_INVOCATION_DUPLICATE"
	CodeMalformedWorkerData  = "REMOTE_WORKER_MALFORMED"
	CodeOutputInvalid        = "OUTPUT_COMPLETION"
	CodeOutcomeIndeterminate = "REMOTE_OUTCOME_INDETERMINATE"
	CodeOverloaded           = "OVERLOADED"
	CodeStaleReference       = "REMOTE_REFERENCE_UNAVAILABLE"
	CodeCancellationInvalid  = "REMOTE_CANCELLATION_INVALID"
)

type Effect string

const (
	EffectQuery        Effect = "query"
	EffectMutation     Effect = "mutation"
	EffectTransaction  Effect = "transaction"
	EffectSubscription Effect = "subscription"
)

type WorkerLimits struct {
	MaxInFlight      uint32 `json:"maxInFlight"`
	MaxRequestBytes  uint32 `json:"maxRequestBytes"`
	MaxResponseBytes uint32 `json:"maxResponseBytes"`
	MaxStreamFrames  uint32 `json:"maxStreamFrames"`
	MaxStreamBytes   uint64 `json:"maxStreamBytes"`
}

type Handler struct {
	ID                   string   `json:"id"`
	InputSchema          string   `json:"inputSchema"`
	OutputSchema         string   `json:"outputSchema"`
	Codec                string   `json:"codec"`
	Effect               Effect   `json:"effect"`
	RequiredCapabilities []string `json:"requiredCapabilities"`
}

type Registration struct {
	Protocol        string       `json:"protocol"`
	WorkerID        string       `json:"workerId"`
	ServiceIdentity string       `json:"serviceIdentity"`
	Audience        string       `json:"audience"`
	Endpoint        string       `json:"endpoint"`
	SchemaRevision  string       `json:"schemaRevision"`
	SchemaDigest    string       `json:"schemaDigest"`
	Capabilities    []string     `json:"capabilities"`
	Limits          WorkerLimits `json:"limits"`
	Handlers        []Handler    `json:"handlers"`
}

type RegistrationAck struct {
	Protocol             string   `json:"protocol"`
	WorkerID             string   `json:"workerId"`
	SessionID            string   `json:"sessionId"`
	SchemaRevision       string   `json:"schemaRevision"`
	AcceptedCapabilities []string `json:"acceptedCapabilities"`
}

type Parent struct {
	Value             json.RawMessage `json:"value,omitempty"`
	Reference         string          `json:"reference,omitempty"`
	OwnerInvocationID string          `json:"ownerInvocationId,omitempty"`
}

type InvokeRequest struct {
	RequestID        string
	InvocationID     string
	HandlerID        string
	IdempotencyKey   string
	DelegatedContext string
	Deadline         time.Time
	Parent           Parent
	Input            json.RawMessage
	ResumeCursor     string
}

type WorkerInvocation struct {
	Protocol          string          `json:"protocol"`
	RequestID         string          `json:"requestId"`
	InvocationID      string          `json:"invocationId"`
	AttemptID         string          `json:"attemptId"`
	HandlerID         string          `json:"handlerId"`
	SchemaRevision    string          `json:"schemaRevision"`
	DeadlineUnixMilli int64           `json:"deadlineUnixMilli"`
	IdempotencyKey    string          `json:"idempotencyKey,omitempty"`
	DelegatedContext  string          `json:"delegatedContext"`
	Parent            Parent          `json:"parent,omitempty"`
	Input             json.RawMessage `json:"input"`
	ResumeCursor      string          `json:"resumeCursor,omitempty"`
}

type WorkerError struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Retryable bool            `json:"retryable"`
	Details   json.RawMessage `json:"details,omitempty"`
}

type ReferenceLifetime string

const (
	ReferenceInvocation ReferenceLifetime = "invocation"
	ReferenceRequest    ReferenceLifetime = "request"
)

type ReferenceGrant struct {
	ID        string            `json:"id"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Lifetime  ReferenceLifetime `json:"lifetime"`
}

type WorkerResult struct {
	Protocol       string           `json:"protocol"`
	InvocationID   string           `json:"invocationId"`
	AttemptID      string           `json:"attemptId"`
	SchemaRevision string           `json:"schemaRevision"`
	Data           json.RawMessage  `json:"data,omitempty"`
	Errors         []WorkerError    `json:"errors"`
	References     []ReferenceGrant `json:"references,omitempty"`
}

type PublicResult struct {
	Data   json.RawMessage
	Errors []WorkerError
}

type CancellationDisposition string

const (
	CancellationRequested    CancellationDisposition = "requested"
	CancellationAcknowledged CancellationDisposition = "acknowledged"
	CancellationTooLate      CancellationDisposition = "too-late"
	CancellationUnsupported  CancellationDisposition = "unsupported"
)

type CancelRequest struct {
	Protocol     string `json:"protocol,omitempty"`
	RequestID    string `json:"requestId"`
	InvocationID string `json:"invocationId"`
}

type CancellationAck struct {
	Protocol     string                  `json:"protocol"`
	InvocationID string                  `json:"invocationId"`
	Disposition  CancellationDisposition `json:"disposition"`
}

type Transport interface {
	Register(context.Context, Registration) (RegistrationAck, error)
	Invoke(context.Context, WorkerInvocation) (WorkerResult, error)
	Cancel(context.Context, CancelRequest) (CancellationAck, error)
}

type DelegationExpectation struct {
	Audience  string
	RequestID string
	HandlerID string
	Deadline  time.Time
}

type AuthorizationRequest struct {
	RequestID    string
	InvocationID string
	HandlerID    string
	Effect       Effect
}

type GatewayError struct {
	Code    string
	Message string
	Cause   error
}

func (e *GatewayError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

func (e *GatewayError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func gatewayError(code, message string, cause error) error {
	return &GatewayError{Code: code, Message: message, Cause: cause}
}

type DeliveryPhase string

const (
	DeliveryBeforeWrite DeliveryPhase = "before-write"
	DeliveryAfterWrite  DeliveryPhase = "after-write"
)

type DeliveryError struct {
	Phase DeliveryPhase
	Cause error
}

func (e *DeliveryError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("remote delivery %s: %v", e.Phase, e.Cause)
}

func (e *DeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

var ErrBackpressure = errors.New("remote stream has no available credit")
