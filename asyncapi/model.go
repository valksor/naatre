// Package asyncapi defines Naatre's bounded AsyncAPI 3.0.0 event-description
// profile. It is a metadata adapter: Naatre remains authoritative for runtime,
// wire, authorization, ordering, retry, and partial-result semantics.
package asyncapi

import (
	"encoding/json"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/schema"
)

const (
	Profile          = "adapter.asyncapi-1"
	Version          = "3.0.0"
	ExporterVersion  = "asyncapi-exporter-1"
	Specification    = "https://www.asyncapi.com/docs/reference/specification/v3.0.0"
	SchemaFormat     = "application/vnd.naatre.schema-reference+json;version=1"
	DefaultMediaType = "application/json"
)

type Action string

const (
	ActionSend    Action = "send"
	ActionReceive Action = "receive"
)

type Transport string

const (
	TransportSSE         Transport = "sse"
	TransportWebSocket   Transport = "websocket"
	TransportWebhookHTTP Transport = "webhook-http"
	TransportWorker      Transport = "worker"
)

type Limits struct {
	MaxDocumentBytes int
	MaxDepth         int
	MaxObjects       int
	MaxChannels      int
	MaxMessages      int
	MaxReferences    int
	MaxExampleBytes  int
}

func DefaultLimits() Limits {
	return Limits{
		MaxDocumentBytes: 2 << 20, MaxDepth: 32, MaxObjects: 16_384,
		MaxChannels: 512, MaxMessages: 1024, MaxReferences: 8192, MaxExampleBytes: 64 << 10,
	}
}

type Revisions struct {
	Schema           string `json:"schema"`
	SchemaDigest     string `json:"schemaDigest"`
	Capabilities     string `json:"capabilities"`
	Canonicalization string `json:"canonicalization"`
	EventEnvelope    string `json:"eventEnvelope"`
	AsyncAPIProfile  string `json:"asyncapiProfile"`
	Exporter         string `json:"exporter"`
}

type Identity struct {
	Kind             string `json:"kind"`
	NaatreID         string `json:"naatreId"`
	Revision         string `json:"revision"`
	Schema           string `json:"schemaRevision"`
	Capability       string `json:"capabilityRevision"`
	Canonicalization string `json:"canonicalizationRevision"`
	EventEnvelope    string `json:"eventEnvelopeRevision"`
}

type Server struct {
	ID       string
	NaatreID string
	Revision string
	URL      string
	Protocol string
}

type Binding struct {
	ID                string
	NaatreID          string
	Revision          string
	Transport         Transport
	Server            string
	Implemented       bool
	Evidence          []string
	WireCompatibility bool
}

type Correlation struct {
	ID       string `json:"id"`
	Location string `json:"location"`
	Lifetime string `json:"lifetime"`
	Trust    string `json:"trust"`
}

type Message struct {
	ID           string
	NaatreID     string
	Revision     string
	PayloadType  schema.TypeID
	ContentType  string
	Correlations []Correlation
	Examples     []json.RawMessage
	Protected    bool
}

type Semantics struct {
	Ordering          string `json:"ordering"`
	Replay            string `json:"replay"`
	Terminal          string `json:"terminal"`
	Errors            string `json:"errors"`
	WireCompatibility bool   `json:"asyncapiWireCompatibility"`
}

type Channel struct {
	ID        string
	NaatreID  string
	Revision  string
	Address   string
	Messages  []string
	Bindings  []string
	Semantics Semantics
}

type Operation struct {
	ID       string
	NaatreID string
	Revision string
	Action   Action
	Channel  string
	Messages []string
	Security []string
}

type SecurityScheme struct {
	ID       string
	NaatreID string
	Revision string
	Type     string
	Scheme   string
	Name     string
	In       string
}

type Model struct {
	ID                    string
	Title                 string
	Version               string
	Schema                schema.Document
	CapabilityRevision    string
	EventEnvelopeRevision string
	Revisions             Revisions
	Servers               []Server
	Bindings              []Binding
	Messages              []Message
	Channels              []Channel
	Operations            []Operation
	Security              []SecurityScheme
	canonical             []byte
}

type IdentityLink struct {
	AsyncAPIKind string `json:"asyncapiKind"`
	AsyncAPIID   string `json:"asyncapiId"`
	NaatreID     string `json:"naatreId"`
	Revision     string `json:"revision"`
}

type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Feature string `json:"feature,omitempty"`
	Message string `json:"message"`
}

type FidelityReport struct {
	Profile         string                   `json:"profile"`
	ExporterVersion string                   `json:"exporterVersion"`
	Specification   string                   `json:"specification"`
	Direction       interopadapter.Direction `json:"direction"`
	Status          string                   `json:"status"`
	Revisions       Revisions                `json:"revisions"`
	Mappings        []interopadapter.Mapping `json:"mappings"`
	Links           []IdentityLink           `json:"links"`
	Diagnostics     []Diagnostic             `json:"diagnostics"`
}

type ImportOptions struct {
	Limits            Limits
	AllowedServerURLs []string
}

type Error struct {
	code  string
	cause error
}

func (e *Error) Error() string      { return "asyncapi: " + e.code }
func (e *Error) Unwrap() error      { return e.cause }
func (e *Error) PublicCode() string { return e.code }

type Change struct {
	Domain         string                      `json:"domain"`
	ID             string                      `json:"id"`
	Path           string                      `json:"path"`
	Classification schema.ChangeClassification `json:"classification"`
}

type CompatibilityReport struct {
	BeforeRevision string                      `json:"beforeRevision"`
	AfterRevision  string                      `json:"afterRevision"`
	Classification schema.ChangeClassification `json:"classification,omitempty"`
	Changes        []Change                    `json:"changes"`
}

type InspectionView struct {
	Profile              string   `json:"profile"`
	ModelDigest          string   `json:"modelDigest"`
	SchemaRevision       string   `json:"schemaRevision"`
	Channels             []string `json:"channels"`
	Messages             []string `json:"messages"`
	Operations           []string `json:"operations"`
	BusinessHandlerCalls int      `json:"businessHandlerCalls"`
}

type DocumentationView struct {
	Profile              string   `json:"profile"`
	ModelDigest          string   `json:"modelDigest"`
	Summary              []string `json:"summary"`
	BusinessHandlerCalls int      `json:"businessHandlerCalls"`
}

type MockView struct {
	Profile              string   `json:"profile"`
	ModelDigest          string   `json:"modelDigest"`
	Seed                 uint64   `json:"seed"`
	Frames               []string `json:"frames"`
	Evidence             string   `json:"evidence"`
	BusinessHandlerCalls int      `json:"businessHandlerCalls"`
}
