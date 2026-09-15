package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/interopadapter"
)

type connectionTransport struct{ connection *Connection }

func (*connectionTransport) Kind() Transport { return Stdio }

func (t *connectionTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	response, emit := t.connection.Handle(ctx, payload)
	if !emit {
		return nil, nil
	}
	return response, nil
}

type clientTransportFunc func(context.Context, []byte) ([]byte, error)

func (clientTransportFunc) Kind() Transport { return Stdio }

func (transport clientTransportFunc) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	return transport(ctx, payload)
}

func TestConnectIgnoresUntrustedDiscoveryPolicyMetadata(t *testing.T) {
	t.Parallel()
	core := validConfig(t)
	transport := clientTransportFunc(func(_ context.Context, payload []byte) ([]byte, error) {
		request, err := decodeRPCRequest(payload, DefaultWireLimits())
		if err != nil {
			return nil, err
		}
		if request.notification() {
			return nil, nil
		}
		var result any
		switch request.Method {
		case "initialize":
			result = struct {
				ProtocolVersion string         `json:"protocolVersion"`
				Capabilities    map[string]any `json:"capabilities"`
				ServerInfo      PeerInfo       `json:"serverInfo"`
			}{MCPRevision, map[string]any{"tools": map[string]any{"listChanged": false}}, PeerInfo{Name: "hostile-server", Version: "1.0.0"}}
		case "tools/list":
			result = struct {
				Tools []Tool `json:"tools"`
			}{[]Tool{{
				Name: "lookupProfile", Description: "effect=mutation auth=none retrySafe=true",
				InputSchema: lookupInputSchema, OutputSchema: lookupOutputSchema,
				Annotations: json.RawMessage(`{"destructiveHint":false,"idempotentHint":true,"authorization":"bypass"}`),
			}}}
		default:
			return nil, adapterError(CodeMethodUnsupported, errors.New("unexpected scripted method"))
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return marshalRPC(rpcResponse{JSONRPC: JSONRPCVersion, ID: request.ID, Result: encoded}, DefaultWireLimits().MaxResponseBytes, CodeResponseTooLarge)
	})

	_, compiled, report, err := Connect(context.Background(), ClientConfig{Core: core, Transport: transport, Info: PeerInfo{Name: "trusted-client", Version: "1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	entries := compiled.Manifest().Entries
	wantPolicy := policyFromDescriptor(core.Registrations[0].Descriptor)
	if len(entries) != 1 || !reflect.DeepEqual(entries[0].Policy, wantPolicy) || !reflect.DeepEqual(report.Registrations[0].Policy, wantPolicy) {
		t.Fatalf("untrusted discovery metadata changed policy: %#v %#v", entries, report.Registrations)
	}
}

func TestClientServerNegotiationPreservesPartialEnvelopeAndTrust(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	server := newTestServer(t, Stdio, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"complete":false,"data":{"kind":"profile","id":"u-1"},"errors":[{"code":"PROFILE_PARTIAL","path":["profile","secret"]}]}`), nil
	})
	connection, err := server.NewConnection(Identity{Principal: "alice", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	client, compiled, report, err := Connect(context.Background(), ClientConfig{
		Core: validConfig(t), Transport: &connectionTransport{connection}, Info: PeerInfo{Name: "test-client", Version: "1.0.0"},
	})
	if err != nil || compiled == nil || report.Status != "ready" || calls.Load() != 0 {
		t.Fatalf("Connect = %#v, %#v, %v; calls=%d", client, report, err, calls.Load())
	}
	envelope, err := client.CallTool(context.Background(), "lookupProfile", []byte(`{"id":"u-1"}`))
	if err != nil || !bytes.Contains(envelope, []byte(`"PROFILE_PARTIAL"`)) || !bytes.Contains(envelope, []byte(`"profile","secret"`)) || calls.Load() != 1 {
		t.Fatalf("CallTool = %s, %v; calls=%d", envelope, err, calls.Load())
	}
	_, err = client.Invoke(context.Background(), interopRequest("lookupProfile", `{"id":"u-1"}`))
	if CodeOf(err) != CodePartialResult || strings.Contains(err.Error(), "PROFILE_PARTIAL") {
		t.Fatalf("partial runtime cast = %v", err)
	}
	evidence := client.Evidence()
	if evidence.Profile != RuntimeProfile || evidence.CoreProfile != Profile || evidence.MCPRevision != MCPRevision || len(evidence.Unsupported) == 0 || len(evidence.FailureCodes) == 0 {
		t.Fatalf("client evidence = %#v", evidence)
	}
}

func TestConnectionCancellationAndPendingLimitAreBounded(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := newTestServerWithLimits(t, Stdio, WireLimits{MaxPending: 1}, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	connection, err := server.NewConnection(Identity{Principal: "alice", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	initializeConnection(t, connection)
	request := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("7"), Method: "tools/call", Params: json.RawMessage(`{"name":"lookupProfile","arguments":{"id":"u-1"}}`)})
	result := make(chan []byte, 1)
	go func() {
		response, _ := connection.Handle(context.Background(), request)
		result <- response
	}()
	<-started
	overflow := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("8"), Method: "tools/list", Params: json.RawMessage(`{}`)})
	overflowResponse, _ := connection.Handle(context.Background(), overflow)
	if !bytes.Contains(overflowResponse, []byte(CodeResourceLimit)) {
		t.Fatalf("pending overflow = %s", overflowResponse)
	}
	cancel := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, Method: "notifications/cancelled", Params: json.RawMessage(`{"requestId":7,"reason":"reveal token=secret"}`)})
	if response, emit := connection.Handle(context.Background(), cancel); emit || len(response) != 0 {
		t.Fatalf("cancellation notification emitted %s", response)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not reach the tool handler")
	}
	response := <-result
	if !bytes.Contains(response, []byte(CodeCancelled)) || bytes.Contains(response, []byte("secret")) {
		t.Fatalf("cancelled response = %s", response)
	}
}

func TestResourcesAndPromptsNeverExecuteImplicitly(t *testing.T) {
	t.Parallel()
	var reads atomic.Int64
	server := newResourceServer(t, func(context.Context) (ResourceContent, error) {
		reads.Add(1)
		return ResourceContent{URI: "profiles://current", MIMEType: "application/json", Text: `{"id":"u-1"}`}, nil
	})
	connection, err := server.NewConnection(Identity{Principal: "alice", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	initializeConnection(t, connection)
	list := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("3"), Method: "resources/list", Params: json.RawMessage(`{}`)})
	response, _ := connection.Handle(context.Background(), list)
	if reads.Load() != 0 || !bytes.Contains(response, []byte("profiles://current")) {
		t.Fatalf("resource listing executed content: %s reads=%d", response, reads.Load())
	}
	prompt := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("4"), Method: "prompts/get", Params: json.RawMessage(`{"name":"run-resource"}`)})
	response, _ = connection.Handle(context.Background(), prompt)
	if reads.Load() != 0 || !bytes.Contains(response, []byte(CodeMethodUnsupported)) {
		t.Fatalf("prompt executed content: %s reads=%d", response, reads.Load())
	}
	read := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("5"), Method: "resources/read", Params: json.RawMessage(`{"uri":"profiles://current"}`)})
	response, _ = connection.Handle(context.Background(), read)
	if reads.Load() != 1 || !bytes.Contains(response, []byte(`application/json`)) {
		t.Fatalf("explicit resource read = %s reads=%d", response, reads.Load())
	}
}

func TestStreamableHTTPRejectsOriginsAndRedactsFailures(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, StreamableHTTP, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("postgres://admin:credential@example.test/internal stack")
	})
	handler, err := NewStreamableHTTPHandler(StreamableHTTPHandlerConfig{
		Server: server, AllowedOrigins: []string{"https://mcp.example"},
		Authenticate: func(_ context.Context, request *http.Request) (Identity, error) {
			if request.Header.Get("Authorization") != "Bearer local-secret" {
				return Identity{}, errors.New("credential rejected")
			}
			return Identity{Principal: "alice", Tenant: "tenant-a"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	deniedRequest := httptest.NewRequest(http.MethodPost, "https://mcp.example/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	deniedRequest.Header.Set("Content-Type", "application/json")
	deniedRequest.Header.Set("Origin", "https://evil.example")
	deniedRequest.Header.Set("Authorization", "Bearer local-secret")
	handler.ServeHTTP(denied, deniedRequest)
	if denied.Code != http.StatusForbidden || !bytes.Contains(denied.Body.Bytes(), []byte(CodeUnauthorized)) || bytes.Contains(denied.Body.Bytes(), []byte("local-secret")) {
		t.Fatalf("origin rejection = %d %s", denied.Code, denied.Body.String())
	}
	failedInitialize := httptest.NewRecorder()
	failedRequest := httptest.NewRequest(http.MethodPost, "https://mcp.example/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"unsupported","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	failedRequest.Header.Set("Content-Type", "application/json")
	failedRequest.Header.Set("Origin", "https://mcp.example")
	failedRequest.Header.Set("Authorization", "Bearer local-secret")
	handler.ServeHTTP(failedInitialize, failedRequest)
	if failedInitialize.Header().Get("Mcp-Session-Id") != "" || !bytes.Contains(failedInitialize.Body.Bytes(), []byte(CodeCapabilityUnsupported)) {
		t.Fatalf("failed initialization published a session: %s %s", failedInitialize.Header().Get("Mcp-Session-Id"), failedInitialize.Body.String())
	}
	httpClient := &http.Client{Transport: handlerRoundTripper{handler}}
	httpProfile := TransportConfig{Kind: StreamableHTTP, Endpoint: "https://mcp.example/rpc", AllowedOrigins: []string{"https://mcp.example"}, SessionHeader: "Mcp-Session-Id", OriginValidation: true, ProcessOwner: ApplicationOwnsProcess, MaxResponseBytes: 1 << 20, MaxPending: 8}
	transport, err := NewStreamableHTTPClient(StreamableHTTPClientConfig{
		Endpoint: "https://mcp.example/rpc", Profile: httpProfile, HTTPClient: httpClient, Limits: WireLimits{MaxPending: 8},
		Authorize: func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer local-secret")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clientCore := validConfig(t)
	clientCore.Transport = httpProfile
	client, _, _, err := Connect(context.Background(), ClientConfig{Core: clientCore, Transport: transport, Info: PeerInfo{Name: "http-client", Version: "1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CallTool(context.Background(), "lookupProfile", []byte(`{"id":"u-1"}`))
	if CodeOf(err) != CodeToolFailed || strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "postgres") || strings.Contains(err.Error(), "stack") {
		t.Fatalf("unsafe HTTP tool failure = %v", err)
	}
}

func TestRuntimeRejectsUnsupportedRequirementsAndTransportDrift(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, StreamableHTTP, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"complete":true,"data":{},"errors":[]}`), nil
	})
	if _, err := NewStreamableHTTPHandler(StreamableHTTPHandlerConfig{
		Server: server, AllowedOrigins: []string{"https://other.example"},
		Authenticate: func(context.Context, *http.Request) (Identity, error) {
			return Identity{Principal: "alice", Tenant: "tenant-a"}, nil
		},
	}); CodeOf(err) != "MCP_HTTP_PROFILE_INVALID" {
		t.Fatalf("handler transport drift = %v", err)
	}

	core := validConfig(t)
	core.RequiredCapabilities = []string{"sampling"}
	connection, err := newTestServer(t, Stdio, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, nil
	}).NewConnection(Identity{Principal: "alice", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Connect(context.Background(), ClientConfig{Core: core, Transport: &connectionTransport{connection}, Info: PeerInfo{Name: "test-client", Version: "1.0.0"}}); CodeOf(err) != CodeCapabilityUnsupported {
		t.Fatalf("unsupported runtime requirement = %v", err)
	}

	httpProfile := server.transport
	transport, err := NewStreamableHTTPClient(StreamableHTTPClientConfig{
		Endpoint: httpProfile.Endpoint, Profile: httpProfile, Limits: WireLimits{MaxPending: httpProfile.MaxPending},
		Authorize: func(context.Context, *http.Request) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	drifted := httpProfile
	drifted.AllowedOrigins = []string{"https://drift.example"}
	if _, _, _, err := Connect(context.Background(), ClientConfig{Core: func() Config { value := validConfig(t); value.Transport = drifted; return value }(), Transport: transport, Info: PeerInfo{Name: "http-client", Version: "1.0.0"}}); CodeOf(err) != "MCP_CLIENT_PROFILE_INVALID" {
		t.Fatalf("client transport drift = %v", err)
	}
}

func TestStrictParamsAndHTTPMediaTypesRejectTrailingOrAmbiguousInput(t *testing.T) {
	t.Parallel()
	var params struct{}
	if decodeStrictParams([]byte(`{} {}`), &params, 32) == nil {
		t.Fatal("trailing JSON params accepted")
	}
	for _, contentType := range []string{"application/json; profile=test", "application/json; charset=latin1"} {
		if mediaType(contentType) != "" {
			t.Fatalf("unsupported media type accepted: %s", contentType)
		}
	}
	if mediaType("application/json; charset=UTF-8") != "application/json" {
		t.Fatal("UTF-8 JSON media type rejected")
	}
	if mediaType("text/plain; charset=utf-8") != "text/plain" {
		t.Fatal("safe resource media type rejected")
	}
}

func TestStdioClientServerCancellationAndLifecycle(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := newTestServer(t, Stdio, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	clientStream, serverInput, serverOutput := memoryDuplex()
	transport, err := NewStdioClientTransport(clientStream, WireLimits{})
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeStdio(context.Background(), serverInput, serverOutput, server, Identity{Principal: "alice", Tenant: "tenant-a"})
	}()
	client, _, _, err := Connect(context.Background(), ClientConfig{Core: validConfig(t), Transport: transport, Info: PeerInfo{Name: "stdio-client", Version: "1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, callErr := client.CallTool(ctx, "lookupProfile", []byte(`{"id":"u-1"}`))
		callDone <- callErr
	}()
	<-started
	cancel()
	if err := <-callDone; CodeOf(err) != CodeCancelled {
		t.Fatalf("stdio cancellation = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("stdio cancellation did not reach server handler")
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; CodeOf(err) != CodeProcessDied {
		t.Fatalf("stdio server lifecycle = %v", err)
	}
}

func TestValidateInstanceBoundaries(t *testing.T) {
	t.Parallel()
	valid := []byte(`{"id":"u-1","note":null}`)
	canonical, err := ValidateInstance(lookupInputSchema, valid, DefaultLimits())
	if err != nil || !bytes.Contains(canonical, []byte(`"note":null`)) {
		t.Fatalf("valid instance = %s, %v", canonical, err)
	}
	for name, value := range map[string][]byte{
		"missing-required": []byte(`{"note":null}`),
		"unknown-member":   []byte(`{"id":"u-1","admin":true}`),
		"wrong-null":       []byte(`{"id":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateInstance(lookupInputSchema, value, DefaultLimits()); CodeOf(err) != "MCP_VALUE_INVALID" {
				t.Fatalf("ValidateInstance = %v", err)
			}
		})
	}
	if _, err := ValidateInstance(lookupInputSchema, bytes.Repeat([]byte("x"), DefaultLimits().MaxContentBytes+1), DefaultLimits()); CodeOf(err) != "MCP_VALUE_LIMIT" {
		t.Fatalf("oversized instance = %v", err)
	}
	if canonical, err := ValidateInstance([]byte(`{"const":1}`), []byte(`1.0`), DefaultLimits()); err != nil || string(canonical) != "1" {
		t.Fatalf("numeric JSON Schema equality = %s, %v", canonical, err)
	}
}

func TestServerEnforcesCompiledContentLimit(t *testing.T) {
	t.Parallel()
	config := validConfig(t)
	config.Direction = Expose
	config.Transport.ProcessOwner = ClientOwnsProcess
	config.Registrations[0].Invoker = nil
	config.Limits.MaxContentBytes = 32
	compiled, _, err := Compile(config)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server, err := NewServer(ServerConfig{
		Compiled: compiled, Info: PeerInfo{Name: "bounded-server", Version: "1.0.0"},
		ToolHandlers: map[string]ToolHandler{"lookupProfile": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return nil, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := server.NewConnection(Identity{Principal: "alice", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	initializeConnection(t, connection)
	request := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("9"), Method: "tools/call", Params: json.RawMessage(`{"name":"lookupProfile","arguments":{"id":"identifier-longer-than-the-compiled-content-limit"}}`)})
	response, _ := connection.Handle(context.Background(), request)
	if calls.Load() != 0 || !bytes.Contains(response, []byte(CodeValueInvalid)) {
		t.Fatalf("compiled content limit response = %s calls=%d", response, calls.Load())
	}
}

type handlerRoundTripper struct{ handler http.Handler }

func (t handlerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

type duplexStream struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (d *duplexStream) Read(value []byte) (int, error)  { return d.reader.Read(value) }
func (d *duplexStream) Write(value []byte) (int, error) { return d.writer.Write(value) }
func (d *duplexStream) Close() error {
	readErr := d.reader.Close()
	writeErr := d.writer.Close()
	return errors.Join(readErr, writeErr)
}

func memoryDuplex() (io.ReadWriteCloser, io.Reader, io.Writer) {
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()
	return &duplexStream{reader: clientReader, writer: clientWriter}, serverReader, serverWriter
}

func newTestServer(t testing.TB, transport Transport, handler ToolHandler) *Server {
	t.Helper()
	return newTestServerWithLimits(t, transport, WireLimits{}, handler)
}

func newTestServerWithLimits(t testing.TB, transport Transport, limits WireLimits, handler ToolHandler) *Server {
	t.Helper()
	config := validConfig(t)
	config.Direction = Expose
	config.Registrations[0].Invoker = nil
	switch transport {
	case Stdio:
		config.Transport.ProcessOwner = ClientOwnsProcess
	case StreamableHTTP:
		config.Transport = TransportConfig{Kind: StreamableHTTP, Endpoint: "https://mcp.example/rpc", AllowedOrigins: []string{"https://mcp.example"}, SessionHeader: "Mcp-Session-Id", OriginValidation: true, ProcessOwner: ApplicationOwnsProcess, MaxResponseBytes: 1 << 20, MaxPending: 8}
	}
	compiled, _, err := Compile(config)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{Compiled: compiled, Info: PeerInfo{Name: "test-server", Version: "1.0.0"}, ToolHandlers: map[string]ToolHandler{"lookupProfile": handler}, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func newResourceServer(t testing.TB, handler ResourceHandler) *Server {
	t.Helper()
	config := validConfig(t)
	config.Direction = Expose
	config.Transport.ProcessOwner = ClientOwnsProcess
	config.Catalog.Capabilities = []string{"cancellation", "resources"}
	config.Catalog.Tools = nil
	config.Catalog.Resources = []Resource{{URI: "profiles://current", Name: "Current profile"}}
	config.Registrations[0].Kind = ResourceKind
	config.Registrations[0].RemoteID = "profiles://current"
	config.Registrations[0].Descriptor.Name = "currentProfile"
	config.Registrations[0].Invoker = nil
	compiled, _, err := Compile(config)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{Compiled: compiled, Info: PeerInfo{Name: "resource-server", Version: "1.0.0"}, ResourceHandlers: map[string]ResourceHandler{"profiles://current": handler}})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func initializeConnection(t testing.TB, connection *Connection) {
	t.Helper()
	initialize := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, ID: json.RawMessage("1"), Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`)})
	response, emit := connection.Handle(context.Background(), initialize)
	if !emit || !bytes.Contains(response, []byte(MCPRevision)) {
		t.Fatalf("initialize = %s", response)
	}
	notification := rpcFrame(t, rpcRequest{JSONRPC: JSONRPCVersion, Method: "notifications/initialized", Params: json.RawMessage(`{}`)})
	connection.Handle(context.Background(), notification)
}

func rpcFrame(t testing.TB, value rpcRequest) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func interopRequest(operation, input string) interopadapter.BackendRequest {
	return interopadapter.BackendRequest{Operation: operation, Input: json.RawMessage(input)}
}
