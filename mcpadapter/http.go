package mcpadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

type HTTPAuthorize func(context.Context, *http.Request) error

type StreamableHTTPClientConfig struct {
	Endpoint   string
	Profile    TransportConfig
	HTTPClient *http.Client
	Authorize  HTTPAuthorize
	Limits     WireLimits
}

type StreamableHTTPClient struct {
	endpoint  *url.URL
	client    *http.Client
	authorize HTTPAuthorize
	limits    WireLimits
	profile   TransportConfig

	mu        sync.Mutex
	sessionID string
}

func NewStreamableHTTPClient(config StreamableHTTPClientConfig) (*StreamableHTTPClient, error) {
	limits, err := normalizeWireLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	if _, err := DescribeTransport(config.Profile); err != nil {
		return nil, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("HTTP client requires a valid trusted transport profile"))
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, adapterError("MCP_HTTP_ENDPOINT_INVALID", errors.New("streamable HTTP requires one exact HTTPS endpoint"))
	}
	if config.Profile.Kind != StreamableHTTP || config.Profile.Endpoint != endpoint.String() || config.Profile.MaxResponseBytes != limits.MaxResponseBytes || config.Profile.MaxPending != limits.MaxPending {
		return nil, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("HTTP wire settings differ from the trusted transport profile"))
	}
	if config.Authorize == nil {
		return nil, adapterError(CodeUnauthorized, errors.New("streamable HTTP requires an explicit credential hook"))
	}
	base := http.DefaultClient
	if config.HTTPClient != nil {
		base = config.HTTPClient
	}
	client := *base
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &StreamableHTTPClient{endpoint: endpoint, client: &client, authorize: config.Authorize, limits: limits, profile: cloneTransportConfig(config.Profile)}, nil
}

func (*StreamableHTTPClient) Kind() Transport { return StreamableHTTP }

// ProfileConfig returns a defensive copy of the trusted core transport
// configuration enforced by this client.
func (c *StreamableHTTPClient) ProfileConfig() TransportConfig {
	if c == nil {
		return TransportConfig{}
	}
	return cloneTransportConfig(c.profile)
}

func (c *StreamableHTTPClient) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	if c == nil || ctx == nil || len(payload) == 0 || len(payload) > c.limits.MaxRequestBytes {
		return nil, adapterError(CodeRequestInvalid, errors.New("HTTP request frame is absent or oversized"))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, adapterError(CodeRequestInvalid, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", c.endpoint.Scheme+"://"+c.endpoint.Host)
	c.mu.Lock()
	if c.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.mu.Unlock()
	if c.authorize(ctx, request) != nil {
		return nil, adapterError(CodeUnauthorized, errors.New("HTTP authorization failed"))
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, adapterError(CodeCancelled, errors.New("HTTP request was cancelled"))
		}
		return nil, adapterError(CodeTransportFailed, errors.New("HTTP exchange failed"))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, adapterError(CodeUnauthorized, errors.New("HTTP authorization failed"))
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, adapterError(CodeTransportFailed, errors.New("HTTP status is outside the profile"))
	}
	content, err := readBoundedBody(response.Body, c.limits.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	if mediaType(response.Header.Get("Content-Type")) != "application/json" {
		return nil, adapterError(CodeResponseInvalid, errors.New("HTTP response media type is unsupported"))
	}
	if sessionID := response.Header.Get("Mcp-Session-Id"); sessionID != "" {
		if !boundedIdentity(sessionID) {
			return nil, adapterError(CodeSessionInvalid, errors.New("HTTP session identifier is invalid"))
		}
		c.mu.Lock()
		if c.sessionID != "" && c.sessionID != sessionID {
			c.mu.Unlock()
			return nil, adapterError(CodeSessionInvalid, errors.New("HTTP session identifier changed"))
		}
		c.sessionID = sessionID
		c.mu.Unlock()
	}
	return content, nil
}

type HTTPAuthenticate func(context.Context, *http.Request) (Identity, error)

type StreamableHTTPHandlerConfig struct {
	Server         *Server
	AllowedOrigins []string
	Authenticate   HTTPAuthenticate
}

type StreamableHTTPHandler struct {
	server         *Server
	allowedOrigins []string
	authenticate   HTTPAuthenticate

	mu       sync.Mutex
	sessions map[string]httpSession
}

type httpSession struct {
	identity   Identity
	connection *Connection
}

func NewStreamableHTTPHandler(config StreamableHTTPHandlerConfig) (*StreamableHTTPHandler, error) {
	if config.Server == nil || config.Server.manifest.Transport != StreamableHTTP || config.Authenticate == nil || len(config.AllowedOrigins) == 0 || len(config.AllowedOrigins) > 256 {
		return nil, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("HTTP handler requires server, origins, and authentication"))
	}
	origins := slices.Clone(config.AllowedOrigins)
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("allowed origins must be exact HTTPS origins"))
		}
	}
	slices.Sort(origins)
	for index := 1; index < len(origins); index++ {
		if origins[index-1] == origins[index] {
			return nil, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("allowed origins must be unique"))
		}
	}
	expectedOrigins := slices.Clone(config.Server.transport.AllowedOrigins)
	slices.Sort(expectedOrigins)
	if !slices.Equal(origins, expectedOrigins) {
		return nil, adapterError("MCP_HTTP_PROFILE_INVALID", errors.New("HTTP handler origins differ from the trusted transport profile"))
	}
	return &StreamableHTTPHandler{server: config.Server, allowedOrigins: origins, authenticate: config.Authenticate, sessions: make(map[string]httpSession)}, nil
}

func (h *StreamableHTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost || mediaType(request.Header.Get("Content-Type")) != "application/json" {
		h.writeHTTPError(writer, http.StatusUnsupportedMediaType, rpcInvalidRequest, CodeRequestInvalid)
		return
	}
	if !slices.Contains(h.allowedOrigins, request.Header.Get("Origin")) {
		h.writeHTTPError(writer, http.StatusForbidden, rpcUnauthorized, CodeUnauthorized)
		return
	}
	identity, err := h.authenticate(request.Context(), request)
	if err != nil || !identity.valid() {
		h.writeHTTPError(writer, http.StatusUnauthorized, rpcUnauthorized, CodeUnauthorized)
		return
	}
	payload, err := readBoundedBody(request.Body, h.server.limits.MaxRequestBytes)
	if err != nil {
		h.writeHTTPError(writer, http.StatusRequestEntityTooLarge, rpcLimitExceeded, CodeResourceLimit)
		return
	}
	rpcRequest, err := decodeRPCRequest(payload, h.server.limits)
	if err != nil {
		h.writeHTTPError(writer, http.StatusBadRequest, rpcInvalidRequest, CodeRequestInvalid)
		return
	}
	sessionID := request.Header.Get("Mcp-Session-Id")
	connection, created, err := h.connection(sessionID, identity, rpcRequest.Method == "initialize" && !rpcRequest.notification())
	if err != nil {
		h.writeHTTPError(writer, http.StatusBadRequest, rpcInvalidRequest, CodeSessionInvalid)
		return
	}
	if created {
		sessionID = h.sessionID(connection)
	}
	response, emit := connection.Handle(request.Context(), payload)
	if created {
		if !emit {
			h.CloseSession(sessionID)
		} else if _, responseErr := decodeRPCResponse(response, rpcRequest.ID, h.server.limits); responseErr != nil {
			h.CloseSession(sessionID)
		} else {
			writer.Header().Set("Mcp-Session-Id", sessionID)
		}
	}
	if !emit {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	_, _ = writer.Write(response)
}

func (h *StreamableHTTPHandler) CloseSession(sessionID string) {
	h.mu.Lock()
	session := h.sessions[sessionID]
	delete(h.sessions, sessionID)
	h.mu.Unlock()
	if session.connection != nil {
		session.connection.Close()
	}
}

func (h *StreamableHTTPHandler) connection(sessionID string, identity Identity, initialize bool) (*Connection, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sessionID != "" {
		session, ok := h.sessions[sessionID]
		if !ok || session.identity != identity || initialize {
			return nil, false, errors.New("invalid session")
		}
		return session.connection, false, nil
	}
	if !initialize || len(h.sessions) >= h.server.limits.MaxSessions {
		return nil, false, errors.New("session required or capacity exhausted")
	}
	connection, err := h.server.NewConnection(identity)
	if err != nil {
		return nil, false, err
	}
	for attempts := 0; attempts < 8; attempts++ {
		id, err := randomSessionID()
		if err != nil {
			return nil, false, err
		}
		if _, exists := h.sessions[id]; !exists {
			h.sessions[id] = httpSession{identity: identity, connection: connection}
			return connection, true, nil
		}
	}
	return nil, false, errors.New("session allocation failed")
}

func (h *StreamableHTTPHandler) sessionID(connection *Connection) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, session := range h.sessions {
		if session.connection == connection {
			return id
		}
	}
	return ""
}

func (h *StreamableHTTPHandler) writeHTTPError(writer http.ResponseWriter, status int, rpcCode int64, publicCode string) {
	writer.WriteHeader(status)
	response, _ := marshalRPC(rpcResponse{JSONRPC: JSONRPCVersion, ID: nullRPCID(), Error: publicRPCError(rpcCode, publicCode)}, h.server.limits.MaxResponseBytes, CodeResponseTooLarge)
	_, _ = writer.Write(response)
}

func randomSessionID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func readBoundedBody(reader io.Reader, maximum int) ([]byte, error) {
	if reader == nil || maximum < 1 {
		return nil, adapterError(CodeResponseTooLarge, errors.New("body limit is invalid"))
	}
	content, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil {
		return nil, adapterError(CodeTransportFailed, errors.New("body read failed"))
	}
	if len(content) > maximum {
		return nil, adapterError(CodeResponseTooLarge, errors.New("body exceeds the configured limit"))
	}
	return content, nil
}

func mediaType(value string) string {
	parsed, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	for name, parameter := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(parameter, "utf-8") {
			return ""
		}
	}
	return strings.ToLower(parsed)
}
