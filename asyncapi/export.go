package asyncapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/event"
	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/tooling"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// Export returns the canonical AsyncAPI document for a caller-authorized
// schema view and explicit event capabilities. It never accepts a handler,
// registry, credential provider, resolver, or network client.
func Export(input Model) ([]byte, FidelityReport, error) {
	model, revisions, err := validateExportModel(input)
	report := newReport(interopadapter.SchemaExport, revisions)
	if err != nil {
		return exportFailure(report, errorCode(err, "ASYNCAPI_MODEL_INVALID"), "model", err)
	}
	report.Links = identityLinks(model)
	document, err := buildWire(model, revisions)
	if err != nil {
		return exportFailure(report, errorCode(err, "ASYNCAPI_EXPORT_INVALID"), "document", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return exportFailure(report, "ASYNCAPI_EXPORT_INVALID", "document", err)
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{
		MaxBytes: DefaultLimits().MaxDocumentBytes, MaxDepth: DefaultLimits().MaxDepth, MaxMembers: DefaultLimits().MaxObjects,
	})
	if err != nil {
		return exportFailure(report, "ASYNCAPI_EXPORT_INVALID", "document", err)
	}
	report.Status = "ready"
	return canonical, report, nil
}

func buildWire(model Model, revisions Revisions) (wireDocument, error) {
	result := wireDocument{
		AsyncAPI: Version, ID: model.ID, Info: wireInfo{Title: model.Title, Version: model.Version},
		DefaultContentType: DefaultMediaType, Servers: map[string]wireServer{}, Channels: map[string]wireChannel{},
		Operations: map[string]wireOperation{}, Components: wireComponents{
			Messages: map[string]wireMessage{}, Schemas: map[string]wireSchema{},
			SecuritySchemes: map[string]wireSecurityScheme{}, CorrelationIDs: map[string]wireCorrelation{},
		},
		Profile: Profile, Exporter: ExporterVersion, Specification: Specification,
		Revisions: revisions, Fidelity: fidelityMappings(),
	}
	if err := exportServers(&result, model.Servers, revisions); err != nil {
		return wireDocument{}, err
	}
	if err := exportMessages(&result.Components, model.Messages, revisions); err != nil {
		return wireDocument{}, err
	}
	exportSecurity(&result.Components, model.Security, revisions)
	exportChannels(&result, model.Channels, indexBindings(model.Bindings), revisions)
	exportOperations(&result, model.Operations, revisions)
	return result, nil
}

func exportServers(document *wireDocument, servers []Server, revisions Revisions) error {
	for _, server := range servers {
		parsed, err := parseServerURL(server.URL, server.Protocol)
		if err != nil {
			return err
		}
		document.Servers[server.ID] = wireServer{
			Host: parsed.Host, Protocol: server.Protocol, Pathname: parsed.EscapedPath(),
			Identity: identity("server", server.NaatreID, server.Revision, revisions),
		}
	}
	return nil
}

func exportMessages(components *wireComponents, messages []Message, revisions Revisions) error {
	for _, message := range messages {
		identityValue := identity("message", message.NaatreID, message.Revision, revisions)
		wire := wireMessage{
			Name: message.ID, ContentType: message.ContentType, SchemaFormat: SchemaFormat,
			Payload:      wireReference{Ref: "#/components/schemas/" + string(message.PayloadType)},
			Correlations: slices.Clone(message.Correlations), Identity: identityValue,
		}
		for _, correlation := range message.Correlations {
			correlationID := message.ID + "." + correlation.ID
			correlationIdentity := identity("correlation", message.NaatreID+"."+correlation.ID, message.Revision, revisions)
			components.CorrelationIDs[correlationID] = wireCorrelation{
				Location: correlation.Location, Lifetime: correlation.Lifetime, Trust: correlation.Trust, Identity: correlationIdentity,
			}
			if wire.CorrelationID == nil {
				wire.CorrelationID = &wireReference{Ref: "#/components/correlationIds/" + correlationID}
			}
		}
		for _, example := range message.Examples {
			redacted := tooling.RedactCredentials(string(example))
			canonical, err := protocol.CanonicalizeJSON([]byte(redacted), protocol.Limits{MaxBytes: DefaultLimits().MaxExampleBytes, MaxDepth: DefaultLimits().MaxDepth, MaxMembers: DefaultLimits().MaxObjects})
			if err != nil {
				return publicError("ASYNCAPI_EXAMPLE_INVALID", err)
			}
			wire.Examples = append(wire.Examples, json.RawMessage(canonical))
		}
		components.Messages[message.ID] = wire
		if _, exists := components.Schemas[string(message.PayloadType)]; !exists {
			components.Schemas[string(message.PayloadType)] = wireSchema{
				Type: "object", AdditionalProperties: true, NaatreType: string(message.PayloadType),
				Identity: identity("schema", string(message.PayloadType), revisions.Schema, revisions),
			}
		}
	}
	return nil
}

func exportSecurity(components *wireComponents, values []SecurityScheme, revisions Revisions) {
	for _, security := range values {
		components.SecuritySchemes[security.ID] = wireSecurityScheme{
			Type: security.Type, Scheme: security.Scheme, Name: security.Name, In: security.In,
			Identity: identity("security", security.NaatreID, security.Revision, revisions),
		}
	}
}

func exportChannels(document *wireDocument, values []Channel, bindings map[string]Binding, revisions Revisions) {
	for _, channel := range values {
		wire := wireChannel{
			Address: channel.Address, Messages: map[string]wireReference{}, Bindings: map[string]wireBinding{},
			Identity: identity("channel", channel.NaatreID, channel.Revision, revisions), Semantics: channel.Semantics,
		}
		for _, message := range channel.Messages {
			wire.Messages[message] = wireReference{Ref: "#/components/messages/" + message}
		}
		for _, bindingID := range channel.Bindings {
			binding := bindings[bindingID]
			wire.Bindings[bindingID] = wireBinding{
				Transport: binding.Transport, Server: wireReference{Ref: "#/servers/" + binding.Server}, Implemented: binding.Implemented,
				Evidence: slices.Clone(binding.Evidence), WireCompatibility: binding.WireCompatibility,
				Identity: identity("binding", binding.NaatreID, binding.Revision, revisions),
			}
		}
		document.Channels[channel.ID] = wire
	}
}

func exportOperations(document *wireDocument, values []Operation, revisions Revisions) {
	for _, operation := range values {
		wire := wireOperation{
			Action: operation.Action, Channel: wireReference{Ref: "#/channels/" + operation.Channel},
			Identity: identity("operation", operation.NaatreID, operation.Revision, revisions),
		}
		for _, message := range operation.Messages {
			wire.Messages = append(wire.Messages, wireReference{Ref: "#/components/messages/" + message})
		}
		for _, security := range operation.Security {
			wire.Security = append(wire.Security, wireReference{Ref: "#/components/securitySchemes/" + security})
		}
		document.Operations[operation.ID] = wire
	}
}

func validateExportModel(input Model) (Model, Revisions, error) {
	if input.Schema.Revision() == "" {
		return Model{}, Revisions{}, publicError("ASYNCAPI_SCHEMA_REQUIRED", errors.New("authorized schema document is required"))
	}
	canonicalSchema, err := input.Schema.CanonicalJSON()
	if err != nil {
		return Model{}, Revisions{}, publicError("ASYNCAPI_SCHEMA_INVALID", err)
	}
	digest, err := input.Schema.Hash()
	if err != nil {
		return Model{}, Revisions{}, publicError("ASYNCAPI_SCHEMA_INVALID", err)
	}
	var schemaHeader struct {
		CanonicalVersion string `json:"canonicalVersion"`
	}
	if json.Unmarshal(canonicalSchema, &schemaHeader) != nil || schemaHeader.CanonicalVersion == "" {
		return Model{}, Revisions{}, publicError("ASYNCAPI_SCHEMA_INVALID", errors.New("schema canonicalization revision is required"))
	}
	revisions := Revisions{
		Schema: input.Schema.Revision(), SchemaDigest: "sha256:" + digest.Hex, Capabilities: input.CapabilityRevision,
		Canonicalization: schemaHeader.CanonicalVersion, EventEnvelope: input.EventEnvelopeRevision,
		AsyncAPIProfile: Profile, Exporter: ExporterVersion,
	}
	if !validText(input.ID, 2048) || !validText(input.Title, 256) || !validText(input.Version, 128) ||
		!identifierPattern.MatchString(input.CapabilityRevision) || input.EventEnvelopeRevision != event.Profile {
		return Model{}, revisions, publicError("ASYNCAPI_IDENTITY_INVALID", errors.New("document identities and revisions are invalid"))
	}
	model := cloneModel(input)
	model.Revisions = revisions
	model.canonical = nil
	model.Messages = nil
	for _, message := range input.Messages {
		if !message.Protected {
			model.Messages = append(model.Messages, message)
		}
	}
	if err := validateDeclarations(model); err != nil {
		return Model{}, revisions, err
	}
	return normalizeModel(model), revisions, nil
}

func cloneModel(input Model) Model {
	result := input
	result.Servers = slices.Clone(input.Servers)
	result.Bindings = slices.Clone(input.Bindings)
	for index := range result.Bindings {
		result.Bindings[index].Evidence = slices.Clone(input.Bindings[index].Evidence)
	}
	result.Messages = slices.Clone(input.Messages)
	for index := range result.Messages {
		result.Messages[index].Correlations = slices.Clone(input.Messages[index].Correlations)
		result.Messages[index].Examples = slices.Clone(input.Messages[index].Examples)
	}
	result.Channels = slices.Clone(input.Channels)
	for index := range result.Channels {
		result.Channels[index].Messages = slices.Clone(input.Channels[index].Messages)
		result.Channels[index].Bindings = slices.Clone(input.Channels[index].Bindings)
	}
	result.Operations = slices.Clone(input.Operations)
	for index := range result.Operations {
		result.Operations[index].Messages = slices.Clone(input.Operations[index].Messages)
		result.Operations[index].Security = slices.Clone(input.Operations[index].Security)
	}
	result.Security = slices.Clone(input.Security)
	result.canonical = slices.Clone(input.canonical)
	return result
}

func validateDeclarations(model Model) error {
	servers, err := validateServers(model.Servers)
	if err != nil {
		return err
	}
	bindings, err := validateBindings(model.Bindings, servers)
	if err != nil {
		return err
	}
	messages, err := validateMessages(model)
	if err != nil {
		return err
	}
	security, err := validateSecurity(model.Security)
	if err != nil {
		return err
	}
	channels, usedBindings, err := validateChannels(model.Channels, messages, bindings)
	if err != nil {
		return err
	}
	if err := requireAllBindings(bindings, usedBindings); err != nil {
		return err
	}
	operations, err := validateOperations(model.Operations, channels, messages, security)
	if err != nil {
		return err
	}
	if len(channels) == 0 || len(messages) == 0 || len(operations) == 0 {
		return publicError("ASYNCAPI_DECLARATION_REQUIRED", errors.New("channel, message, and operation declarations are required"))
	}
	return nil
}

func validateServers(values []Server) (map[string]Server, error) {
	result := make(map[string]Server, len(values))
	for _, value := range values {
		if err := addIdentity(value.ID, value.NaatreID, value.Revision, result); err != nil {
			return nil, err
		}
		if _, err := parseServerURL(value.URL, value.Protocol); err != nil {
			return nil, err
		}
		result[value.ID] = value
	}
	return result, nil
}

func validateBindings(values []Binding, servers map[string]Server) (map[string]Binding, error) {
	result := make(map[string]Binding, len(values))
	for _, value := range values {
		if err := addIdentity(value.ID, value.NaatreID, value.Revision, result); err != nil {
			return nil, err
		}
		if !validTransport(value.Transport) || !value.Implemented || len(value.Evidence) == 0 || value.WireCompatibility || servers[value.Server].ID == "" {
			return nil, publicError("ASYNCAPI_BINDING_UNPROVEN", errors.New("bindings require an implemented tested transport and server"))
		}
		for _, evidence := range value.Evidence {
			if !validText(evidence, 512) {
				return nil, publicError("ASYNCAPI_BINDING_UNPROVEN", errors.New("binding evidence is invalid"))
			}
		}
		result[value.ID] = value
	}
	return result, nil
}

func validateMessages(model Model) (map[string]Message, error) {
	snapshot, err := model.Schema.Snapshot()
	if err != nil {
		return nil, publicError("ASYNCAPI_SCHEMA_INVALID", err)
	}
	result := make(map[string]Message, len(model.Messages))
	for _, value := range model.Messages {
		if err := addIdentity(value.ID, value.NaatreID, value.Revision, result); err != nil {
			return nil, err
		}
		if value.ContentType == "" || len(value.Correlations) == 0 {
			return nil, publicError("ASYNCAPI_MESSAGE_INVALID", errors.New("message content type and correlation metadata are required"))
		}
		if _, ok := snapshot.Lookup(value.PayloadType); !ok {
			return nil, publicError("ASYNCAPI_SCHEMA_INVALID", errors.New("message payload type is not in the authorized schema"))
		}
		if err := validateCorrelations(value.Correlations); err != nil {
			return nil, err
		}
		result[value.ID] = value
	}
	return result, nil
}

func validateCorrelations(values []Correlation) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !identifierPattern.MatchString(value.ID) || seen[value.ID] || !validCorrelationLocation(value.Location) || !validText(value.Lifetime, 128) || !validText(value.Trust, 128) {
			return publicError("ASYNCAPI_CORRELATION_INVALID", errors.New("correlation metadata is invalid"))
		}
		seen[value.ID] = true
	}
	return nil
}

func validateSecurity(values []SecurityScheme) (map[string]SecurityScheme, error) {
	result := make(map[string]SecurityScheme, len(values))
	for _, value := range values {
		if err := addIdentity(value.ID, value.NaatreID, value.Revision, result); err != nil {
			return nil, err
		}
		if value.Type != "http" && value.Type != "httpApiKey" {
			return nil, publicError("ASYNCAPI_SECURITY_UNSUPPORTED", errors.New("security scheme type is outside the profile"))
		}
		if value.Type == "http" && value.Scheme == "" || value.Type == "httpApiKey" && (value.Name == "" || value.In != "header") {
			return nil, publicError("ASYNCAPI_SECURITY_INVALID", errors.New("security scheme is incomplete"))
		}
		result[value.ID] = value
	}
	return result, nil
}

func validateChannels(values []Channel, messages map[string]Message, bindings map[string]Binding) (map[string]Channel, map[string]bool, error) {
	result := make(map[string]Channel, len(values))
	used := make(map[string]bool, len(bindings))
	for _, value := range values {
		if err := addIdentity(value.ID, value.NaatreID, value.Revision, result); err != nil {
			return nil, nil, err
		}
		if err := validateChannel(value, messages, bindings, used); err != nil {
			return nil, nil, err
		}
		result[value.ID] = value
	}
	return result, used, nil
}

func validateChannel(value Channel, messages map[string]Message, bindings map[string]Binding, used map[string]bool) error {
	semantics := value.Semantics
	if !validText(value.Address, 2048) || len(value.Messages) == 0 || len(value.Bindings) == 0 || semantics.WireCompatibility || !validText(semantics.Ordering, 128) || !validText(semantics.Replay, 128) || !validText(semantics.Terminal, 128) || !validText(semantics.Errors, 128) {
		return publicError("ASYNCAPI_CHANNEL_INVALID", errors.New("channel semantics are incomplete"))
	}
	for _, id := range value.Messages {
		if messages[id].ID == "" {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("channel references an unknown message"))
		}
	}
	for _, id := range value.Bindings {
		if bindings[id].ID == "" {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("channel references an unknown binding"))
		}
		used[id] = true
	}
	return nil
}

func requireAllBindings(bindings map[string]Binding, used map[string]bool) error {
	for id := range bindings {
		if !used[id] {
			return publicError("ASYNCAPI_BINDING_UNREFERENCED", errors.New("every exported binding must belong to a channel"))
		}
	}
	return nil
}

func validateOperations(values []Operation, channels map[string]Channel, messages map[string]Message, security map[string]SecurityScheme) (map[string]Operation, error) {
	result := make(map[string]Operation, len(values))
	for _, value := range values {
		if err := addIdentity(value.ID, value.NaatreID, value.Revision, result); err != nil {
			return nil, err
		}
		if err := validateOperation(value, channels, messages, security); err != nil {
			return nil, err
		}
		result[value.ID] = value
	}
	return result, nil
}

func validateOperation(value Operation, channels map[string]Channel, messages map[string]Message, security map[string]SecurityScheme) error {
	channel := channels[value.Channel]
	if value.Action != ActionSend && value.Action != ActionReceive || channel.ID == "" || len(value.Messages) == 0 || len(value.Security) == 0 {
		return publicError("ASYNCAPI_OPERATION_INVALID", errors.New("operation direction, channel, messages, and security are required"))
	}
	for _, id := range value.Messages {
		if messages[id].ID == "" || !slices.Contains(channel.Messages, id) {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("operation message is not declared on its channel"))
		}
	}
	for _, id := range value.Security {
		if security[id].ID == "" {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("operation references an unknown security scheme"))
		}
	}
	return nil
}

func addIdentity[T any](id, naatreID, revision string, values map[string]T) error {
	if !identifierPattern.MatchString(id) || !identifierPattern.MatchString(naatreID) || !identifierPattern.MatchString(revision) {
		return publicError("ASYNCAPI_IDENTITY_INVALID", errors.New("stable declaration identity is invalid"))
	}
	if _, exists := values[id]; exists {
		return publicError("ASYNCAPI_IDENTITY_DUPLICATE", errors.New("stable declaration identity is duplicated"))
	}
	return nil
}

func normalizeModel(model Model) Model {
	sort.Slice(model.Servers, func(i, j int) bool { return model.Servers[i].ID < model.Servers[j].ID })
	sort.Slice(model.Bindings, func(i, j int) bool { return model.Bindings[i].ID < model.Bindings[j].ID })
	sort.Slice(model.Messages, func(i, j int) bool { return model.Messages[i].ID < model.Messages[j].ID })
	sort.Slice(model.Channels, func(i, j int) bool { return model.Channels[i].ID < model.Channels[j].ID })
	sort.Slice(model.Operations, func(i, j int) bool { return model.Operations[i].ID < model.Operations[j].ID })
	sort.Slice(model.Security, func(i, j int) bool { return model.Security[i].ID < model.Security[j].ID })
	for index := range model.Bindings {
		slices.Sort(model.Bindings[index].Evidence)
	}
	for index := range model.Messages {
		sort.Slice(model.Messages[index].Correlations, func(i, j int) bool {
			return model.Messages[index].Correlations[i].ID < model.Messages[index].Correlations[j].ID
		})
	}
	for index := range model.Channels {
		slices.Sort(model.Channels[index].Messages)
		slices.Sort(model.Channels[index].Bindings)
	}
	for index := range model.Operations {
		slices.Sort(model.Operations[index].Messages)
		slices.Sort(model.Operations[index].Security)
	}
	return model
}

func identity(kind, naatreID, revision string, revisions Revisions) Identity {
	return Identity{
		Kind: kind, NaatreID: naatreID, Revision: revision, Schema: revisions.Schema,
		Capability: revisions.Capabilities, Canonicalization: revisions.Canonicalization, EventEnvelope: revisions.EventEnvelope,
	}
}

func identityLinks(model Model) []IdentityLink {
	links := make([]IdentityLink, 0, len(model.Servers)+len(model.Bindings)+len(model.Messages)+len(model.Channels)+len(model.Operations)+len(model.Security))
	appendLink := func(kind, id, naatreID, revision string) {
		links = append(links, IdentityLink{AsyncAPIKind: kind, AsyncAPIID: id, NaatreID: naatreID, Revision: revision})
	}
	for _, value := range model.Servers {
		appendLink("server", value.ID, value.NaatreID, value.Revision)
	}
	for _, value := range model.Bindings {
		appendLink("binding", value.ID, value.NaatreID, value.Revision)
	}
	for _, value := range model.Messages {
		appendLink("message", value.ID, value.NaatreID, value.Revision)
	}
	for _, value := range model.Channels {
		appendLink("channel", value.ID, value.NaatreID, value.Revision)
	}
	for _, value := range model.Operations {
		appendLink("operation", value.ID, value.NaatreID, value.Revision)
	}
	for _, value := range model.Security {
		appendLink("security", value.ID, value.NaatreID, value.Revision)
	}
	sort.Slice(links, func(i, j int) bool {
		return links[i].AsyncAPIKind+"\x00"+links[i].AsyncAPIID < links[j].AsyncAPIKind+"\x00"+links[j].AsyncAPIID
	})
	return links
}

func fidelityMappings() []interopadapter.Mapping {
	values := []interopadapter.Mapping{
		{Feature: "authorization-security", Classification: interopadapter.ApplicationSupplied, Resolution: "AsyncAPI security metadata never grants Naatre authorization"},
		{Feature: "canonicalization", Classification: interopadapter.Lossless},
		{Feature: "capability-revision", Classification: interopadapter.Lossless},
		{Feature: "correlation-identifiers", Classification: interopadapter.Lossless},
		{Feature: "event-envelope", Classification: interopadapter.ExplicitlyAdapted, Resolution: "CloudEvents structured JSON remains governed by the Naatre event profile"},
		{Feature: "execution-semantics", Classification: interopadapter.ApplicationSupplied, Resolution: "AsyncAPI operations are descriptive and never invoke handlers"},
		{Feature: "ordering-replay-terminal-errors", Classification: interopadapter.ExplicitlyAdapted, Resolution: "Naatre channel extensions preserve logical stream semantics"},
		{Feature: "partial-results", Classification: interopadapter.ExplicitlyAdapted, Resolution: "Naatre stream frames remain authoritative"},
		{Feature: "retry-delivery", Classification: interopadapter.ApplicationSupplied, Resolution: "Naatre webhook policy remains authoritative"},
		{Feature: "schema-identity", Classification: interopadapter.Lossless},
		{Feature: "transport-bindings", Classification: interopadapter.ExplicitlyAdapted, Resolution: "Only implemented evidence-backed Naatre bindings are declared"},
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Feature < values[j].Feature })
	return values
}

func newReport(direction interopadapter.Direction, revisions Revisions) FidelityReport {
	return FidelityReport{
		Profile: Profile, ExporterVersion: ExporterVersion, Specification: Specification,
		Direction: direction, Status: "rejected", Revisions: revisions,
		Mappings: fidelityMappings(), Links: []IdentityLink{}, Diagnostics: []Diagnostic{},
	}
}

func exportFailure(report FidelityReport, code, feature string, cause error) ([]byte, FidelityReport, error) {
	report.Diagnostics = []Diagnostic{{Code: code, Feature: feature, Message: "AsyncAPI export rejected unsafe or unsupported metadata"}}
	return nil, report, publicError(code, cause)
}

func parseServerURL(raw, protocolName string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != protocolName {
		return nil, publicError("ASYNCAPI_SERVER_INVALID", errors.New("server URL must be an exact credential-free absolute URL"))
	}
	if protocolName != "https" && protocolName != "wss" {
		return nil, publicError("ASYNCAPI_SERVER_UNSUPPORTED", fmt.Errorf("server protocol %q is unsupported", protocolName))
	}
	return parsed, nil
}

func validTransport(value Transport) bool {
	switch value {
	case TransportSSE, TransportWebSocket, TransportWebhookHTTP, TransportWorker:
		return true
	default:
		return false
	}
}

func validCorrelationLocation(value string) bool {
	return strings.HasPrefix(value, "$message.header#/") || strings.HasPrefix(value, "$message.payload#/")
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

func indexBindings(values []Binding) map[string]Binding {
	result := make(map[string]Binding, len(values))
	for _, value := range values {
		result[value.ID] = value
	}
	return result
}

func publicError(code string, cause error) *Error { return &Error{code: code, cause: cause} }

func errorCode(err error, fallback string) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.code
	}
	return fallback
}
