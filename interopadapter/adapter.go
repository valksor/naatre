// Package interopadapter compiles explicitly approved imported-service
// operations into ordinary Naatre runtime definitions.
//
// It is the protocol-neutral core of the core.adapters-1 profile. Concrete
// OpenAPI, GraphQL, OpenRPC, JSON-RPC, protobuf, gRPC, and Connect parsers and
// transports are separate optional integrations. Imported descriptions are
// untrusted and never infer effect, retry, cache, transaction, or cost policy.
package interopadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const Profile = "core.adapters-1"

type Protocol string

const (
	OpenAPI  Protocol = "openapi"
	GraphQL  Protocol = "graphql"
	OpenRPC  Protocol = "openrpc-jsonrpc"
	Protobuf Protocol = "protobuf-grpc-connect"
)

const (
	OpenAPISpecification  = "https://spec.openapis.org/oas/v3.2.0"
	GraphQLSpecification  = "https://spec.graphql.org/September2025/"
	OpenRPCSpecification  = "https://github.com/open-rpc/spec/releases/tag/v1.4.1"
	ProtobufSpecification = "protobuf-edition-2024+connect-fac060371d74da4205f28ef504d078d2d2ce286f"
)

type Direction string

const (
	SchemaImport   Direction = "schema-import"
	SchemaExport   Direction = "schema-export"
	RuntimeConsume Direction = "runtime-consume"
	RuntimeExpose  Direction = "runtime-expose"
)

type Classification string

const (
	Lossless            Classification = "lossless"
	ExplicitlyAdapted   Classification = "explicitly-adapted"
	Unsupported         Classification = "unsupported"
	ApplicationSupplied Classification = "application-supplied"
)

var (
	identityPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	featurePattern  = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,127}$`)
)

var requiredFeatures = []string{
	"authentication", "batching", "cancellation", "cost", "deadlines",
	"error-paths", "field-masks-projections", "idempotency", "metadata",
	"nullable-optional", "oneof-unions", "pagination", "partial-failure",
	"redirects-egress", "scalar-ranges", "streaming", "transactions",
}

type Mapping struct {
	Feature        string         `json:"feature"`
	Classification Classification `json:"classification"`
	Resolution     string         `json:"resolution,omitempty"`
}

type Diagnostic struct {
	Code      string `json:"code"`
	Operation string `json:"operation,omitempty"`
	Feature   string `json:"feature,omitempty"`
	Message   string `json:"message"`
}

type PolicyClaims struct {
	Effect      runtime.Effect                   `json:"effect"`
	Idempotency runtime.IdempotencyPolicy        `json:"idempotency"`
	Cost        uint64                           `json:"cost"`
	RetrySafe   bool                             `json:"retrySafe"`
	Cacheable   bool                             `json:"cacheable"`
	Batching    runtime.Batching                 `json:"batching"`
	Transaction runtime.TransactionParticipation `json:"transaction"`
}

type OperationReport struct {
	ExternalName string       `json:"externalName"`
	NaatreName   string       `json:"naatreName"`
	Approved     bool         `json:"approved"`
	Policy       PolicyClaims `json:"policy"`
}

type FidelityReport struct {
	Profile        string            `json:"profile"`
	AdapterID      string            `json:"adapterId"`
	Protocol       Protocol          `json:"protocol"`
	Specification  string            `json:"specification"`
	Direction      Direction         `json:"direction"`
	SchemaIdentity string            `json:"schemaIdentity"`
	WireVersion    string            `json:"wireVersion"`
	Status         string            `json:"status"`
	Mappings       []Mapping         `json:"mappings"`
	Operations     []OperationReport `json:"operations"`
	Diagnostics    []Diagnostic      `json:"diagnostics"`
}

type BackendRequest struct {
	Operation  string          `json:"operation"`
	Input      json.RawMessage `json:"input,omitempty"`
	Projection []string        `json:"projection"`
}

type Invoker interface {
	Invoke(context.Context, BackendRequest) (map[string]any, error)
}

type InvokerFunc func(context.Context, BackendRequest) (map[string]any, error)

func (f InvokerFunc) Invoke(ctx context.Context, request BackendRequest) (map[string]any, error) {
	return f(ctx, request)
}

type Operation struct {
	ExternalName string
	Approved     bool
	Descriptor   runtime.Descriptor
	Invoker      Invoker
}

type Config struct {
	AdapterID      string
	Protocol       Protocol
	Specification  string
	Direction      Direction
	SchemaIdentity string
	WireVersion    string
	Types          schema.Snapshot
	Mappings       []Mapping
	Operations     []Operation
	MaxOperations  int
	MaxFanOut      int
}

type Compiled struct {
	definitions []runtime.Definition
	report      FidelityReport
}

type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return "interop adapter: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

func Compile(config Config) (*Compiled, FidelityReport, error) {
	report := FidelityReport{
		Profile: Profile, AdapterID: config.AdapterID, Protocol: config.Protocol,
		Specification: config.Specification, Direction: config.Direction,
		SchemaIdentity: config.SchemaIdentity, WireVersion: config.WireVersion,
		Status: "rejected", Mappings: normalizeMappings(config.Mappings),
		Operations: []OperationReport{}, Diagnostics: []Diagnostic{},
	}
	validateConfig(config, &report)
	definitions := make([]runtime.Definition, 0, len(config.Operations))
	validator := runtime.NewRegistry(config.Types)
	seenExternal := make(map[string]bool, len(config.Operations))
	for _, operation := range config.Operations {
		metadata := operation.Descriptor.Metadata
		report.Operations = append(report.Operations, OperationReport{
			ExternalName: operation.ExternalName, NaatreName: operation.Descriptor.Name,
			Approved: operation.Approved,
			Policy: PolicyClaims{
				Effect: metadata.Effect, Idempotency: metadata.Idempotency, Cost: metadata.Cost,
				RetrySafe: metadata.RetrySafe, Cacheable: metadata.Cacheable,
				Batching: metadata.Batching, Transaction: metadata.Transaction,
			},
		})
		validateOperation(operation, seenExternal, &report)
		if operation.Invoker == nil || !operation.Approved || operation.ExternalName == "" {
			continue
		}
		current := operation
		definition := runtime.BindInvocation[map[string]any](current.Descriptor, func(ctx context.Context, invocation runtime.Invocation) (map[string]any, error) {
			projection, err := Projection(invocation.Selected.Selections(), config.MaxFanOut)
			if err != nil {
				return nil, err
			}
			var input json.RawMessage
			if !invocation.Input.IsMissing() {
				input, err = invocation.Input.MarshalJSON()
				if err != nil {
					return nil, adapterError("ADAPTER_INPUT_INVALID", err)
				}
			}
			return current.Invoker.Invoke(ctx, BackendRequest{
				Operation: current.ExternalName, Input: input, Projection: projection,
			})
		})
		if err := validator.Register(definition); err != nil {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{
				Code: "ADAPTER_REGISTRATION_INVALID", Operation: operation.ExternalName,
				Message: "approved operation is not a valid Naatre registration",
			})
			continue
		}
		definitions = append(definitions, definition)
	}
	sort.Slice(report.Operations, func(i, j int) bool {
		left := report.Operations[i].ExternalName + "\x00" + report.Operations[i].NaatreName
		right := report.Operations[j].ExternalName + "\x00" + report.Operations[j].NaatreName
		return left < right
	})
	sortDiagnostics(report.Diagnostics)
	if len(report.Diagnostics) != 0 {
		return nil, report, adapterError(report.Diagnostics[0].Code, errors.New("adapter configuration rejected"))
	}
	report.Status = "ready"
	compiled := &Compiled{definitions: definitions, report: report}
	return compiled, compiled.FidelityReport(), nil
}

func (c *Compiled) Register(registry *runtime.Registry) error {
	if c == nil {
		return adapterError("ADAPTER_NOT_COMPILED", errors.New("compiled adapter is nil"))
	}
	if registry == nil {
		return adapterError("ADAPTER_REGISTRY_REQUIRED", errors.New("runtime registry is nil"))
	}
	for _, definition := range c.definitions {
		if err := registry.Register(definition); err != nil {
			return adapterError("ADAPTER_REGISTRATION_FAILED", err)
		}
	}
	return nil
}

func (c *Compiled) FidelityReport() FidelityReport {
	if c == nil {
		return FidelityReport{}
	}
	result := c.report
	result.Mappings = slices.Clone(c.report.Mappings)
	result.Operations = slices.Clone(c.report.Operations)
	result.Diagnostics = slices.Clone(c.report.Diagnostics)
	return result
}

func (c *Compiled) MarshalFidelityReport() ([]byte, error) {
	if c == nil {
		return nil, adapterError("ADAPTER_NOT_COMPILED", errors.New("compiled adapter is nil"))
	}
	return MarshalFidelityReport(c.report)
}

func MarshalFidelityReport(report FidelityReport) ([]byte, error) {
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, adapterError("ADAPTER_REPORT_INVALID", err)
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return nil, adapterError("ADAPTER_REPORT_INVALID", err)
	}
	return canonical, nil
}

func Projection(selections []protocol.Selection, maximum int) ([]string, error) {
	if maximum < 1 {
		return nil, adapterError("ADAPTER_FANOUT_LIMIT", errors.New("maximum fan-out must be positive"))
	}
	paths := make([]string, 0, len(selections))
	nodes := 0
	var visit func([]protocol.Selection, []string) error
	visit = func(values []protocol.Selection, prefix []string) error {
		for _, selection := range values {
			nodes++
			if nodes > maximum {
				return adapterError("ADAPTER_FANOUT_LIMIT", errors.New("selection fan-out exceeds configured maximum"))
			}
			if selection.Kind() != protocol.FieldSelection && selection.Kind() != protocol.CallSelection {
				return adapterError("ADAPTER_SELECTION_UNSUPPORTED", fmt.Errorf("selection kind %s requires a protocol-specific adapter", selection.Kind()))
			}
			path := append(slices.Clone(prefix), selection.Name())
			children := selection.Selections()
			if len(children) == 0 {
				paths = append(paths, strings.Join(path, "."))
				continue
			}
			if err := visit(children, path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(selections, nil); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	paths = slices.Compact(paths)
	return paths, nil
}

func validateConfig(config Config, report *FidelityReport) {
	if !identityPattern.MatchString(config.AdapterID) || !identityPattern.MatchString(config.SchemaIdentity) || !identityPattern.MatchString(config.WireVersion) {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_IDENTITY_INVALID", Message: "adapter, schema, and wire identities must be explicit bounded identifiers"})
	}
	if config.SchemaIdentity == config.WireVersion {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_IDENTITY_COUPLED", Message: "schema identity and wire version must be independent"})
	}
	expected := expectedSpecification(config.Protocol)
	if expected == "" || expected != config.Specification {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_SPECIFICATION_UNSUPPORTED", Message: "protocol specification is not the pinned profile version"})
	}
	if config.Direction != RuntimeConsume {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_DIRECTION_UNSUPPORTED", Message: "the Go reference compiler implements runtime-consume only"})
	}
	if config.MaxOperations < 1 || config.MaxOperations > 1024 || len(config.Operations) > config.MaxOperations {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_OPERATION_LIMIT", Message: "approved operation count exceeds the configured bound"})
	}
	if len(config.Operations) == 0 {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_OPERATION_REQUIRED", Message: "runtime-consume requires at least one approved operation"})
	}
	if config.MaxFanOut < 1 || config.MaxFanOut > 4096 {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_FANOUT_LIMIT", Message: "fan-out bound must be between one and 4096"})
	}
	validateMappings(report)
}

func validateMappings(report *FidelityReport) {
	seen := make(map[string]bool, len(report.Mappings))
	for _, mapping := range report.Mappings {
		if !featurePattern.MatchString(mapping.Feature) || seen[mapping.Feature] {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_MAPPING_INVALID", Feature: mapping.Feature, Message: "mapping feature is invalid or duplicated"})
			continue
		}
		seen[mapping.Feature] = true
		switch mapping.Classification {
		case Lossless:
			if mapping.Resolution != "" {
				report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_MAPPING_INVALID", Feature: mapping.Feature, Message: "lossless mappings cannot claim an adaptation"})
			}
		case ExplicitlyAdapted, ApplicationSupplied:
			if mapping.Resolution == "" {
				report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_MAPPING_UNRESOLVED", Feature: mapping.Feature, Message: "adapted and application-supplied mappings require an explicit resolution"})
			}
		case Unsupported:
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_MAPPING_UNSUPPORTED", Feature: mapping.Feature, Message: "unsupported mapping blocks deployment"})
		default:
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_MAPPING_INVALID", Feature: mapping.Feature, Message: "mapping classification is invalid"})
		}
	}
	for _, feature := range requiredFeatures {
		if !seen[feature] {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_MAPPING_MISSING", Feature: feature, Message: "required fidelity feature is absent"})
		}
	}
}

func validateOperation(operation Operation, seen map[string]bool, report *FidelityReport) {
	if !operation.Approved {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_OPERATION_UNAPPROVED", Operation: operation.ExternalName, Message: "imported operation is not explicitly approved"})
	}
	if !identityPattern.MatchString(operation.ExternalName) || seen[operation.ExternalName] {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_OPERATION_INVALID", Operation: operation.ExternalName, Message: "external operation identity is invalid or duplicated"})
	}
	seen[operation.ExternalName] = true
	if operation.Invoker == nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_INVOKER_REQUIRED", Operation: operation.ExternalName, Message: "approved operation requires an invoker"})
	}
	metadata := operation.Descriptor.Metadata
	if metadata.Effect == "" {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_POLICY_REQUIRED", Operation: operation.ExternalName, Feature: "effect", Message: "application effect policy is required"})
	}
	if metadata.Idempotency == "" {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_POLICY_REQUIRED", Operation: operation.ExternalName, Feature: "idempotency", Message: "application idempotency policy is required"})
	}
	if metadata.Cost == 0 {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_POLICY_REQUIRED", Operation: operation.ExternalName, Feature: "cost", Message: "application cost policy is required"})
	}
	if metadata.AuthorizationPolicy == "" {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "ADAPTER_POLICY_REQUIRED", Operation: operation.ExternalName, Feature: "authentication", Message: "application authorization policy is required"})
	}
}

func normalizeMappings(input []Mapping) []Mapping {
	result := slices.Clone(input)
	sort.Slice(result, func(i, j int) bool {
		left := result[i].Feature + "\x00" + string(result[i].Classification) + "\x00" + result[i].Resolution
		right := result[j].Feature + "\x00" + string(result[j].Classification) + "\x00" + result[j].Resolution
		return left < right
	})
	return result
}

func sortDiagnostics(values []Diagnostic) {
	sort.Slice(values, func(i, j int) bool {
		left := values[i].Code + "\x00" + values[i].Operation + "\x00" + values[i].Feature
		right := values[j].Code + "\x00" + values[j].Operation + "\x00" + values[j].Feature
		return left < right
	})
}

func expectedSpecification(protocolName Protocol) string {
	switch protocolName {
	case OpenAPI:
		return OpenAPISpecification
	case GraphQL:
		return GraphQLSpecification
	case OpenRPC:
		return OpenRPCSpecification
	case Protobuf:
		return ProtobufSpecification
	default:
		return ""
	}
}

func adapterError(code string, cause error) *Error { return &Error{Code: code, cause: cause} }
