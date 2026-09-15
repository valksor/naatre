package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

type Compiled struct {
	definitions []runtime.Definition
	report      FidelityReport
	manifest    Manifest
}

func Compile(config Config) (*Compiled, FidelityReport, error) {
	limits := withDefaultLimits(config.Limits)
	report := FidelityReport{
		Profile: Profile, AdapterID: config.AdapterID, Direction: config.Direction, Status: "rejected",
		Revisions: revisions(), Mappings: DefaultMappings(), Registrations: []RegistrationReport{},
		Diagnostics: []Diagnostic{}, Claims: []string{"bounded-registration-preflight", "lossless-structured-content-subset", "trusted-naatre-policy"},
		NonClaims: []string{"mcp-wire-compatibility", "naatre-wire-compatibility", "transport-runtime-implementation"},
	}
	validateConfig(config, limits, &report)
	validateCatalog(config.Catalog, limits, &report)
	transportReport, transportErr := DescribeTransport(config.Transport)
	if transportErr != nil {
		appendErrorDiagnostic(&report, transportErr, "", "transport")
	} else {
		report.Transport = transportReport
	}

	manifest, definitions := compileRegistrations(config, limits, &report)
	sort.Slice(report.Registrations, func(i, j int) bool { return report.Registrations[i].RemoteID < report.Registrations[j].RemoteID })
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].RemoteID < manifest.Entries[j].RemoteID })
	sortDiagnostics(report.Diagnostics)
	if len(report.Diagnostics) != 0 {
		return nil, report, adapterError(report.Diagnostics[0].Code, errors.New("MCP adapter configuration rejected"))
	}

	report.Status = "ready"
	compiled := &Compiled{definitions: definitions, report: cloneReport(report), manifest: cloneManifest(manifest)}
	return compiled, compiled.FidelityReport(), nil
}

func compileRegistrations(config Config, limits Limits, report *FidelityReport) (Manifest, []runtime.Definition) {
	manifest := Manifest{Profile: Profile, Direction: config.Direction, Revisions: revisions(), Transport: config.Transport.Kind, Endpoint: config.Transport.Endpoint, Entries: []ManifestEntry{}, WireClaims: []string{}}
	definitions := make([]runtime.Definition, 0, len(config.Registrations))
	validator := runtime.NewRegistry(config.Types)
	seen := make(map[string]bool, len(config.Registrations))
	for _, registration := range config.Registrations {
		validateRegistration(config, limits, registration, seen, report)
		policy := policyFromDescriptor(registration.Descriptor)
		report.Registrations = append(report.Registrations, RegistrationReport{Kind: registration.Kind, RemoteID: registration.RemoteID, NaatreName: registration.Descriptor.Name, Policy: policy})
		inputSchema, inputErr := ValidateSchema(registration.InputSchema, limits)
		outputSchema, outputErr := ValidateSchema(registration.OutputSchema, limits)
		if inputErr != nil || outputErr != nil || !registration.Approved {
			continue
		}
		manifest.Entries = append(manifest.Entries, ManifestEntry{Kind: registration.Kind, RemoteID: registration.RemoteID, NaatreName: registration.Descriptor.Name, InputSchema: inputSchema, OutputSchema: outputSchema, Policy: policy})
		definition, ok := consumedDefinition(config.Direction, registration, limits.MaxEntries)
		if !ok {
			continue
		}
		if err := validator.Register(definition); err != nil {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_RUNTIME_REGISTRATION_INVALID", RemoteID: registration.RemoteID, Message: "trusted entry is not a valid Naatre runtime definition"})
			continue
		}
		definitions = append(definitions, definition)
	}
	return manifest, definitions
}

func consumedDefinition(direction Direction, registration Registration, maxFanOut int) (runtime.Definition, bool) {
	if direction != Consume || registration.Invoker == nil {
		return runtime.Definition{}, false
	}
	definition := runtime.BindInvocation[map[string]any](registration.Descriptor, func(ctx context.Context, invocation runtime.Invocation) (map[string]any, error) {
		projection, err := interopadapter.Projection(invocation.Selected.Selections(), maxFanOut)
		if err != nil {
			return nil, adapterError("MCP_SELECTION_UNSUPPORTED", err)
		}
		var input json.RawMessage
		if !invocation.Input.IsMissing() {
			input, err = invocation.Input.MarshalJSON()
			if err != nil {
				return nil, adapterError("MCP_INPUT_INVALID", err)
			}
		}
		return registration.Invoker.Invoke(ctx, interopadapter.BackendRequest{Operation: registration.RemoteID, Input: input, Projection: projection})
	})
	return definition, true
}

func (c *Compiled) Register(registry *runtime.Registry) error {
	if c == nil {
		return adapterError("MCP_NOT_COMPILED", errors.New("compiled adapter is nil"))
	}
	if c.report.Direction != Consume {
		return adapterError("MCP_DIRECTION_UNSUPPORTED", errors.New("only consumed MCP entries register Naatre handlers"))
	}
	if registry == nil {
		return adapterError("MCP_REGISTRY_REQUIRED", errors.New("runtime registry is nil"))
	}
	for _, definition := range c.definitions {
		if err := registry.Register(definition); err != nil {
			return adapterError("MCP_REGISTRATION_FAILED", err)
		}
	}
	return nil
}

func (c *Compiled) FidelityReport() FidelityReport {
	if c == nil {
		return FidelityReport{}
	}
	return cloneReport(c.report)
}

func (c *Compiled) Manifest() Manifest {
	if c == nil {
		return Manifest{}
	}
	return cloneManifest(c.manifest)
}

func (c *Compiled) MarshalFidelityReport() ([]byte, error) {
	if c == nil {
		return nil, adapterError("MCP_NOT_COMPILED", errors.New("compiled adapter is nil"))
	}
	return marshalCanonical(c.report)
}

func (c *Compiled) MarshalManifest() ([]byte, error) {
	if c == nil {
		return nil, adapterError("MCP_NOT_COMPILED", errors.New("compiled adapter is nil"))
	}
	return marshalCanonical(c.manifest)
}

func DefaultMappings() []Mapping {
	return []Mapping{
		{Feature: "initialization", Classification: ExplicitlyAdapted, Required: true, Resolution: "exact 2025-11-25 negotiation before registration"},
		{Feature: "capabilities", Classification: ExplicitlyAdapted, Required: true, Resolution: "required capabilities are checked before invocation"},
		{Feature: "tools", Classification: ExplicitlyAdapted, Required: true, Resolution: "only exact trusted registrations are callable"},
		{Feature: "resources", Classification: ExplicitlyAdapted, Required: false, Resolution: "exact URI registrations only"},
		{Feature: "resource-templates", Classification: ExplicitlyAdapted, Required: false, Resolution: "exact trusted template registrations only"},
		{Feature: "prompts", Classification: ExplicitlyAdapted, Required: false, Resolution: "untrusted user-controlled data; never registration or policy"},
		{Feature: "progress", Classification: ExplicitlyAdapted, Required: false, Resolution: "session-bound tokens and finite monotonic events"},
		{Feature: "cancellation", Classification: ExplicitlyAdapted, Required: true, Resolution: "request-scoped context cancellation with deterministic outcome"},
		{Feature: "logging", Classification: ExplicitlyAdapted, Required: false, Resolution: "bounded untrusted data outside stdio stdout"},
		{Feature: "pagination", Classification: ExplicitlyAdapted, Required: false, Resolution: "opaque bounded cursors scoped to one listing"},
		{Feature: "structured-content", Classification: Lossless, Required: true},
		{Feature: "errors", Classification: ExplicitlyAdapted, Required: true, Resolution: "Naatre errors stay in structured content; MCP protocol errors stay transport-level"},
		{Feature: "transport-lifecycle", Classification: ExplicitlyAdapted, Required: true, Resolution: "stdio and Streamable HTTP publish separate claims"},
		{Feature: "missing-null", Classification: Lossless, Required: true},
		{Feature: "numeric", Classification: Lossless, Required: true},
		{Feature: "timestamp", Classification: Lossless, Required: true},
		{Feature: "closed-union", Classification: Lossless, Required: true},
		{Feature: "extended-scalars", Classification: ExplicitlyAdapted, Required: true, Resolution: "trusted string codecs identify exact scalar semantics"},
		{Feature: "open-variants", Classification: Unsupported, Required: false, Resolution: "registration fails when an operation requires an open variant"},
		{Feature: "partial-results", Classification: Lossless, Required: true},
		{Feature: "streams", Classification: Unsupported, Required: false, Resolution: "stream runtime integration belongs to issue 103"},
		{Feature: "files", Classification: Unsupported, Required: false, Resolution: "file content requires a separately registered bounded resource codec"},
		{Feature: "custom-constraints", Classification: ExplicitlyAdapted, Required: false, Resolution: "only the declared closed keyword subset is accepted"},
	}
}

func validateConfig(config Config, limits Limits, report *FidelityReport) {
	if !identityPattern.MatchString(config.AdapterID) {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_IDENTITY_INVALID", Message: "adapter identity must be a bounded identifier"})
	}
	if config.Direction != Consume && config.Direction != Expose {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_DIRECTION_UNSUPPORTED", Message: "adapter direction must be declared separately"})
	}
	if config.SchemaRevision != NaatreSchemaRevision || config.ProtocolRevision != NaatreProtocolRevision || config.ConformanceRevision != ConformanceRevision {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_NAATRE_REVISION_UNSUPPORTED", Message: "Naatre schema, protocol, and conformance revisions must match the profile"})
	}
	if config.Catalog.ProtocolVersion != MCPRevision {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_REVISION_UNSUPPORTED", Message: "MCP revision is not the pinned profile revision"})
	}
	total := len(config.Catalog.Tools) + len(config.Catalog.Resources) + len(config.Catalog.ResourceTemplates) + len(config.Catalog.Prompts)
	if limits.MaxEntries < 1 || limits.MaxEntries > 4096 || total > limits.MaxEntries || len(config.Registrations) > limits.MaxEntries || len(config.Registrations) == 0 {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_ENTRY_LIMIT", Message: "catalog or registration count is outside the configured bound"})
	}
	capabilities := make(map[string]bool, len(config.Catalog.Capabilities))
	for _, capability := range config.Catalog.Capabilities {
		capabilities[capability] = true
	}
	required := slices.Clone(config.RequiredCapabilities)
	required = append(required, "cancellation")
	for _, registration := range config.Registrations {
		switch registration.Kind {
		case ToolKind:
			required = append(required, "tools", "structured-content")
		case ResourceKind:
			required = append(required, "resources")
		case ResourceTemplateKind:
			required = append(required, "resource-templates")
		}
	}
	sort.Strings(required)
	required = slices.Compact(required)
	for _, capability := range required {
		if !capabilities[capability] {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_CAPABILITY_UNSUPPORTED", Feature: capability, Message: "required MCP capability was not negotiated"})
		}
	}
}

func validateCatalog(catalog Catalog, limits Limits, report *FidelityReport) {
	encoded, err := json.Marshal(catalog)
	if err != nil || len(encoded) > limits.MaxCatalogBytes {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_CATALOG_LIMIT", Message: "catalog cannot be represented within the configured byte limit"})
	}
	validateCatalogCapabilities(catalog.Capabilities, limits, report)
	validator := catalogEntryValidator{limits: limits, report: report, seen: make(map[string]bool, len(catalog.Tools)+len(catalog.Resources)+len(catalog.ResourceTemplates)+len(catalog.Prompts))}
	for _, tool := range catalog.Tools {
		validator.validate("tool", tool.Name, tool.Description, tool.Annotations)
		if len(tool.InputSchema) == 0 || len(tool.InputSchema) > limits.MaxSchemaBytes || len(tool.OutputSchema) == 0 || len(tool.OutputSchema) > limits.MaxSchemaBytes {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_SCHEMA_LIMIT", RemoteID: tool.Name, Message: "tool schemas are absent or oversized"})
		}
	}
	for _, resource := range catalog.Resources {
		validator.validate("resource", resource.URI, resource.Description, resource.Annotations)
	}
	for _, resource := range catalog.ResourceTemplates {
		validator.validate("resource-template", resource.URITemplate, resource.Description, resource.Annotations)
	}
	for _, prompt := range catalog.Prompts {
		validator.validate("prompt", prompt.Name, prompt.Description, prompt.Arguments)
	}
	if len(catalog.NextCursor) > limits.MaxIdentifier {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_PAGINATION_LIMIT", Message: "catalog cursor exceeds the configured bound"})
	}
}

func validateCatalogCapabilities(capabilities []string, limits Limits, report *FidelityReport) {
	seenCapabilities := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		if len(capability) == 0 || len(capability) > limits.MaxIdentifier || seenCapabilities[capability] {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_CAPABILITY_INVALID", Feature: capability, Message: "capability identity is empty, duplicated, or oversized"})
		}
		seenCapabilities[capability] = true
	}
}

type catalogEntryValidator struct {
	limits Limits
	report *FidelityReport
	seen   map[string]bool
}

func (v *catalogEntryValidator) validate(kind, identity, description string, rawValues ...json.RawMessage) {
	key := kind + "\x00" + identity
	if len(identity) == 0 || len(identity) > v.limits.MaxIdentifier || v.seen[key] {
		v.report.Diagnostics = append(v.report.Diagnostics, Diagnostic{Code: "MCP_CATALOG_ENTRY_INVALID", RemoteID: identity, Message: "catalog entry identity is empty, duplicated, or oversized"})
	}
	v.seen[key] = true
	if len(description) > v.limits.MaxContentBytes {
		v.report.Diagnostics = append(v.report.Diagnostics, Diagnostic{Code: "MCP_CONTENT_LIMIT", RemoteID: identity, Message: "untrusted description exceeds the configured content limit"})
	}
	for _, raw := range rawValues {
		if len(raw) > 0 && (len(raw) > v.limits.MaxContentBytes || protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: v.limits.MaxContentBytes, MaxDepth: v.limits.MaxSchemaDepth}) != nil) {
			v.report.Diagnostics = append(v.report.Diagnostics, Diagnostic{Code: "MCP_CONTENT_INVALID", RemoteID: identity, Message: "untrusted metadata is invalid or oversized"})
		}
	}
}

func validateRegistration(config Config, limits Limits, registration Registration, seen map[string]bool, report *FidelityReport) {
	validateRegistrationTrust(config.Direction, limits, registration, seen, report)
	validateRegistrationSchemas(config.Catalog, limits, registration, report)
}

func validateRegistrationTrust(direction Direction, limits Limits, registration Registration, seen map[string]bool, report *FidelityReport) {
	if !registration.Approved {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_REGISTRATION_UNAPPROVED", RemoteID: registration.RemoteID, Message: "remote entry is not explicitly approved"})
	}
	key := string(registration.Kind) + "\x00" + registration.RemoteID
	if len(registration.RemoteID) == 0 || len(registration.RemoteID) > limits.MaxIdentifier || seen[key] || !slices.Contains([]EntryKind{ToolKind, ResourceKind, ResourceTemplateKind}, registration.Kind) {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_REGISTRATION_INVALID", RemoteID: registration.RemoteID, Message: "registration identity is invalid or duplicated"})
	}
	seen[key] = true
	if direction == Consume && registration.Invoker == nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_INVOKER_REQUIRED", RemoteID: registration.RemoteID, Message: "consumed entries require an application-supplied invoker"})
	}
	metadata := registration.Descriptor.Metadata
	if metadata.Effect == "" || metadata.Idempotency == "" || metadata.Cost == 0 || metadata.AuthorizationPolicy == "" {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_POLICY_REQUIRED", RemoteID: registration.RemoteID, Message: "trusted Naatre effect, idempotency, cost, and authorization policy are required"})
	}
}

func validateRegistrationSchemas(catalog Catalog, limits Limits, registration Registration, report *FidelityReport) {
	trustedInput, inputErr := ValidateSchema(registration.InputSchema, limits)
	trustedOutput, outputErr := ValidateSchema(registration.OutputSchema, limits)
	if inputErr != nil {
		appendErrorDiagnostic(report, inputErr, registration.RemoteID, "input-schema")
	}
	if outputErr != nil {
		appendErrorDiagnostic(report, outputErr, registration.RemoteID, "output-schema")
	}
	remoteInput, remoteOutput, found := catalogSchemas(catalog, registration)
	if !found {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_REMOTE_ENTRY_MISSING", RemoteID: registration.RemoteID, Message: "trusted registration has no exact discovered entry"})
		return
	}
	if registration.Kind == ToolKind {
		catalogInput, err := ValidateSchema(remoteInput, limits)
		if err != nil {
			appendErrorDiagnostic(report, err, registration.RemoteID, "input-schema")
			return
		}
		catalogOutput, err := ValidateSchema(remoteOutput, limits)
		if err != nil {
			appendErrorDiagnostic(report, err, registration.RemoteID, "output-schema")
			return
		}
		if inputErr == nil && !bytes.Equal(trustedInput, catalogInput) || outputErr == nil && !bytes.Equal(trustedOutput, catalogOutput) {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "MCP_SCHEMA_MISMATCH", RemoteID: registration.RemoteID, Message: "remote schema differs from the trusted registration"})
		}
	}
}

func catalogSchemas(catalog Catalog, registration Registration) ([]byte, []byte, bool) {
	switch registration.Kind {
	case ToolKind:
		for _, tool := range catalog.Tools {
			if tool.Name == registration.RemoteID {
				return tool.InputSchema, tool.OutputSchema, true
			}
		}
	case ResourceKind:
		for _, resource := range catalog.Resources {
			if resource.URI == registration.RemoteID {
				return registration.InputSchema, registration.OutputSchema, true
			}
		}
	case ResourceTemplateKind:
		for _, resource := range catalog.ResourceTemplates {
			if resource.URITemplate == registration.RemoteID {
				return registration.InputSchema, registration.OutputSchema, true
			}
		}
	}
	return nil, nil, false
}

func DescribeTransport(config TransportConfig) (TransportReport, error) {
	if config.MaxResponseBytes < 1 || config.MaxPending < 1 {
		return TransportReport{}, adapterError("MCP_TRANSPORT_LIMIT", errors.New("response and pending-work limits must be positive"))
	}
	report := TransportReport{Kind: config.Kind, SessionIsolation: "principal-tenant-request-local", Cancellation: "active-request-abort", Backpressure: fmt.Sprintf("bounded-pending-%d", config.MaxPending), ResponseLimit: config.MaxResponseBytes, ProcessOwnership: config.ProcessOwner, LifecycleClaimOnly: true}
	switch config.Kind {
	case Stdio:
		if config.Endpoint != "" || len(config.AllowedOrigins) != 0 || config.SessionHeader != "" || config.OriginValidation || config.Reconnect || (config.ProcessOwner != AdapterOwnsProcess && config.ProcessOwner != ClientOwnsProcess) {
			return TransportReport{}, adapterError("MCP_STDIO_PROFILE_INVALID", errors.New("stdio cannot inherit HTTP lifecycle settings"))
		}
		report.OriginValidation = "not-applicable"
	case StreamableHTTP:
		endpoint, err := url.Parse(config.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return TransportReport{}, adapterError("MCP_HTTP_ENDPOINT_INVALID", errors.New("streamable HTTP requires one trusted HTTPS endpoint"))
		}
		origin := endpoint.Scheme + "://" + endpoint.Host
		if !config.OriginValidation || !slices.Contains(config.AllowedOrigins, origin) || config.SessionHeader != "Mcp-Session-Id" || config.ProcessOwner != ApplicationOwnsProcess {
			return TransportReport{}, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("streamable HTTP requires exact origin, session, and ownership policy"))
		}
		report.OriginValidation = "exact-allowlist"
		report.Reconnect = config.Reconnect
	default:
		return TransportReport{}, adapterError("MCP_TRANSPORT_UNSUPPORTED", errors.New("transport is outside the profile"))
	}
	return report, nil
}

func policyFromDescriptor(descriptor runtime.Descriptor) Policy {
	metadata := descriptor.Metadata
	return Policy{Kind: descriptor.Kind, Effect: metadata.Effect, Idempotency: metadata.Idempotency, RetrySafe: metadata.RetrySafe, Cacheable: metadata.Cacheable, Cost: metadata.Cost, Batching: metadata.Batching, Transaction: metadata.Transaction, AuthorizationPolicy: metadata.AuthorizationPolicy}
}

func revisions() Revisions {
	return Revisions{MCP: MCPSpecification, Schema: NaatreSchemaRevision, Protocol: NaatreProtocolRevision, Conformance: ConformanceRevision}
}

func appendErrorDiagnostic(report *FidelityReport, err error, remoteID, feature string) {
	var typed *Error
	code := "MCP_INVALID"
	if errors.As(err, &typed) {
		code = typed.Code
	}
	report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: code, RemoteID: remoteID, Feature: feature, Message: safeText(err.Error(), 256)})
}

func sortDiagnostics(values []Diagnostic) {
	sort.Slice(values, func(i, j int) bool {
		left := values[i].Code + "\x00" + values[i].RemoteID + "\x00" + values[i].Feature
		right := values[j].Code + "\x00" + values[j].RemoteID + "\x00" + values[j].Feature
		return left < right
	})
}

func marshalCanonical(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, adapterError("MCP_REPORT_INVALID", err)
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: 4 << 20, MaxDepth: 64})
	if err != nil {
		return nil, adapterError("MCP_REPORT_INVALID", err)
	}
	return canonical, nil
}

func cloneReport(input FidelityReport) FidelityReport {
	result := input
	result.Mappings = slices.Clone(input.Mappings)
	result.Registrations = slices.Clone(input.Registrations)
	result.Diagnostics = slices.Clone(input.Diagnostics)
	result.Claims = slices.Clone(input.Claims)
	result.NonClaims = slices.Clone(input.NonClaims)
	return result
}

func cloneManifest(input Manifest) Manifest {
	result := input
	result.Entries = slices.Clone(input.Entries)
	for index := range result.Entries {
		result.Entries[index].InputSchema = slices.Clone(result.Entries[index].InputSchema)
		result.Entries[index].OutputSchema = slices.Clone(result.Entries[index].OutputSchema)
	}
	result.WireClaims = slices.Clone(input.WireClaims)
	return result
}
