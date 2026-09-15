package mcpadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"sync"
)

type ServerConfig struct {
	Compiled         *Compiled
	Info             PeerInfo
	ToolHandlers     map[string]ToolHandler
	ResourceHandlers map[string]ResourceHandler
	Limits           WireLimits
}

type Server struct {
	info       PeerInfo
	manifest   Manifest
	tools      map[string]serverTool
	resources  map[string]serverResource
	limits     WireLimits
	coreLimits Limits
	evidence   RuntimeEvidence
	transport  TransportConfig
}

type serverTool struct {
	entry  ManifestEntry
	handle ToolHandler
}

type serverResource struct {
	entry  ManifestEntry
	handle ResourceHandler
}

func NewServer(config ServerConfig) (*Server, error) {
	limits, err := normalizeWireLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	if config.Compiled == nil || config.Compiled.report.Direction != Expose {
		return nil, adapterError("MCP_SERVER_PROFILE_INVALID", errors.New("server requires a compiled exposure profile"))
	}
	if err := validateRuntimeRequirements(config.Compiled.requiredCapabilities); err != nil {
		return nil, err
	}
	if !boundedIdentity(config.Info.Name) || !boundedIdentity(config.Info.Version) {
		return nil, adapterError("MCP_SERVER_PROFILE_INVALID", errors.New("server identity must be bounded"))
	}
	server := &Server{
		info: config.Info, manifest: config.Compiled.Manifest(), limits: limits, coreLimits: config.Compiled.limits,
		tools: make(map[string]serverTool), resources: make(map[string]serverResource),
		evidence: runtimeEvidence(Expose, config.Compiled.manifest.Transport, limits), transport: cloneTransportConfig(config.Compiled.transport),
	}
	for _, entry := range server.manifest.Entries {
		switch entry.Kind {
		case ToolKind:
			handler := config.ToolHandlers[entry.RemoteID]
			if handler == nil {
				return nil, adapterError("MCP_SERVER_HANDLER_REQUIRED", errors.New("every exposed tool requires an exact handler"))
			}
			server.tools[entry.RemoteID] = serverTool{entry: entry, handle: handler}
		case ResourceKind:
			handler := config.ResourceHandlers[entry.RemoteID]
			if handler == nil {
				return nil, adapterError("MCP_SERVER_HANDLER_REQUIRED", errors.New("every exposed resource requires an exact handler"))
			}
			server.resources[entry.RemoteID] = serverResource{entry: entry, handle: handler}
		case ResourceTemplateKind:
			return nil, adapterError(CodeCapabilityUnsupported, errors.New("resource template runtime dispatch is not supported by this profile"))
		default:
			return nil, adapterError("MCP_SERVER_PROFILE_INVALID", errors.New("manifest contains an unknown entry kind"))
		}
	}
	if len(config.ToolHandlers) != len(server.tools) || len(config.ResourceHandlers) != len(server.resources) {
		return nil, adapterError("MCP_SERVER_HANDLER_UNAPPROVED", errors.New("handlers must match the trusted manifest exactly"))
	}
	return server, nil
}

func (s *Server) Evidence() RuntimeEvidence {
	if s == nil {
		return RuntimeEvidence{}
	}
	return cloneEvidence(s.evidence)
}

func (s *Server) NewConnection(identity Identity) (*Connection, error) {
	if s == nil || !identity.valid() {
		return nil, adapterError(CodeSessionInvalid, errors.New("connection identity must be explicit and bounded"))
	}
	return &Connection{server: s, identity: identity, pending: make(chan struct{}, s.limits.MaxPending), active: make(map[string]context.CancelFunc)}, nil
}

type Connection struct {
	server   *Server
	identity Identity

	mu          sync.Mutex
	negotiated  bool
	initialized bool
	closed      bool
	active      map[string]context.CancelFunc
	pending     chan struct{}
}

func (c *Connection) Handle(ctx context.Context, input []byte) ([]byte, bool) {
	if c == nil || c.server == nil || ctx == nil {
		return nil, false
	}
	request, err := decodeRPCRequest(input, c.server.limits)
	if err != nil {
		return c.errorResponse(nullRPCID(), rpcInvalidRequest, CodeRequestInvalid), true
	}
	if request.Method == "notifications/cancelled" {
		c.cancelRequest(request.Params)
		return nil, false
	}
	if !c.acquire() {
		if request.notification() {
			return nil, false
		}
		return c.errorResponse(request.ID, rpcLimitExceeded, CodeResourceLimit), true
	}
	defer c.release()
	if request.notification() {
		c.handleNotification(request)
		return nil, false
	}
	requestCtx, cancel, ok := c.begin(ctx, request.ID)
	if !ok {
		return c.errorResponse(request.ID, rpcInvalidRequest, CodeRequestInvalid), true
	}
	defer c.finish(request.ID, cancel)
	return c.dispatch(requestCtx, request), true
}

func (c *Connection) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	active := make([]context.CancelFunc, 0, len(c.active))
	for _, cancel := range c.active {
		active = append(active, cancel)
	}
	clear(c.active)
	c.mu.Unlock()
	for _, cancel := range active {
		cancel()
	}
}

func (c *Connection) acquire() bool {
	select {
	case c.pending <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *Connection) release() { <-c.pending }

func (c *Connection) begin(parent context.Context, id []byte) (context.Context, context.CancelFunc, bool) {
	key := rpcIDKey(id)
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || key == "" || c.active[key] != nil {
		cancel()
		return nil, nil, false
	}
	c.active[key] = cancel
	return ctx, cancel, true
}

func (c *Connection) finish(id []byte, cancel context.CancelFunc) {
	key := rpcIDKey(id)
	c.mu.Lock()
	delete(c.active, key)
	c.mu.Unlock()
	cancel()
}

func (c *Connection) cancelRequest(input []byte) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason,omitempty"`
	}
	if decodeStrictParams(input, &params, c.server.limits.MaxRequestBytes) != nil || !validRPCID(params.RequestID) {
		return
	}
	c.mu.Lock()
	cancel := c.active[rpcIDKey(params.RequestID)]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Connection) handleNotification(request rpcRequest) {
	if request.Method != "notifications/initialized" {
		return
	}
	c.mu.Lock()
	if c.negotiated && !c.closed {
		c.initialized = true
	}
	c.mu.Unlock()
}

func (c *Connection) dispatch(ctx context.Context, request rpcRequest) []byte {
	if request.Method == "initialize" {
		return c.initialize(request)
	}
	c.mu.Lock()
	ready := c.initialized && !c.closed
	c.mu.Unlock()
	if !ready {
		return c.errorResponse(request.ID, rpcInvalidRequest, CodeNotInitialized)
	}
	if ctx.Err() != nil {
		return c.errorResponse(request.ID, rpcCancelled, CodeCancelled)
	}
	switch request.Method {
	case "tools/list":
		return c.listTools(request)
	case "tools/call":
		return c.callTool(ctx, request)
	case "resources/list":
		return c.listResources(request)
	case "resources/read":
		return c.readResource(ctx, request)
	default:
		return c.errorResponse(request.ID, rpcMethodNotFound, CodeMethodUnsupported)
	}
}

func (c *Connection) initialize(request rpcRequest) []byte {
	var params struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ClientInfo      PeerInfo                   `json:"clientInfo"`
	}
	if decodeStrictParams(request.Params, &params, c.server.limits.MaxRequestBytes) != nil || params.ProtocolVersion != MCPRevision || !boundedIdentity(params.ClientInfo.Name) || !boundedIdentity(params.ClientInfo.Version) || len(params.Capabilities) > 64 {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeCapabilityUnsupported)
	}
	c.mu.Lock()
	if c.negotiated || c.closed {
		c.mu.Unlock()
		return c.errorResponse(request.ID, rpcInvalidRequest, CodeRequestInvalid)
	}
	c.negotiated = true
	c.mu.Unlock()
	capabilities := make(map[string]any)
	if len(c.server.tools) != 0 {
		capabilities["tools"] = map[string]any{"listChanged": false}
	}
	if len(c.server.resources) != 0 {
		capabilities["resources"] = map[string]any{"subscribe": false, "listChanged": false}
	}
	result := struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      PeerInfo       `json:"serverInfo"`
	}{MCPRevision, capabilities, c.server.info}
	return c.resultResponse(request.ID, result)
}

func (c *Connection) listTools(request rpcRequest) []byte {
	if len(c.server.tools) == 0 {
		return c.errorResponse(request.ID, rpcMethodNotFound, CodeCapabilityUnsupported)
	}
	if !emptyCursor(request.Params, c.server.limits.MaxRequestBytes) {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeRequestInvalid)
	}
	type listedTool struct {
		Name         string          `json:"name"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	names := sortedServerKeys(c.server.tools)
	tools := make([]listedTool, 0, len(names))
	for _, name := range names {
		entry := c.server.tools[name].entry
		tools = append(tools, listedTool{Name: name, InputSchema: slices.Clone(entry.InputSchema), OutputSchema: slices.Clone(entry.OutputSchema)})
	}
	return c.resultResponse(request.ID, struct {
		Tools []listedTool `json:"tools"`
	}{tools})
}

func (c *Connection) callTool(ctx context.Context, request rpcRequest) []byte {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if decodeStrictParams(request.Params, &params, c.server.limits.MaxRequestBytes) != nil {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeRequestInvalid)
	}
	tool, exists := c.server.tools[params.Name]
	if !exists {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeToolUnknown)
	}
	arguments, err := ValidateInstance(tool.entry.InputSchema, params.Arguments, c.server.coreLimits)
	if err != nil {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeValueInvalid)
	}
	result, err := tool.handle(ctx, arguments)
	if err != nil || ctx.Err() != nil {
		code := CodeToolFailed
		if ctx.Err() != nil {
			code = CodeCancelled
		}
		return c.resultResponse(request.ID, errorToolResult(code))
	}
	canonical, err := ValidateInstance(tool.entry.OutputSchema, result, c.server.coreLimits)
	if err != nil {
		return c.resultResponse(request.ID, errorToolResult(CodeToolFailed))
	}
	encoded, err := EncodeStructuredResult(canonical, c.server.limits.MaxResponseBytes)
	if err != nil {
		return c.resultResponse(request.ID, errorToolResult(CodeResponseTooLarge))
	}
	return c.resultResponse(request.ID, encoded)
}

func errorToolResult(code string) any {
	return struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}{Content: []Content{{Type: "text", Text: code}}, IsError: true}
}

func (c *Connection) listResources(request rpcRequest) []byte {
	if len(c.server.resources) == 0 {
		return c.errorResponse(request.ID, rpcMethodNotFound, CodeCapabilityUnsupported)
	}
	if !emptyCursor(request.Params, c.server.limits.MaxRequestBytes) {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeRequestInvalid)
	}
	type listedResource struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	}
	names := sortedServerKeys(c.server.resources)
	resources := make([]listedResource, 0, len(names))
	for _, name := range names {
		resources = append(resources, listedResource{URI: name, Name: c.server.resources[name].entry.NaatreName})
	}
	return c.resultResponse(request.ID, struct {
		Resources []listedResource `json:"resources"`
	}{resources})
}

func (c *Connection) readResource(ctx context.Context, request rpcRequest) []byte {
	var params struct {
		URI string `json:"uri"`
	}
	if decodeStrictParams(request.Params, &params, c.server.limits.MaxRequestBytes) != nil {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeRequestInvalid)
	}
	resource, exists := c.server.resources[params.URI]
	if !exists {
		return c.errorResponse(request.ID, rpcInvalidParams, CodeResourceUnknown)
	}
	content, err := resource.handle(ctx)
	if err != nil || ctx.Err() != nil {
		code := CodeResourceFailed
		if ctx.Err() != nil {
			code = CodeCancelled
		}
		return c.errorResponse(request.ID, rpcInternalError, code)
	}
	maximum := min(c.server.coreLimits.MaxContentBytes, c.server.limits.MaxResponseBytes)
	if !validResourceContent(content, params.URI, maximum) {
		return c.errorResponse(request.ID, rpcInternalError, CodeResponseInvalid)
	}
	return c.resultResponse(request.ID, struct {
		Contents []ResourceContent `json:"contents"`
	}{[]ResourceContent{content}})
}

func validResourceContent(content ResourceContent, uri string, maximum int) bool {
	if content.URI != uri || len(content.MIMEType) == 0 || len(content.MIMEType) > 256 || mediaType(content.MIMEType) == "" || (content.Text == "") == (content.Blob == "") || len(content.Text) > maximum || len(content.Blob) > maximum*2 {
		return false
	}
	if content.Blob != "" {
		decoded, err := base64.StdEncoding.Strict().DecodeString(content.Blob)
		return err == nil && len(decoded) <= maximum
	}
	return true
}

func (c *Connection) resultResponse(id json.RawMessage, result any) []byte {
	encodedResult, err := json.Marshal(result)
	if err != nil || len(encodedResult) > c.server.limits.MaxResponseBytes {
		return c.errorResponse(id, rpcLimitExceeded, CodeResponseTooLarge)
	}
	response, err := marshalRPC(rpcResponse{JSONRPC: JSONRPCVersion, ID: slices.Clone(id), Result: encodedResult}, c.server.limits.MaxResponseBytes, CodeResponseTooLarge)
	if err != nil {
		response, _ = json.Marshal(rpcResponse{JSONRPC: JSONRPCVersion, ID: slices.Clone(id), Error: publicRPCError(rpcLimitExceeded, CodeResponseTooLarge)})
	}
	return response
}

func (c *Connection) errorResponse(id json.RawMessage, rpcCode int64, publicCode string) []byte {
	response, _ := marshalRPC(rpcResponse{JSONRPC: JSONRPCVersion, ID: slices.Clone(id), Error: publicRPCError(rpcCode, publicCode)}, c.server.limits.MaxResponseBytes, CodeResponseTooLarge)
	return response
}

func decodeStrictParams(input []byte, output any, maximum int) error {
	if len(input) == 0 || len(input) > maximum {
		return errors.New("invalid params")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing params")
	}
	return nil
}

func emptyCursor(input []byte, maximum int) bool {
	if len(input) == 0 {
		return true
	}
	var params struct {
		Cursor string `json:"cursor,omitempty"`
	}
	return decodeStrictParams(input, &params, maximum) == nil && params.Cursor == ""
}

func sortedServerKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
