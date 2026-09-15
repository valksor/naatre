package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
)

const (
	// RuntimeProfile is the Go client/server integration profile owned by issue
	// 103. It consumes Profile and does not define another MCP or Naatre schema.
	RuntimeProfile  = "adapter.mcp-go-runtime-1"
	Issue64Revision = "6e894e925dd7c24f3b1d7bd8c07c1040a2a11fcd"
	JSONRPCVersion  = "2.0"
)

const (
	CodeCancelled             = "MCP_CANCELLED"
	CodeCapabilityUnsupported = "MCP_CAPABILITY_UNSUPPORTED"
	CodeInternal              = "MCP_INTERNAL"
	CodeMethodUnsupported     = "MCP_METHOD_UNSUPPORTED"
	CodeNotInitialized        = "MCP_NOT_INITIALIZED"
	CodePartialResult         = "MCP_PARTIAL_RESULT"
	CodeProcessDied           = "MCP_PROCESS_DIED"
	CodeRequestInvalid        = "MCP_REQUEST_INVALID"
	CodeResourceFailed        = "MCP_RESOURCE_FAILED"
	CodeResourceLimit         = "MCP_RESOURCE_LIMIT"
	CodeResourceUnknown       = "MCP_RESOURCE_UNKNOWN"
	CodeResponseInvalid       = "MCP_RESPONSE_INVALID"
	CodeResponseTooLarge      = "MCP_RESPONSE_TOO_LARGE"
	CodeSessionInvalid        = "MCP_SESSION_INVALID"
	CodeToolFailed            = "MCP_TOOL_FAILED"
	CodeToolUnknown           = "MCP_TOOL_UNKNOWN"
	CodeTransportFailed       = "MCP_TRANSPORT_FAILED"
	CodeUnauthorized          = "MCP_UNAUTHORIZED"
	CodeValueInvalid          = "MCP_VALUE_INVALID"
)

type WireLimits struct {
	MaxRequestBytes  int
	MaxResponseBytes int
	MaxPending       int
	MaxPages         int
	MaxSessions      int
}

func DefaultWireLimits() WireLimits {
	return WireLimits{
		MaxRequestBytes:  1 << 20,
		MaxResponseBytes: 1 << 20,
		MaxPending:       64,
		MaxPages:         8,
		MaxSessions:      256,
	}
}

func normalizeWireLimits(input WireLimits) (WireLimits, error) {
	defaults := DefaultWireLimits()
	values := []*int{&input.MaxRequestBytes, &input.MaxResponseBytes, &input.MaxPending, &input.MaxPages, &input.MaxSessions}
	fallbacks := []int{defaults.MaxRequestBytes, defaults.MaxResponseBytes, defaults.MaxPending, defaults.MaxPages, defaults.MaxSessions}
	for index, value := range values {
		if *value == 0 {
			*value = fallbacks[index]
		}
		if *value < 1 {
			return WireLimits{}, adapterError("MCP_TRANSPORT_LIMIT", errors.New("wire limits must be positive"))
		}
	}
	if input.MaxRequestBytes < 512 || input.MaxResponseBytes < 512 {
		return WireLimits{}, adapterError("MCP_TRANSPORT_LIMIT", errors.New("wire byte limits are too small for safe protocol errors"))
	}
	if input.MaxRequestBytes > 16<<20 || input.MaxResponseBytes > 16<<20 || input.MaxPending > 4096 || input.MaxPages > 256 || input.MaxSessions > 1<<16 {
		return WireLimits{}, adapterError("MCP_TRANSPORT_LIMIT", errors.New("wire limits exceed the supported profile maximum"))
	}
	return input, nil
}

func validateRuntimeRequirements(required []string) error {
	supported := []string{"cancellation", "pagination", "resources", "structured-content", "tools"}
	for _, capability := range required {
		if !slices.Contains(supported, capability) {
			return adapterError(CodeCapabilityUnsupported, errors.New("required capability is unsupported by the Go runtime profile"))
		}
	}
	return nil
}

type PeerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Identity struct {
	Principal string
	Tenant    string
}

func (i Identity) valid() bool {
	return boundedIdentity(i.Principal) && boundedIdentity(i.Tenant)
}

type ToolHandler func(context.Context, json.RawMessage) (json.RawMessage, error)

type ResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

type ResourceHandler func(context.Context) (ResourceContent, error)

// ClientTransport exchanges already-bounded JSON-RPC frames. Implementations
// must honor ctx and return only stable mcpadapter errors.
type ClientTransport interface {
	Kind() Transport
	RoundTrip(context.Context, []byte) ([]byte, error)
}

// RuntimeEvidence is the machine-readable profile boundary emitted by both
// clients and servers. Unsupported is exhaustive for this runtime revision.
type RuntimeEvidence struct {
	Profile      string     `json:"profile"`
	CoreProfile  string     `json:"coreProfile"`
	MCPRevision  string     `json:"mcpRevision"`
	Direction    Direction  `json:"direction"`
	Transport    Transport  `json:"transport"`
	Mappings     []Mapping  `json:"mappings"`
	Supported    []string   `json:"supported"`
	Unsupported  []string   `json:"unsupported"`
	FailureCodes []string   `json:"failureCodes"`
	WireLimits   WireLimits `json:"wireLimits"`
}

func runtimeEvidence(direction Direction, transport Transport, limits WireLimits) RuntimeEvidence {
	return RuntimeEvidence{
		Profile: RuntimeProfile, CoreProfile: Profile, MCPRevision: MCPRevision,
		Direction: direction, Transport: transport, WireLimits: limits, Mappings: DefaultMappings(),
		Supported: []string{
			"bounded-json-rpc", "bounded-schema-instance-conversion", "capability-negotiation", "cancellation",
			"partial-envelope-fidelity", "safe-public-errors", "structured-content", "trusted-policy",
			"trusted-resource-projection", "trusted-tool-projection",
		},
		Unsupported: []string{
			"audio-content", "automatic-auth-refresh", "automatic-operation-retry", "batch-json-rpc",
			"completion", "elicitation", "embedded-resources", "http-delete-session", "http-get-sse",
			"image-content", "implicit-file-access", "logging-forwarding", "multi-server-routing",
			"oauth-discovery", "prompt-execution", "resource-subscriptions", "resource-templates-runtime",
			"roots", "sampling", "server-initiated-requests", "sse-legacy-transport", "streaming-tool-results",
			"task-augmentation", "websocket", "wire-compression",
		},
		FailureCodes: []string{
			CodeCancelled, CodeCapabilityUnsupported, CodeInternal, CodeMethodUnsupported,
			CodeNotInitialized, CodePartialResult, CodeProcessDied, CodeRequestInvalid, CodeResourceFailed, CodeResourceLimit, CodeResourceUnknown, CodeResponseInvalid,
			CodeResponseTooLarge, CodeSessionInvalid, CodeToolFailed, CodeToolUnknown,
			CodeTransportFailed, CodeUnauthorized, CodeValueInvalid,
		},
	}
}

func cloneEvidence(input RuntimeEvidence) RuntimeEvidence {
	result := input
	result.Mappings = slices.Clone(input.Mappings)
	result.Supported = slices.Clone(input.Supported)
	result.Unsupported = slices.Clone(input.Unsupported)
	result.FailureCodes = slices.Clone(input.FailureCodes)
	return result
}
