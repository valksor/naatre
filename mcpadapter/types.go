package mcpadapter

import (
	"encoding/json"
	"slices"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const (
	Profile                = "adapter.mcp-1"
	MCPRevision            = "2025-11-25"
	MCPSpecification       = "https://modelcontextprotocol.io/specification/2025-11-25"
	NaatreSchemaRevision   = "core.schema-1"
	NaatreProtocolRevision = "core.protocol-1"
	ConformanceRevision    = "1.0.0"
)

type Direction string

const (
	Expose  Direction = "naatre-to-mcp"
	Consume Direction = "mcp-to-naatre"
)

type Transport string

const (
	Stdio          Transport = "stdio"
	StreamableHTTP Transport = "streamable-http"
)

type ProcessOwner string

const (
	AdapterOwnsProcess     ProcessOwner = "adapter"
	ApplicationOwnsProcess ProcessOwner = "application"
	ClientOwnsProcess      ProcessOwner = "mcp-client"
)

type EntryKind string

const (
	ToolKind             EntryKind = "tool"
	ResourceKind         EntryKind = "resource"
	ResourceTemplateKind EntryKind = "resource-template"
)

type Limits struct {
	MaxCatalogBytes   int
	MaxSchemaBytes    int
	MaxSchemaDepth    int
	MaxEntries        int
	MaxIdentifier     int
	MaxProgressEvents int
	MaxContentBytes   int
}

func DefaultLimits() Limits {
	return Limits{MaxCatalogBytes: 1 << 20, MaxSchemaBytes: 256 << 10, MaxSchemaDepth: 32, MaxEntries: 256, MaxIdentifier: 256, MaxProgressEvents: 1024, MaxContentBytes: 1 << 20}
}

type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	MIMEType    string          `json:"mimeType,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

type ResourceTemplate struct {
	URITemplate string          `json:"uriTemplate"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	MIMEType    string          `json:"mimeType,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

type Prompt struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

type Catalog struct {
	ProtocolVersion   string             `json:"protocolVersion"`
	Capabilities      []string           `json:"capabilities"`
	Tools             []Tool             `json:"tools,omitempty"`
	Resources         []Resource         `json:"resources,omitempty"`
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates,omitempty"`
	Prompts           []Prompt           `json:"prompts,omitempty"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

type Registration struct {
	Kind         EntryKind
	RemoteID     string
	Approved     bool
	Descriptor   runtime.Descriptor
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Invoker      interopadapter.Invoker
}

type TransportConfig struct {
	Kind             Transport
	Endpoint         string
	AllowedOrigins   []string
	SessionHeader    string
	OriginValidation bool
	Reconnect        bool
	ProcessOwner     ProcessOwner
	MaxResponseBytes int
	MaxPending       int
}

type Config struct {
	AdapterID            string
	Direction            Direction
	SchemaRevision       string
	ProtocolRevision     string
	ConformanceRevision  string
	Transport            TransportConfig
	Catalog              Catalog
	Types                schema.Snapshot
	Limits               Limits
	RequiredCapabilities []string
	Registrations        []Registration
}

type Classification string

const (
	Lossless            Classification = "lossless"
	ExplicitlyAdapted   Classification = "explicitly-adapted"
	Unsupported         Classification = "unsupported"
	ApplicationSupplied Classification = "application-supplied"
)

type Mapping struct {
	Feature        string         `json:"feature"`
	Classification Classification `json:"classification"`
	Required       bool           `json:"required"`
	Resolution     string         `json:"resolution,omitempty"`
}

type Revisions struct {
	MCP         string `json:"mcp"`
	Schema      string `json:"naatreSchema"`
	Protocol    string `json:"naatreProtocol"`
	Conformance string `json:"conformance"`
}

type Policy struct {
	Kind                protocol.OperationKind           `json:"kind"`
	Effect              runtime.Effect                   `json:"effect"`
	Idempotency         runtime.IdempotencyPolicy        `json:"idempotency"`
	RetrySafe           bool                             `json:"retrySafe"`
	Cacheable           bool                             `json:"cacheable"`
	Cost                uint64                           `json:"cost"`
	Batching            runtime.Batching                 `json:"batching"`
	Transaction         runtime.TransactionParticipation `json:"transaction"`
	AuthorizationPolicy string                           `json:"authorizationPolicy"`
}

type RegistrationReport struct {
	Kind       EntryKind `json:"kind"`
	RemoteID   string    `json:"remoteId"`
	NaatreName string    `json:"naatreName"`
	Policy     Policy    `json:"policy"`
}

type Diagnostic struct {
	Code     string `json:"code"`
	RemoteID string `json:"remoteId,omitempty"`
	Feature  string `json:"feature,omitempty"`
	Message  string `json:"message"`
}

type TransportReport struct {
	Kind               Transport    `json:"kind"`
	SessionIsolation   string       `json:"sessionIsolation"`
	OriginValidation   string       `json:"originValidation"`
	Reconnect          bool         `json:"reconnect"`
	Cancellation       string       `json:"cancellation"`
	Backpressure       string       `json:"backpressure"`
	ResponseLimit      int          `json:"responseLimit"`
	ProcessOwnership   ProcessOwner `json:"processOwnership"`
	LifecycleClaimOnly bool         `json:"lifecycleClaimOnly"`
}

type FidelityReport struct {
	Profile       string               `json:"profile"`
	AdapterID     string               `json:"adapterId"`
	Direction     Direction            `json:"direction"`
	Status        string               `json:"status"`
	Revisions     Revisions            `json:"revisions"`
	Mappings      []Mapping            `json:"mappings"`
	Transport     TransportReport      `json:"transport"`
	Registrations []RegistrationReport `json:"registrations"`
	Diagnostics   []Diagnostic         `json:"diagnostics"`
	Claims        []string             `json:"claims"`
	NonClaims     []string             `json:"nonClaims"`
}

type ManifestEntry struct {
	Kind         EntryKind       `json:"kind"`
	RemoteID     string          `json:"remoteId"`
	NaatreName   string          `json:"naatreName"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Policy       Policy          `json:"policy"`
}

type Manifest struct {
	Profile    string          `json:"profile"`
	Direction  Direction       `json:"direction"`
	Revisions  Revisions       `json:"revisions"`
	Transport  Transport       `json:"transport"`
	Endpoint   string          `json:"endpoint,omitempty"`
	Entries    []ManifestEntry `json:"entries"`
	WireClaims []string        `json:"wireClaims"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func cloneCatalog(input Catalog) Catalog {
	result := input
	result.Capabilities = slices.Clone(input.Capabilities)
	result.Tools = slices.Clone(input.Tools)
	for index := range result.Tools {
		result.Tools[index].InputSchema = slices.Clone(result.Tools[index].InputSchema)
		result.Tools[index].OutputSchema = slices.Clone(result.Tools[index].OutputSchema)
		result.Tools[index].Annotations = slices.Clone(result.Tools[index].Annotations)
	}
	result.Resources = slices.Clone(input.Resources)
	result.ResourceTemplates = slices.Clone(input.ResourceTemplates)
	result.Prompts = slices.Clone(input.Prompts)
	return result
}
