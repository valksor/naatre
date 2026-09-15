// Package graphqladapter maps the supported GraphQL September 2025 subset to
// the existing Naatre schema, protocol, and runtime contracts.
//
// The package is an optional integration for core.adapters-1. It is not a
// second schema authority: every successful import produces a schema.Document
// and every runtime registration is an ordinary explicit runtime definition.
package graphqladapter

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const (
	Profile       = "core.adapters.graphql-1"
	Specification = interopadapter.GraphQLSpecification
)

const (
	CodeCancelled          = "GRAPHQL_CANCELLED"
	CodeDocumentInvalid    = "GRAPHQL_DOCUMENT_INVALID"
	CodeSchemaInvalid      = "GRAPHQL_SCHEMA_INVALID"
	CodeUnsupported        = "GRAPHQL_UNSUPPORTED"
	CodeResourceExhausted  = "GRAPHQL_RESOURCE_EXHAUSTED"
	CodePolicyRequired     = "GRAPHQL_POLICY_REQUIRED"
	CodeResolverRequired   = "GRAPHQL_RESOLVER_REQUIRED"
	CodeOperationUnknown   = "GRAPHQL_OPERATION_UNKNOWN"
	CodeResponseInvalid    = "GRAPHQL_RESPONSE_INVALID"
	CodeUpstreamFailed     = "GRAPHQL_UPSTREAM_FAILED"
	CodeExecutionFailed    = "GRAPHQL_EXECUTION_FAILED"
	CodeRegistrationFailed = "GRAPHQL_REGISTRATION_FAILED"
)

// Error exposes only a stable public code. Cause remains available to trusted
// in-process diagnostics through errors.Unwrap and is never serialized here.
type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return "graphql adapter: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

func adapterError(code string, cause error) *Error { return &Error{Code: code, cause: cause} }

// CodeOf returns a stable public code without exposing the wrapped cause.
func CodeOf(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return CodeRegistrationFailed
}

// Limits bounds all untrusted GraphQL documents and responses. Zero values
// select the conservative defaults returned by DefaultLimits.
type Limits struct {
	MaxDocumentBytes int
	MaxResponseBytes int
	MaxTokens        int
	MaxDepth         int
	MaxTypes         int
	MaxFields        int
	MaxOperations    int
	MaxErrors        int
}

// DefaultLimits returns the supported reference profile bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20,
		MaxTokens:        32 << 10,
		MaxDepth:         32,
		MaxTypes:         1024,
		MaxFields:        8192,
		MaxOperations:    256,
		MaxErrors:        128,
	}
}

func normalizeLimits(input Limits) (Limits, error) {
	defaults := DefaultLimits()
	values := []*int{
		&input.MaxDocumentBytes, &input.MaxResponseBytes, &input.MaxTokens,
		&input.MaxDepth, &input.MaxTypes, &input.MaxFields,
		&input.MaxOperations, &input.MaxErrors,
	}
	fallbacks := []int{
		defaults.MaxDocumentBytes, defaults.MaxResponseBytes, defaults.MaxTokens,
		defaults.MaxDepth, defaults.MaxTypes, defaults.MaxFields,
		defaults.MaxOperations, defaults.MaxErrors,
	}
	for index, value := range values {
		if *value == 0 {
			*value = fallbacks[index]
		}
		if *value < 0 {
			return Limits{}, adapterError(CodeResourceExhausted, errors.New("negative GraphQL resource limit"))
		}
	}
	if input.MaxDocumentBytes > 16<<20 || input.MaxResponseBytes > 16<<20 ||
		input.MaxTokens > 1<<20 || input.MaxDepth > 256 || input.MaxTypes > 16384 ||
		input.MaxFields > 131072 || input.MaxOperations > 4096 || input.MaxErrors > 4096 {
		return Limits{}, adapterError(CodeResourceExhausted, errors.New("GraphQL resource limit exceeds profile maximum"))
	}
	return input, nil
}

// Policy is application-owned Naatre metadata. No value is inferred from a
// GraphQL query, mutation, subscription, directive, or field name.
type Policy struct {
	Effect              runtime.Effect
	Idempotency         runtime.IdempotencyPolicy
	Cost                uint64
	RetrySafe           bool
	Cacheable           bool
	Deterministic       bool
	ThreadSafety        runtime.ThreadSafety
	Batching            runtime.Batching
	Transaction         runtime.TransactionParticipation
	AuthorizationPolicy string
}

func (p Policy) metadata() runtime.Metadata {
	return runtime.Metadata{
		Effect: p.Effect, Idempotency: p.Idempotency, Cost: p.Cost,
		RetrySafe: p.RetrySafe, Cacheable: p.Cacheable,
		Deterministic: p.Deterministic, ThreadSafety: p.ThreadSafety,
		Batching: p.Batching, Transaction: p.Transaction,
		AuthorizationPolicy: p.AuthorizationPolicy,
	}
}

func (p Policy) valid() bool {
	return p.Effect != "" && p.Idempotency != "" && p.Cost > 0 &&
		p.ThreadSafety != "" && p.Batching != "" && p.Transaction != "" &&
		p.AuthorizationPolicy != ""
}

// ImportConfig pins identities and supplies every application policy and
// custom scalar mapping needed by a schema import.
type ImportConfig struct {
	AdapterID      string
	SchemaIdentity string
	WireVersion    string
	Revision       string
	Limits         Limits
	MaxTypeDepth   int
	Policies       map[string]Policy
	ScalarMappings map[string]schema.TypeID
}

// ExportConfig pins the report identities for one schema-export direction.
type ExportConfig struct {
	AdapterID      string
	SchemaIdentity string
	WireVersion    string
	Limits         Limits
}

// ImportedSchema is an immutable mapping backed by a normative Naatre schema
// document. Its private GraphQL model is used only to reproduce wire syntax and
// non-null behavior.
type ImportedSchema struct {
	document       schema.Document
	model          *schemaModel
	typeIDs        map[string]schema.TypeID
	typeNames      map[schema.TypeID]string
	typeRefs       map[string]schema.TypeID
	operations     map[string]runtime.Descriptor
	adapterID      string
	schemaIdentity string
	wireVersion    string
}

// Document returns the immutable Naatre schema document.
func (s ImportedSchema) Document() schema.Document { return s.document }

// Operations returns detached descriptors keyed by GraphQL root field name.
func (s ImportedSchema) Operations() map[string]runtime.Descriptor {
	result := make(map[string]runtime.Descriptor, len(s.operations))
	for name, descriptor := range s.operations {
		descriptor.Capabilities = slices.Clone(descriptor.Capabilities)
		result[name] = descriptor
	}
	return result
}

// Request is the transport-neutral GraphQL request emitted for an explicitly
// registered imported resolver. Authentication metadata is intentionally not
// present; a concrete transport owns an exact credential allowlist.
type Request struct {
	Document      string          `json:"query"`
	OperationName string          `json:"operationName"`
	Variables     json.RawMessage `json:"variables,omitempty"`
}

// ResponseError is the untrusted GraphQL error shape accepted from a client.
// Message and Extensions are never copied into a public Naatre failure.
type ResponseError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

// Response is a GraphQL response. Data is any because GraphQL permits a null
// top-level data value after non-null propagation.
type Response struct {
	Data   any             `json:"data"`
	Errors []ResponseError `json:"errors,omitempty"`
}

// PublicError is the bounded, sanitized error visible to an explicit response
// adapter. It contains no upstream message, extensions, endpoint, or cause.
type PublicError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

// MappedResponse preserves partial data separately from safe errors. Adapt is
// mandatory at the resolver boundary so neither side can silently discard the
// semantic difference.
type MappedResponse struct {
	Data     any           `json:"data"`
	Errors   []PublicError `json:"errors"`
	Complete bool          `json:"complete"`
}

// Client performs GraphQL I/O. Implementations own transport, origin,
// credential, redirect, and deadline policy; the adapter propagates ctx.
type Client interface {
	Execute(context.Context, Request) (Response, error)
}

type ClientFunc func(context.Context, Request) (Response, error)

func (f ClientFunc) Execute(ctx context.Context, request Request) (Response, error) {
	return f(ctx, request)
}

// Resolver is one explicit runtime-consume registration. Adapt must decide how
// GraphQL partial data and errors become the declared Naatre output.
type Resolver struct {
	Operation string
	Approved  bool
	Adapt     func(context.Context, MappedResponse) (map[string]any, error)
}

// ConsumerConfig compiles imported GraphQL operations into ordinary runtime
// definitions through the protocol-neutral adapter core.
type ConsumerConfig struct {
	Schema        ImportedSchema
	Client        Client
	Resolvers     []Resolver
	Limits        Limits
	MaxFanOut     int
	MaxOperations int
}

// CompiledConsumer retains the core registration and fidelity behavior.
type CompiledConsumer struct {
	core   *interopadapter.Compiled
	report interopadapter.FidelityReport
}

func (c *CompiledConsumer) Register(registry *runtime.Registry) error {
	if c == nil || c.core == nil {
		return adapterError(CodeRegistrationFailed, errors.New("GraphQL consumer is not compiled"))
	}
	if err := c.core.Register(registry); err != nil {
		return adapterError(CodeRegistrationFailed, err)
	}
	return nil
}

func (c *CompiledConsumer) FidelityReport() interopadapter.FidelityReport {
	if c == nil || c.core == nil {
		return interopadapter.FidelityReport{}
	}
	result := c.report
	result.Mappings = slices.Clone(result.Mappings)
	result.Operations = slices.Clone(result.Operations)
	result.Diagnostics = slices.Clone(result.Diagnostics)
	return result
}

// Executor is the runtime-expose boundary. A runtime.Registry-backed executor
// retains Naatre validation, authorization, completion, and error ownership.
type Executor func(context.Context, *protocol.Request) runtime.Outcome

// Subscriber starts a Naatre subscription and returns ordered outcomes. The
// returned stream must close when ctx is cancelled.
type Subscriber func(context.Context, *protocol.Request) (<-chan runtime.Outcome, error)

// RuntimeConfig wires GraphQL documents to an existing Naatre execution path.
type RuntimeConfig struct {
	Schema    ImportedSchema
	Execute   Executor
	Subscribe Subscriber
	Limits    Limits
}

// Runtime exposes the supported in-process GraphQL execution profile. HTTP,
// WebSocket, multipart, SSE, and authentication transports are not included.
type Runtime struct {
	schema    ImportedSchema
	execute   Executor
	subscribe Subscriber
	limits    Limits
}

func newReport(adapterID, schemaIdentity, wireVersion string, direction interopadapter.Direction) interopadapter.FidelityReport {
	return interopadapter.FidelityReport{
		Profile: Profile, AdapterID: adapterID, Protocol: interopadapter.GraphQL,
		Specification: Specification, Direction: direction,
		SchemaIdentity: schemaIdentity, WireVersion: wireVersion,
		Status: "rejected", Mappings: directionMappings(direction),
		Operations: []interopadapter.OperationReport{}, Diagnostics: []interopadapter.Diagnostic{},
	}
}

func rejectReport(report *interopadapter.FidelityReport, code, feature, message string) error {
	report.Status = "rejected"
	report.Diagnostics = append(report.Diagnostics, interopadapter.Diagnostic{
		Code: code, Feature: feature, Message: message,
	})
	return adapterError(code, errors.New(message))
}

func directionMappings(_ interopadapter.Direction) []interopadapter.Mapping {
	all := []interopadapter.Mapping{
		{Feature: "scalar-ranges", Classification: interopadapter.ExplicitlyAdapted, Resolution: "GraphQL Int is Int32; custom scalars require an explicit Naatre mapping"},
		{Feature: "nullable-optional", Classification: interopadapter.ExplicitlyAdapted, Resolution: "GraphQL omission and explicit null remain distinct Naatre input states"},
		{Feature: "oneof-unions", Classification: interopadapter.ExplicitlyAdapted, Resolution: "GraphQL output unions retain explicit Naatre variants"},
		{Feature: "field-masks-projections", Classification: interopadapter.Lossless},
		{Feature: "error-paths", Classification: interopadapter.ExplicitlyAdapted, Resolution: "only bounded paths and stable public codes cross the adapter"},
		{Feature: "partial-failure", Classification: interopadapter.ExplicitlyAdapted, Resolution: "partial data is carried separately from safe errors"},
		{Feature: "streaming", Classification: interopadapter.ExplicitlyAdapted, Resolution: "subscriptions require the separate in-process subscriber boundary"},
	}
	all = append(all,
		interopadapter.Mapping{Feature: "authentication", Classification: interopadapter.ApplicationSupplied, Resolution: "transport authenticates and Naatre registry authorization remains authoritative"},
		interopadapter.Mapping{Feature: "batching", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre batching policy"},
		interopadapter.Mapping{Feature: "cancellation", Classification: interopadapter.ExplicitlyAdapted, Resolution: "caller context cancels execution or subscription"},
		interopadapter.Mapping{Feature: "cost", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre cost"},
		interopadapter.Mapping{Feature: "deadlines", Classification: interopadapter.ExplicitlyAdapted, Resolution: "caller context deadline is propagated without extension"},
		interopadapter.Mapping{Feature: "idempotency", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre idempotency"},
		interopadapter.Mapping{Feature: "metadata", Classification: interopadapter.ApplicationSupplied, Resolution: "transport-specific exact allowlist"},
		interopadapter.Mapping{Feature: "pagination", Classification: interopadapter.ApplicationSupplied, Resolution: "declared per-field cursor mapping"},
		interopadapter.Mapping{Feature: "redirects-egress", Classification: interopadapter.ExplicitlyAdapted, Resolution: "concrete client must pin origins and reject redirects"},
		interopadapter.Mapping{Feature: "transactions", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre transaction participation"},
	)
	slices.SortFunc(all, func(left, right interopadapter.Mapping) int {
		return cmp.Compare(fmt.Sprintf("%s\x00%s", left.Feature, left.Classification), fmt.Sprintf("%s\x00%s", right.Feature, right.Classification))
	})
	return all
}
