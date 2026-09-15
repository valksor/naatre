package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
)

type ClientConfig struct {
	Core      Config
	Transport ClientTransport
	Info      PeerInfo
	Limits    WireLimits
}

type Client struct {
	transport     ClientTransport
	info          PeerInfo
	limits        WireLimits
	coreLimits    Limits
	registrations map[string]Registration
	nextID        atomic.Uint64
	callMu        sync.Mutex
	evidence      RuntimeEvidence
}

// Connect performs initialization and bounded discovery before compiling any
// runtime definition. Remote catalog data never becomes a registration.
func Connect(ctx context.Context, config ClientConfig) (*Client, *Compiled, FidelityReport, error) {
	limits, err := normalizeWireLimits(config.Limits)
	if err != nil {
		return nil, nil, FidelityReport{}, err
	}
	if config.Transport == nil || !boundedIdentity(config.Info.Name) || !boundedIdentity(config.Info.Version) || config.Core.Direction != Consume || config.Core.Transport.Kind != config.Transport.Kind() {
		return nil, nil, FidelityReport{}, adapterError("MCP_CLIENT_PROFILE_INVALID", errors.New("client configuration is incomplete or inconsistent"))
	}
	if err := validateRuntimeRequirements(config.Core.RequiredCapabilities); err != nil {
		return nil, nil, FidelityReport{}, err
	}
	for _, registration := range config.Core.Registrations {
		if registration.Kind == ResourceTemplateKind {
			return nil, nil, FidelityReport{}, adapterError(CodeCapabilityUnsupported, errors.New("resource template runtime calls are unsupported"))
		}
	}
	if described, ok := config.Transport.(interface{ ProfileConfig() TransportConfig }); ok && !reflect.DeepEqual(described.ProfileConfig(), config.Core.Transport) {
		return nil, nil, FidelityReport{}, adapterError("MCP_CLIENT_PROFILE_INVALID", errors.New("transport implementation differs from the trusted core configuration"))
	}
	client := &Client{
		transport: config.Transport, info: config.Info, limits: limits,
		coreLimits: withDefaultLimits(config.Core.Limits), registrations: make(map[string]Registration),
		evidence: runtimeEvidence(Consume, config.Transport.Kind(), limits),
	}
	capabilities, err := client.initialize(ctx)
	if err != nil {
		return nil, nil, FidelityReport{}, err
	}
	catalog, err := client.discover(ctx, capabilities)
	if err != nil {
		return nil, nil, FidelityReport{}, err
	}
	core := config.Core
	core.Catalog = catalog
	core.Registrations = slices.Clone(config.Core.Registrations)
	for index := range core.Registrations {
		registration := core.Registrations[index]
		registration.Invoker = client
		core.Registrations[index] = registration
		client.registrations[registrationKey(registration.Kind, registration.RemoteID)] = registration
	}
	compiled, report, err := Compile(core)
	if err != nil {
		return nil, nil, report, err
	}
	return client, compiled, report, nil
}

func (c *Client) Evidence() RuntimeEvidence {
	if c == nil {
		return RuntimeEvidence{}
	}
	return cloneEvidence(c.evidence)
}

func (c *Client) initialize(ctx context.Context) (map[string]json.RawMessage, error) {
	params := struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      PeerInfo       `json:"clientInfo"`
	}{MCPRevision, map[string]any{}, c.info}
	result, err := c.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var initialized struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      PeerInfo                   `json:"serverInfo"`
	}
	if decodeStrictParams(result, &initialized, c.limits.MaxResponseBytes) != nil || initialized.ProtocolVersion != MCPRevision || !boundedIdentity(initialized.ServerInfo.Name) || !boundedIdentity(initialized.ServerInfo.Version) || len(initialized.Capabilities) > 64 {
		return nil, adapterError(CodeCapabilityUnsupported, errors.New("server initialization response is outside the profile"))
	}
	if err := c.notify(ctx, "notifications/initialized", struct{}{}); err != nil {
		return nil, err
	}
	return initialized.Capabilities, nil
}

func (c *Client) discover(ctx context.Context, capabilities map[string]json.RawMessage) (Catalog, error) {
	catalog := Catalog{ProtocolVersion: MCPRevision, Capabilities: []string{"cancellation"}}
	if _, ok := capabilities["tools"]; ok {
		catalog.Capabilities = append(catalog.Capabilities, "structured-content", "tools")
		tools, err := c.listTools(ctx)
		if err != nil {
			return Catalog{}, err
		}
		catalog.Tools = tools
	}
	if _, ok := capabilities["resources"]; ok {
		catalog.Capabilities = append(catalog.Capabilities, "resources")
		resources, err := c.listResources(ctx)
		if err != nil {
			return Catalog{}, err
		}
		catalog.Resources = resources
	}
	if _, ok := capabilities["prompts"]; ok {
		catalog.Capabilities = append(catalog.Capabilities, "prompts")
	}
	sort.Strings(catalog.Capabilities)
	encoded, err := json.Marshal(catalog)
	if err != nil || len(encoded) > c.coreLimits.MaxCatalogBytes {
		return Catalog{}, adapterError("MCP_CATALOG_LIMIT", errors.New("discovered catalog exceeds the configured bound"))
	}
	return catalog, nil
}

func (c *Client) listTools(ctx context.Context) ([]Tool, error) {
	var result []Tool
	seen := make(map[string]bool)
	cursor := ""
	for page := 0; page < c.limits.MaxPages; page++ {
		response, err := c.call(ctx, "tools/list", cursorParams(cursor))
		if err != nil {
			return nil, err
		}
		var listing struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor,omitempty"`
		}
		if decodeStrictParams(response, &listing, c.limits.MaxResponseBytes) != nil || len(listing.NextCursor) > c.coreLimits.MaxIdentifier || seen[listing.NextCursor] && listing.NextCursor != "" {
			return nil, adapterError(CodeResponseInvalid, errors.New("tool listing is invalid"))
		}
		result = append(result, listing.Tools...)
		if len(result) > c.coreLimits.MaxEntries {
			return nil, adapterError(CodeResourceLimit, errors.New("tool listing exceeds the configured entry limit"))
		}
		if listing.NextCursor == "" {
			return result, nil
		}
		seen[listing.NextCursor] = true
		cursor = listing.NextCursor
	}
	return nil, adapterError(CodeResourceLimit, errors.New("tool listing exceeds the configured page limit"))
}

func (c *Client) listResources(ctx context.Context) ([]Resource, error) {
	var result []Resource
	seen := make(map[string]bool)
	cursor := ""
	for page := 0; page < c.limits.MaxPages; page++ {
		response, err := c.call(ctx, "resources/list", cursorParams(cursor))
		if err != nil {
			return nil, err
		}
		var listing struct {
			Resources  []Resource `json:"resources"`
			NextCursor string     `json:"nextCursor,omitempty"`
		}
		if decodeStrictParams(response, &listing, c.limits.MaxResponseBytes) != nil || len(listing.NextCursor) > c.coreLimits.MaxIdentifier || seen[listing.NextCursor] && listing.NextCursor != "" {
			return nil, adapterError(CodeResponseInvalid, errors.New("resource listing is invalid"))
		}
		result = append(result, listing.Resources...)
		if len(result) > c.coreLimits.MaxEntries {
			return nil, adapterError(CodeResourceLimit, errors.New("resource listing exceeds the configured entry limit"))
		}
		if listing.NextCursor == "" {
			return result, nil
		}
		seen[listing.NextCursor] = true
		cursor = listing.NextCursor
	}
	return nil, adapterError(CodeResourceLimit, errors.New("resource listing exceeds the configured page limit"))
}

func cursorParams(cursor string) any {
	if cursor == "" {
		return struct{}{}
	}
	return struct {
		Cursor string `json:"cursor"`
	}{cursor}
}

// CallTool preserves the complete canonical Naatre envelope, including
// partial data and safe error paths.
func (c *Client) CallTool(ctx context.Context, name string, arguments []byte) ([]byte, error) {
	if c == nil {
		return nil, adapterError(CodeToolUnknown, errors.New("client is nil"))
	}
	registration, ok := c.registrations[registrationKey(ToolKind, name)]
	if !ok {
		return nil, adapterError(CodeToolUnknown, errors.New("tool is not registered"))
	}
	canonical, err := ValidateInstance(registration.InputSchema, arguments, c.coreLimits)
	if err != nil {
		return nil, adapterError(CodeValueInvalid, err)
	}
	result, err := c.call(ctx, "tools/call", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{name, canonical})
	if err != nil {
		return nil, err
	}
	var toolResult ToolResult
	if decodeStrictParams(result, &toolResult, c.limits.MaxResponseBytes) != nil {
		return nil, adapterError(CodeResponseInvalid, errors.New("tool result envelope is invalid"))
	}
	if toolResult.IsError {
		return nil, adapterError(safeToolFailureCode(toolResult.Content), errors.New("remote tool failed"))
	}
	canonical, err = DecodeStructuredResult(toolResult, c.coreLimits.MaxContentBytes)
	if err != nil {
		return nil, adapterError(CodeResponseInvalid, err)
	}
	if _, err := ValidateInstance(registration.OutputSchema, canonical, c.coreLimits); err != nil {
		return nil, adapterError(CodeResponseInvalid, err)
	}
	return canonical, nil
}

func (c *Client) ReadResource(ctx context.Context, uri string) (ResourceContent, error) {
	if c == nil {
		return ResourceContent{}, adapterError(CodeResourceUnknown, errors.New("client is nil"))
	}
	if _, ok := c.registrations[registrationKey(ResourceKind, uri)]; !ok {
		return ResourceContent{}, adapterError(CodeResourceUnknown, errors.New("resource is not registered"))
	}
	result, err := c.call(ctx, "resources/read", struct {
		URI string `json:"uri"`
	}{uri})
	if err != nil {
		return ResourceContent{}, err
	}
	var response struct {
		Contents []ResourceContent `json:"contents"`
	}
	if decodeStrictParams(result, &response, c.limits.MaxResponseBytes) != nil || len(response.Contents) != 1 || !validResourceContent(response.Contents[0], uri, c.limits.MaxResponseBytes) {
		return ResourceContent{}, adapterError(CodeResponseInvalid, errors.New("resource response is invalid"))
	}
	return response.Contents[0], nil
}

// Invoke implements interopadapter.Invoker for ordinary Naatre registration.
// Partial envelopes stay available through CallTool and are never silently
// cast into a complete domain value.
func (c *Client) Invoke(ctx context.Context, request interopadapter.BackendRequest) (map[string]any, error) {
	if registration, ok := c.registrations[registrationKey(ToolKind, request.Operation)]; ok {
		envelope, err := c.CallTool(ctx, registration.RemoteID, request.Input)
		if err != nil {
			return nil, err
		}
		value, err := protocol.DecodeJSONValue(envelope)
		if err != nil {
			return nil, adapterError(CodeResponseInvalid, err)
		}
		object := value.(map[string]any)
		complete, _ := object["complete"].(bool)
		errorsValue, _ := object["errors"].([]any)
		if !complete || len(errorsValue) != 0 {
			return nil, adapterError(CodePartialResult, errors.New("partial result requires the envelope API"))
		}
		data, ok := object["data"].(map[string]any)
		if !ok {
			return nil, adapterError(CodeResponseInvalid, errors.New("complete result data is not an object"))
		}
		return data, nil
	}
	if registration, ok := c.registrations[registrationKey(ResourceKind, request.Operation)]; ok {
		content, err := c.ReadResource(ctx, registration.RemoteID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"uri": content.URI, "mimeType": content.MIMEType, "text": content.Text, "blob": content.Blob}, nil
	}
	return nil, adapterError(CodeMethodUnsupported, errors.New("registration kind is not callable"))
}

func (c *Client) call(ctx context.Context, method string, params any) ([]byte, error) {
	if c == nil || ctx == nil {
		return nil, adapterError(CodeTransportFailed, errors.New("client or context is nil"))
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, adapterError(CodeRequestInvalid, err)
	}
	id := numericRPCID(c.nextID.Add(1))
	request, err := marshalRPC(rpcRequest{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: encodedParams}, c.limits.MaxRequestBytes, CodeRequestInvalid)
	if err != nil {
		return nil, err
	}
	c.callMu.Lock()
	responseBytes, err := c.transport.RoundTrip(ctx, request)
	c.callMu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return nil, adapterError(CodeCancelled, errors.New("MCP request was cancelled"))
		}
		return nil, adapterError(CodeOf(err), errors.New("MCP transport failed"))
	}
	response, err := decodeRPCResponse(responseBytes, id, c.limits)
	if err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return adapterError(CodeRequestInvalid, err)
	}
	request, err := marshalRPC(rpcRequest{JSONRPC: JSONRPCVersion, Method: method, Params: encodedParams}, c.limits.MaxRequestBytes, CodeRequestInvalid)
	if err != nil {
		return err
	}
	c.callMu.Lock()
	response, err := c.transport.RoundTrip(ctx, request)
	c.callMu.Unlock()
	if err != nil {
		return adapterError(CodeOf(err), errors.New("MCP notification failed"))
	}
	if len(response) != 0 {
		return adapterError(CodeResponseInvalid, errors.New("notification returned a response"))
	}
	return nil
}

func registrationKey(kind EntryKind, id string) string { return string(kind) + "\x00" + id }

func safeToolFailureCode(content []Content) string {
	if len(content) == 1 && content[0].Type == "text" && validPublicCode(content[0].Text) {
		return content[0].Text
	}
	return CodeToolFailed
}
