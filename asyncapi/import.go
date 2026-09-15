package asyncapi

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/tooling"
)

// Import validates the complete local document and returns a detached model.
// References are never dereferenced over a network. Server URLs are inert
// metadata and require an exact caller-provided allowlist before acceptance.
func Import(input []byte, options ImportOptions) (Model, FidelityReport, error) {
	limits, err := resolveLimits(options.Limits)
	report := newReport(interopadapter.SchemaImport, Revisions{})
	if err != nil {
		return importFailure(report, "ASYNCAPI_LIMIT_INVALID", "resource-limits", "", err)
	}
	if len(input) > limits.MaxDocumentBytes {
		return importFailure(report, "ASYNCAPI_DOCUMENT_LIMIT", "resource-limits", "", errors.New("document exceeds configured limit"))
	}
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{MaxBytes: limits.MaxDocumentBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxObjects})
	if err != nil {
		return importFailure(report, "ASYNCAPI_DOCUMENT_INVALID", "document", "", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var document wireDocument
	if err := decoder.Decode(&document); err != nil {
		return importFailure(report, "ASYNCAPI_DOCUMENT_INVALID", "document", "", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return importFailure(report, "ASYNCAPI_DOCUMENT_INVALID", "document", "", err)
	}
	report.Revisions = document.Revisions
	report.Mappings = slices.Clone(document.Fidelity)
	if err := validateHeader(document); err != nil {
		return importFailure(report, errorCode(err, "ASYNCAPI_DOCUMENT_INVALID"), "document", "", err)
	}
	if err := validateImportedMappings(document.Fidelity); err != nil {
		return importFailure(report, errorCode(err, "ASYNCAPI_REQUIRED_SEMANTIC_UNSUPPORTED"), "fidelity", "", err)
	}
	allowed, err := normalizeAllowedServers(options.AllowedServerURLs)
	if err != nil {
		return importFailure(report, "ASYNCAPI_EGRESS_POLICY_INVALID", "network-egress", "", err)
	}
	model, graph, err := importWire(document, allowed, limits)
	if err != nil {
		return importFailure(report, errorCode(err, "ASYNCAPI_DOCUMENT_INVALID"), importErrorFeature(err), "", err)
	}
	if err := detectReferenceCycles(graph, limits.MaxReferences); err != nil {
		return importFailure(report, errorCode(err, "ASYNCAPI_REFERENCE_CYCLE"), "references", "", err)
	}
	model.canonical = slices.Clone(canonical)
	model = normalizeModel(model)
	report.Links = identityLinks(model)
	report.Status = "ready"
	return model, report, nil
}

func validateHeader(document wireDocument) error {
	if document.AsyncAPI != Version || document.Profile != Profile || document.Exporter != ExporterVersion || document.Specification != Specification {
		return publicError("ASYNCAPI_SPECIFICATION_UNSUPPORTED", errors.New("document does not use the pinned AsyncAPI profile"))
	}
	if !validText(document.ID, 2048) || !validText(document.Info.Title, 256) || !validText(document.Info.Version, 128) || document.DefaultContentType != DefaultMediaType {
		return publicError("ASYNCAPI_DOCUMENT_INVALID", errors.New("document identity, info, or content type is invalid"))
	}
	r := document.Revisions
	digestBytes, digestErr := hex.DecodeString(strings.TrimPrefix(r.SchemaDigest, "sha256:"))
	if !identifierPattern.MatchString(r.Schema) || !strings.HasPrefix(r.SchemaDigest, "sha256:") || len(digestBytes) != 32 || digestErr != nil ||
		!identifierPattern.MatchString(r.Capabilities) || !identifierPattern.MatchString(r.Canonicalization) ||
		r.EventEnvelope == "" || r.AsyncAPIProfile != Profile || r.Exporter != ExporterVersion {
		return publicError("ASYNCAPI_REVISION_INVALID", errors.New("naatre revision binding is incomplete"))
	}
	return nil
}

func validateImportedMappings(values []interopadapter.Mapping) error {
	required := map[string]bool{}
	for _, mapping := range fidelityMappings() {
		required[mapping.Feature] = true
	}
	seen := map[string]bool{}
	for _, mapping := range values {
		if !required[mapping.Feature] || seen[mapping.Feature] {
			return publicError("ASYNCAPI_FIDELITY_INVALID", errors.New("fidelity feature is unknown or duplicated"))
		}
		seen[mapping.Feature] = true
		switch mapping.Classification {
		case interopadapter.Lossless:
			if mapping.Resolution != "" {
				return publicError("ASYNCAPI_FIDELITY_INVALID", errors.New("lossless mapping has an adaptation"))
			}
		case interopadapter.ExplicitlyAdapted, interopadapter.ApplicationSupplied:
			if mapping.Resolution == "" {
				return publicError("ASYNCAPI_REQUIRED_SEMANTIC_UNSUPPORTED", errors.New("required adapted semantic has no resolution"))
			}
		case interopadapter.Unsupported:
			return publicError("ASYNCAPI_REQUIRED_SEMANTIC_UNSUPPORTED", errors.New("required semantic is unsupported"))
		default:
			return publicError("ASYNCAPI_FIDELITY_INVALID", errors.New("fidelity classification is invalid"))
		}
	}
	for feature := range required {
		if !seen[feature] {
			return publicError("ASYNCAPI_REQUIRED_SEMANTIC_UNSUPPORTED", errors.New("required semantic is absent"))
		}
	}
	return nil
}

func importWire(document wireDocument, allowed map[string]bool, limits Limits) (Model, map[string][]string, error) {
	if len(document.Channels) > limits.MaxChannels || len(document.Components.Messages) > limits.MaxMessages {
		return Model{}, nil, publicError("ASYNCAPI_DECLARATION_LIMIT", errors.New("channel or message count exceeds configured limit"))
	}
	model := Model{
		ID: document.ID, Title: document.Info.Title, Version: document.Info.Version,
		CapabilityRevision: document.Revisions.Capabilities, EventEnvelopeRevision: document.Revisions.EventEnvelope,
		Revisions: document.Revisions,
	}
	graph := map[string][]string{}
	var err error
	if model.Servers, err = importServers(document.Servers, document.Revisions, allowed); err != nil {
		return Model{}, nil, err
	}
	if model.Messages, err = importMessages(document.Components, document.Revisions, limits, graph); err != nil {
		return Model{}, nil, err
	}
	if model.Security, err = importSecurity(document.Components.SecuritySchemes, document.Revisions); err != nil {
		return Model{}, nil, err
	}
	var bindings map[string]Binding
	if model.Channels, bindings, err = importChannels(document.Channels, document.Revisions, model, graph); err != nil {
		return Model{}, nil, err
	}
	for _, id := range sortedMapKeys(bindings) {
		model.Bindings = append(model.Bindings, bindings[id])
	}
	if model.Operations, err = importOperations(document.Operations, document.Revisions, model, graph); err != nil {
		return Model{}, nil, err
	}
	if len(model.Channels) == 0 || len(model.Messages) == 0 || len(model.Operations) == 0 {
		return Model{}, nil, publicError("ASYNCAPI_DECLARATION_REQUIRED", errors.New("channel, message, and operation declarations are required"))
	}
	return model, graph, nil
}

func importServers(values map[string]wireServer, revisions Revisions, allowed map[string]bool) ([]Server, error) {
	result := make([]Server, 0, len(values))
	for _, id := range sortedMapKeys(values) {
		value := values[id]
		if err := validateWireIdentity(value.Identity, "server", revisions); err != nil {
			return nil, err
		}
		raw := value.Protocol + "://" + value.Host + value.Pathname
		if _, err := parseServerURL(raw, value.Protocol); err != nil {
			return nil, err
		}
		if !allowed[raw] {
			return nil, publicError("ASYNCAPI_SERVER_DENIED", errors.New("server URL is not on the configured egress allowlist"))
		}
		result = append(result, Server{ID: id, NaatreID: value.Identity.NaatreID, Revision: value.Identity.Revision, URL: raw, Protocol: value.Protocol})
	}
	return result, nil
}

func importMessages(components wireComponents, revisions Revisions, limits Limits, graph map[string][]string) ([]Message, error) {
	result := make([]Message, 0, len(components.Messages))
	context := messageImportContext{
		components: components, revisions: revisions, limits: limits, graph: graph,
		usedSchemas: map[string]bool{}, usedCorrelations: map[string]bool{},
	}
	for _, id := range sortedMapKeys(components.Messages) {
		message, err := importMessage(id, components.Messages[id], context)
		if err != nil {
			return nil, err
		}
		result = append(result, message)
	}
	if len(context.usedSchemas) != len(components.Schemas) || len(context.usedCorrelations) != len(components.CorrelationIDs) {
		return nil, publicError("ASYNCAPI_COMPONENT_UNREFERENCED", errors.New("unreferenced schema or correlation components are outside the subset"))
	}
	return result, nil
}

type messageImportContext struct {
	components       wireComponents
	revisions        Revisions
	limits           Limits
	graph            map[string][]string
	usedSchemas      map[string]bool
	usedCorrelations map[string]bool
}

func importMessage(id string, value wireMessage, context messageImportContext) (Message, error) {
	if err := validateWireIdentity(value.Identity, "message", context.revisions); err != nil {
		return Message{}, err
	}
	payload, err := localReference(value.Payload.Ref, "#/components/schemas/")
	if err != nil {
		return Message{}, err
	}
	schemaValue, exists := context.components.Schemas[payload]
	if !exists || schemaValue.NaatreType != payload || schemaValue.Type != "object" || !schemaValue.AdditionalProperties {
		return Message{}, publicError("ASYNCAPI_SCHEMA_INVALID", errors.New("message payload schema is missing or outside the opaque Naatre subset"))
	}
	if err := validateWireIdentity(schemaValue.Identity, "schema", context.revisions); err != nil {
		return Message{}, err
	}
	if value.Name != id || value.SchemaFormat != SchemaFormat || !validText(value.ContentType, 256) || len(value.Correlations) == 0 {
		return Message{}, publicError("ASYNCAPI_MESSAGE_INVALID", errors.New("message schema format, content type, or correlation is invalid"))
	}
	message := Message{ID: id, NaatreID: value.Identity.NaatreID, Revision: value.Identity.Revision, PayloadType: schema.TypeID(schemaValue.NaatreType), ContentType: value.ContentType, Correlations: slices.Clone(value.Correlations)}
	for _, example := range value.Examples {
		if len(example) > context.limits.MaxExampleBytes || !json.Valid(example) {
			return Message{}, publicError("ASYNCAPI_EXAMPLE_INVALID", errors.New("message example exceeds limits or is invalid"))
		}
		if !credentialSafeExample(example, context.limits) {
			return Message{}, publicError("ASYNCAPI_EXAMPLE_CREDENTIAL", errors.New("message example contains credential material"))
		}
		message.Examples = append(message.Examples, slices.Clone(example))
	}
	if err := importCorrelations(id, message, value.CorrelationID, context); err != nil {
		return Message{}, err
	}
	context.usedSchemas[payload] = true
	context.graph["message:"+id] = append(context.graph["message:"+id], "schema:"+payload)
	return message, nil
}

func importCorrelations(id string, message Message, primary *wireReference, context messageImportContext) error {
	for _, correlation := range message.Correlations {
		componentID := id + "." + correlation.ID
		component, ok := context.components.CorrelationIDs[componentID]
		if !ok || component.Location != correlation.Location || component.Lifetime != correlation.Lifetime || component.Trust != correlation.Trust {
			return publicError("ASYNCAPI_CORRELATION_INVALID", errors.New("correlation component and Naatre metadata differ"))
		}
		if err := validateWireIdentity(component.Identity, "correlation", context.revisions); err != nil {
			return err
		}
		context.usedCorrelations[componentID] = true
		context.graph["message:"+id] = append(context.graph["message:"+id], "correlation:"+componentID)
	}
	if primary == nil {
		return publicError("ASYNCAPI_CORRELATION_INVALID", errors.New("primary correlation reference is required"))
	}
	primaryID, err := localReference(primary.Ref, "#/components/correlationIds/")
	if err != nil {
		return err
	}
	if context.components.CorrelationIDs[primaryID].Location == "" {
		return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("primary correlation reference is invalid"))
	}
	return nil
}

func importSecurity(values map[string]wireSecurityScheme, revisions Revisions) ([]SecurityScheme, error) {
	result := make([]SecurityScheme, 0, len(values))
	for _, id := range sortedMapKeys(values) {
		value := values[id]
		if err := validateWireIdentity(value.Identity, "security", revisions); err != nil {
			return nil, err
		}
		if value.Type != "http" && value.Type != "httpApiKey" {
			return nil, publicError("ASYNCAPI_SECURITY_UNSUPPORTED", errors.New("security scheme is outside the profile"))
		}
		if value.Type == "http" && value.Scheme == "" || value.Type == "httpApiKey" && (value.Name == "" || value.In != "header") {
			return nil, publicError("ASYNCAPI_SECURITY_INVALID", errors.New("security scheme is incomplete"))
		}
		result = append(result, SecurityScheme{ID: id, NaatreID: value.Identity.NaatreID, Revision: value.Identity.Revision, Type: value.Type, Scheme: value.Scheme, Name: value.Name, In: value.In})
	}
	return result, nil
}

func importChannels(values map[string]wireChannel, revisions Revisions, model Model, graph map[string][]string) ([]Channel, map[string]Binding, error) {
	channels := make([]Channel, 0, len(values))
	context := importContext{revisions: revisions, model: model, graph: graph, bindings: map[string]Binding{}}
	for _, id := range sortedMapKeys(values) {
		channel, err := importChannel(id, values[id], context)
		if err != nil {
			return nil, nil, err
		}
		channels = append(channels, channel)
	}
	return channels, context.bindings, nil
}

type importContext struct {
	revisions Revisions
	model     Model
	graph     map[string][]string
	bindings  map[string]Binding
}

func importChannel(id string, value wireChannel, context importContext) (Channel, error) {
	if err := validateWireIdentity(value.Identity, "channel", context.revisions); err != nil {
		return Channel{}, err
	}
	semantics := value.Semantics
	if semantics.WireCompatibility || !validText(semantics.Ordering, 128) || !validText(semantics.Replay, 128) || !validText(semantics.Terminal, 128) || !validText(semantics.Errors, 128) || !validText(value.Address, 2048) {
		return Channel{}, publicError("ASYNCAPI_CHANNEL_INVALID", errors.New("channel Naatre semantics or address is invalid"))
	}
	channel := Channel{ID: id, NaatreID: value.Identity.NaatreID, Revision: value.Identity.Revision, Address: value.Address, Semantics: semantics}
	if err := importChannelMessages(id, value.Messages, context, &channel); err != nil {
		return Channel{}, err
	}
	if err := importChannelBindings(id, value.Bindings, context, &channel); err != nil {
		return Channel{}, err
	}
	if len(channel.Messages) == 0 || len(channel.Bindings) == 0 {
		return Channel{}, publicError("ASYNCAPI_CHANNEL_INVALID", errors.New("channel messages and bindings are required"))
	}
	return channel, nil
}

func importChannelMessages(id string, values map[string]wireReference, context importContext, channel *Channel) error {
	for _, messageID := range sortedMapKeys(values) {
		ref, err := localReference(values[messageID].Ref, "#/components/messages/")
		if err != nil {
			return err
		}
		if ref != messageID || !hasMessage(context.model.Messages, ref) {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("channel message reference is invalid"))
		}
		channel.Messages = append(channel.Messages, ref)
		context.graph["channel:"+id] = append(context.graph["channel:"+id], "message:"+ref)
	}
	return nil
}

func importChannelBindings(id string, values map[string]wireBinding, context importContext, channel *Channel) error {
	for _, bindingID := range sortedMapKeys(values) {
		binding, err := importBinding(bindingID, values[bindingID], context.revisions, context.model.Servers)
		if err != nil {
			return err
		}
		if previous, exists := context.bindings[bindingID]; exists && !reflect.DeepEqual(previous, binding) {
			return publicError("ASYNCAPI_BINDING_INVALID", errors.New("binding identity has conflicting declarations"))
		}
		context.bindings[bindingID] = binding
		channel.Bindings = append(channel.Bindings, bindingID)
		context.graph["channel:"+id] = append(context.graph["channel:"+id], "server:"+binding.Server)
	}
	return nil
}

func importBinding(id string, value wireBinding, revisions Revisions, servers []Server) (Binding, error) {
	if err := validateWireIdentity(value.Identity, "binding", revisions); err != nil {
		return Binding{}, err
	}
	serverID, err := localReference(value.Server.Ref, "#/servers/")
	if err != nil {
		return Binding{}, err
	}
	if !hasServer(servers, serverID) || !value.Implemented || value.WireCompatibility || len(value.Evidence) == 0 || !validTransport(value.Transport) {
		return Binding{}, publicError("ASYNCAPI_BINDING_UNPROVEN", errors.New("binding implementation evidence is incomplete"))
	}
	return Binding{ID: id, NaatreID: value.Identity.NaatreID, Revision: value.Identity.Revision, Transport: value.Transport, Server: serverID, Implemented: true, Evidence: slices.Clone(value.Evidence)}, nil
}

func importOperations(values map[string]wireOperation, revisions Revisions, model Model, graph map[string][]string) ([]Operation, error) {
	result := make([]Operation, 0, len(values))
	context := importContext{revisions: revisions, model: model, graph: graph}
	for _, id := range sortedMapKeys(values) {
		operation, err := importOperation(id, values[id], context)
		if err != nil {
			return nil, err
		}
		result = append(result, operation)
	}
	return result, nil
}

func importOperation(id string, value wireOperation, context importContext) (Operation, error) {
	if err := validateWireIdentity(value.Identity, "operation", context.revisions); err != nil {
		return Operation{}, err
	}
	channelID, err := localReference(value.Channel.Ref, "#/channels/")
	if err != nil {
		return Operation{}, err
	}
	if !hasChannel(context.model.Channels, channelID) || value.Action != ActionSend && value.Action != ActionReceive {
		return Operation{}, publicError("ASYNCAPI_OPERATION_INVALID", errors.New("operation action or channel is invalid"))
	}
	result := Operation{ID: id, NaatreID: value.Identity.NaatreID, Revision: value.Identity.Revision, Action: value.Action, Channel: channelID}
	context.graph["operation:"+id] = append(context.graph["operation:"+id], "channel:"+channelID)
	if err := importOperationMessages(id, value.Messages, context, &result); err != nil {
		return Operation{}, err
	}
	if err := importOperationSecurity(id, value.Security, context, &result); err != nil {
		return Operation{}, err
	}
	if len(result.Messages) == 0 || len(result.Security) == 0 {
		return Operation{}, publicError("ASYNCAPI_OPERATION_INVALID", errors.New("operation messages and security are required"))
	}
	return result, nil
}

func importOperationMessages(id string, values []wireReference, context importContext, result *Operation) error {
	for _, reference := range values {
		messageID, err := localReference(reference.Ref, "#/components/messages/")
		if err != nil {
			return err
		}
		if !hasMessage(context.model.Messages, messageID) {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("operation message reference is invalid"))
		}
		result.Messages = append(result.Messages, messageID)
		context.graph["operation:"+id] = append(context.graph["operation:"+id], "message:"+messageID)
	}
	return nil
}

func importOperationSecurity(id string, values []wireReference, context importContext, result *Operation) error {
	for _, reference := range values {
		securityID, err := localReference(reference.Ref, "#/components/securitySchemes/")
		if err != nil {
			return err
		}
		if !hasSecurity(context.model.Security, securityID) {
			return publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("operation security reference is invalid"))
		}
		result.Security = append(result.Security, securityID)
		context.graph["operation:"+id] = append(context.graph["operation:"+id], "security:"+securityID)
	}
	return nil
}

func credentialSafeExample(example []byte, limits Limits) bool {
	jsonLimits := protocol.Limits{MaxBytes: limits.MaxExampleBytes, MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxObjects}
	canonical, err := protocol.CanonicalizeJSON(example, jsonLimits)
	if err != nil {
		return false
	}
	redacted, err := protocol.CanonicalizeJSON([]byte(tooling.RedactCredentials(string(example))), jsonLimits)
	return err == nil && bytes.Equal(canonical, redacted)
}

func validateWireIdentity(value Identity, kind string, revisions Revisions) error {
	if value.Kind != kind || !identifierPattern.MatchString(value.NaatreID) || !identifierPattern.MatchString(value.Revision) ||
		value.Schema != revisions.Schema || value.Capability != revisions.Capabilities || value.Canonicalization != revisions.Canonicalization || value.EventEnvelope != revisions.EventEnvelope {
		return publicError("ASYNCAPI_IDENTITY_INVALID", errors.New("declaration identity is missing or revision binding differs"))
	}
	return nil
}

func normalizeAllowedServers(values []string) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for _, raw := range values {
		parsed, err := parseServerURL(raw, strings.SplitN(raw, ":", 2)[0])
		if err != nil {
			return nil, err
		}
		normalized := parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
		result[normalized] = true
	}
	return result, nil
}

func localReference(value, prefix string) (string, error) {
	if !strings.HasPrefix(value, "#/") {
		return "", publicError("ASYNCAPI_REFERENCE_BLOCKED", errors.New("remote references are disabled"))
	}
	if !strings.HasPrefix(value, prefix) {
		return "", publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("local reference targets an unsupported component"))
	}
	name := strings.TrimPrefix(value, prefix)
	if !identifierPattern.MatchString(name) {
		return "", publicError("ASYNCAPI_REFERENCE_INVALID", errors.New("local reference identity is invalid"))
	}
	return name, nil
}

func detectReferenceCycles(graph map[string][]string, maximum int) error {
	count := 0
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(node string) error {
		if state[node] == 1 {
			return publicError("ASYNCAPI_REFERENCE_CYCLE", errors.New("reference cycle detected"))
		}
		if state[node] == 2 {
			return nil
		}
		state[node] = 1
		for _, child := range graph[node] {
			count++
			if count > maximum {
				return publicError("ASYNCAPI_REFERENCE_LIMIT", errors.New("reference count exceeds configured limit"))
			}
			if err := visit(child); err != nil {
				return err
			}
		}
		state[node] = 2
		return nil
	}
	for node := range graph {
		if err := visit(node); err != nil {
			return err
		}
	}
	return nil
}

func resolveLimits(value Limits) (Limits, error) {
	if value == (Limits{}) {
		return DefaultLimits(), nil
	}
	if value.MaxDocumentBytes < 1 || value.MaxDepth < 1 || value.MaxDepth > 64 || value.MaxObjects < 1 || value.MaxChannels < 1 || value.MaxMessages < 1 || value.MaxReferences < 1 || value.MaxExampleBytes < 1 {
		return Limits{}, errors.New("all AsyncAPI limits must be positive and depth must not exceed 64")
	}
	return value, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("document has trailing JSON values")
	}
	return nil
}

func importFailure(report FidelityReport, code, feature, path string, cause error) (Model, FidelityReport, error) {
	report.Status = "rejected"
	report.Diagnostics = []Diagnostic{{Code: code, Path: path, Feature: feature, Message: "AsyncAPI import rejected unsafe, lossy, or unsupported metadata"}}
	return Model{}, report, publicError(code, cause)
}

func importErrorFeature(err error) string {
	switch errorCode(err, "") {
	case "ASYNCAPI_SERVER_DENIED", "ASYNCAPI_SERVER_INVALID", "ASYNCAPI_SERVER_UNSUPPORTED":
		return "network-egress"
	case "ASYNCAPI_REFERENCE_BLOCKED", "ASYNCAPI_REFERENCE_INVALID", "ASYNCAPI_REFERENCE_CYCLE", "ASYNCAPI_REFERENCE_LIMIT":
		return "references"
	case "ASYNCAPI_BINDING_UNPROVEN", "ASYNCAPI_BINDING_INVALID":
		return "transport-bindings"
	case "ASYNCAPI_CORRELATION_INVALID":
		return "correlation-identifiers"
	case "ASYNCAPI_SECURITY_INVALID", "ASYNCAPI_SECURITY_UNSUPPORTED":
		return "authorization-security"
	default:
		return "document"
	}
}

func sortedMapKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func hasServer(values []Server, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}
func hasMessage(values []Message, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}
func hasChannel(values []Channel, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}
func hasSecurity(values []SecurityScheme, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}
