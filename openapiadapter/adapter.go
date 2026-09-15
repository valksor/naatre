// Package openapiadapter implements the bounded OpenAPI 3.2.0 integration for
// the protocol-neutral core.adapters-1 contract.
//
// Imported descriptions are untrusted. Only explicitly bound operations enter
// the Naatre runtime, and HTTP methods or OpenAPI extensions never supply
// effect, authorization, retry, cache, cost, batching, or transaction policy.
package openapiadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const (
	Profile       = "adapter.openapi-1"
	Specification = interopadapter.OpenAPISpecification
	Version       = "3.2.0"
	JSONDialect   = schema.JSONSchema202012
)

// Limits bounds every attacker-controlled document and runtime dimension.
type Limits struct {
	MaxDocumentBytes    int
	MaxOperations       int
	MaxSchemas          int
	MaxProperties       int
	MaxDepth            int
	MaxFanOut           int
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	MaxProjectionFields int
}

// Binding is application approval and policy for one operationId. Presence in
// ImportConfig.Bindings is the allowlist; unbound document operations are not
// registered. Invoker may be omitted only when HTTP is configured.
type Binding struct {
	Name     string
	Kind     protocol.OperationKind
	Metadata runtime.Metadata
	Invoker  interopadapter.Invoker
}

// CredentialProvider returns outbound metadata. Only exact names listed in
// HTTPConfig.CredentialHeaders are copied to a request.
type CredentialProvider func(context.Context) http.Header

// HTTPConfig configures the concrete runtime-consume transport. BaseURL and
// every allowed origin are startup validated; redirects are always denied.
type HTTPConfig struct {
	BaseURL             string
	AllowedOrigins      []string
	CredentialHeaders   []string
	Credentials         CredentialProvider
	Client              *http.Client
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	MaxProjectionFields int
}

// ImportConfig imports a strict JSON OpenAPI document and compiles approved
// operations against the existing interopadapter authority.
type ImportConfig struct {
	Document       []byte
	AdapterID      string
	SchemaIdentity string
	WireVersion    string
	Bindings       map[string]Binding
	HTTP           *HTTPConfig
	Limits         Limits
}

// SecurityRequirement names one configured OpenAPI security scheme.
type SecurityRequirement struct {
	Scheme string
}

// SecurityScheme is the supported, credential-free portion of an OpenAPI
// security scheme. Secrets and credential values never enter exported schema.
type SecurityScheme struct {
	Type   string
	Scheme string
	Name   string
	In     string
}

// ExportBinding maps one Naatre operation to one explicit HTTP route.
type ExportBinding struct {
	Method         string
	Path           string
	SuccessStatus  string
	FieldMaskQuery string
	Security       []SecurityRequirement
}

// ExportConfig exports explicitly bound operations from the portable Naatre
// schema authority. Route, status, and security semantics are never inferred.
type ExportConfig struct {
	Document        schema.Document
	AdapterID       string
	WireVersion     string
	Title           string
	Version         string
	Bindings        map[string]ExportBinding
	SecuritySchemes map[string]SecurityScheme
	Limits          Limits
}

// Compiled is an approved imported runtime adapter and its immutable types.
type Compiled struct {
	core   *interopadapter.Compiled
	types  schema.Snapshot
	report interopadapter.FidelityReport
}

// Error is the stable public error surface. Its text never includes the
// protected cause, document contents, credentials, endpoints, or HTTP bodies.
type Error struct {
	code string
}

func (e *Error) Error() string      { return "openapi adapter: " + e.code }
func (e *Error) PublicCode() string { return e.code }

// Types returns the immutable schema imported for approved operations.
func (c *Compiled) Types() schema.Snapshot {
	if c == nil {
		return schema.Snapshot{}
	}
	return c.types
}

// Register installs only the preflight-approved root operations.
func (c *Compiled) Register(registry *runtime.Registry) error {
	if c == nil || c.core == nil {
		return publicError("OPENAPI_NOT_COMPILED", errors.New("compiled adapter is unavailable"))
	}
	return c.core.Register(registry)
}

// FidelityReport returns a detached direction-specific report.
func (c *Compiled) FidelityReport() interopadapter.FidelityReport {
	if c == nil {
		return interopadapter.FidelityReport{}
	}
	report := c.report
	report.Mappings = slices.Clone(c.report.Mappings)
	report.Operations = slices.Clone(c.report.Operations)
	report.Diagnostics = slices.Clone(c.report.Diagnostics)
	return report
}

// MarshalFidelityReport returns canonical machine-readable evidence.
func (c *Compiled) MarshalFidelityReport() ([]byte, error) {
	if c == nil {
		return nil, publicError("OPENAPI_NOT_COMPILED", errors.New("compiled adapter is unavailable"))
	}
	return interopadapter.MarshalFidelityReport(c.report)
}

func publicError(code string, _ error) *Error { return &Error{code: code} }

func rejectedReport(config ImportConfig, diagnostic interopadapter.Diagnostic) interopadapter.FidelityReport {
	return interopadapter.FidelityReport{
		Profile: Profile, AdapterID: config.AdapterID, Protocol: interopadapter.OpenAPI,
		Specification: Specification, Direction: interopadapter.RuntimeConsume,
		SchemaIdentity: config.SchemaIdentity, WireVersion: config.WireVersion, Status: "rejected",
		Mappings: openAPIMappings(), Operations: []interopadapter.OperationReport{},
		Diagnostics: []interopadapter.Diagnostic{diagnostic},
	}
}

func openAPIMappings() []interopadapter.Mapping {
	return []interopadapter.Mapping{
		{Feature: "authentication", Classification: interopadapter.ApplicationSupplied, Resolution: "exact configured credential header allowlist"},
		{Feature: "batching", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre batching policy"},
		{Feature: "cancellation", Classification: interopadapter.ExplicitlyAdapted, Resolution: "caller context cancels the outbound request"},
		{Feature: "cost", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre cost"},
		{Feature: "deadlines", Classification: interopadapter.ExplicitlyAdapted, Resolution: "caller deadline is propagated without extension"},
		{Feature: "error-paths", Classification: interopadapter.ExplicitlyAdapted, Resolution: "status and bounded problem responses become safe adapter codes"},
		{Feature: "field-masks-projections", Classification: interopadapter.ExplicitlyAdapted, Resolution: "sorted unique selections enter only a declared query parameter"},
		{Feature: "idempotency", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre idempotency policy"},
		{Feature: "metadata", Classification: interopadapter.ApplicationSupplied, Resolution: "exact configured outbound header allowlist"},
		{Feature: "nullable-optional", Classification: interopadapter.Lossless},
		{Feature: "oneof-unions", Classification: interopadapter.ExplicitlyAdapted, Resolution: "documents using polymorphism are rejected before registration"},
		{Feature: "pagination", Classification: interopadapter.ApplicationSupplied, Resolution: "declared operation input mapping"},
		{Feature: "partial-failure", Classification: interopadapter.ExplicitlyAdapted, Resolution: "independent Naatre root outcomes preserve successful siblings"},
		{Feature: "redirects-egress", Classification: interopadapter.ExplicitlyAdapted, Resolution: "exact origins and deny-by-default redirects"},
		{Feature: "scalar-ranges", Classification: interopadapter.ExplicitlyAdapted, Resolution: "int64 and uint64 lexical conversion never uses binary floating point"},
		{Feature: "streaming", Classification: interopadapter.ExplicitlyAdapted, Resolution: "this unary profile rejects streaming media before registration"},
		{Feature: "transactions", Classification: interopadapter.ApplicationSupplied, Resolution: "declared Naatre transaction participation"},
	}
}

func canonical(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return protocol.CanonicalizeJSON(raw, protocol.Limits{MaxBytes: 1 << 20})
}
