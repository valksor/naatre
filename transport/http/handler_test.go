package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	naatrecbor "github.com/valksor/naatre/protocol/cbor"
	"github.com/valksor/naatre/runtime"
)

const validRequest = `{"version":"1","id":"client-1","document":{"operations":[{"name":"Ping","kind":"query","select":[{"$call":{"name":"ping"}}]}]}}`

func TestHandlerRequiresAuthenticationChallenge(t *testing.T) {
	t.Parallel()
	_, err := NewHandler(Config{
		Executor:     func(context.Context, *protocol.Request) runtime.Outcome { return runtime.Outcome{} },
		Authenticate: func(context.Context, *stdhttp.Request) (context.Context, error) { return nil, errors.New("denied") },
	})
	if err == nil {
		t.Fatal("constructed an authenticated handler without a challenge")
	}
}

func TestHandlerResponseWithoutCapabilitiesDecodes(t *testing.T) {
	t.Parallel()
	response := serve(testHandler(t, Config{}), newRequest(validRequest))
	if response.Code != stdhttp.StatusOK {
		t.Fatalf("response = %d body=%s", response.Code, response.Body.String())
	}
	if _, err := protocol.DecodeResponse(response.Body.Bytes(), protocol.DecodeOptions{}); err != nil {
		t.Fatalf("DecodeResponse() error = %v body=%s", err, response.Body.String())
	}
}

func TestHandlerStatusMediaEncodingAndProtocolParity(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, Config{})

	for _, protocolVersion := range []struct {
		name  string
		proto string
		major int
		minor int
	}{{"HTTP/1.1", "HTTP/1.1", 1, 1}, {"HTTP/2", "HTTP/2.0", 2, 0}, {"HTTP/3", "HTTP/3.0", 3, 0}} {
		t.Run(protocolVersion.name, func(t *testing.T) {
			request := newRequest(validRequest)
			request.Proto, request.ProtoMajor, request.ProtoMinor = protocolVersion.proto, protocolVersion.major, protocolVersion.minor
			response := serve(handler, request)
			if response.Code != stdhttp.StatusOK || response.Header().Get("Content-Type") != ResponseMediaType || response.Header().Get("Content-Encoding") != "identity" {
				t.Fatalf("response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope["id"] != "client-1" || envelope["requestId"] != "server-1" {
				t.Fatalf("identity = %#v", envelope)
			}
		})
	}

	for _, testCase := range []struct {
		name, contentType, accept, code string
		status                          int
	}{{"missing media", "", "", "UNSUPPORTED_MEDIA_TYPE", 415}, {"generic media", "application/json", "", "UNSUPPORTED_MEDIA_TYPE", 415}, {"wrong version", "application/vnd.naatre.request+json;version=2", "", "UNSUPPORTED_MEDIA_TYPE", 415}, {"not acceptable", RequestMediaType, "application/vnd.naatre.response+json;version=2", "NOT_ACCEPTABLE", 406}} {
		t.Run(testCase.name, func(t *testing.T) {
			request := newRequest(validRequest)
			request.Header.Set("Content-Type", testCase.contentType)
			if testCase.accept != "" {
				request.Header.Set("Accept", testCase.accept)
			}
			assertProblem(t, serve(handler, request), testCase.status, testCase.code)
		})
	}

	request := newRequest(validRequest)
	request.Method = stdhttp.MethodGet
	response := serve(handler, request)
	assertProblem(t, response, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
	if response.Header().Get("Allow") != "POST, OPTIONS" || response.Header().Get("Location") != "" {
		t.Fatalf("method headers = %v", response.Header())
	}
}

func TestHandlerCBORNegotiationAndMalformedIsolation(t *testing.T) {
	t.Parallel()
	var invocations atomic.Int64
	handler := testHandler(t, Config{EnableCBOR: true, Executor: func(context.Context, *protocol.Request) runtime.Outcome {
		invocations.Add(1)
		return runtime.Outcome{Data: map[string]any{"value": "ok"}, Capabilities: []string{CBORCapability}}
	}})
	cborRequest := []byte(`{"version":"1","id":"client-1","capabilities":["` + CBORCapability + `"],"document":{"operations":[{"name":"Ping","kind":"query","select":[{"$call":{"name":"ping"}}]}]}}`)
	body, err := encodeCBORPayload(cborRequest)
	if err != nil {
		t.Fatal(err)
	}
	request := newRequestBytes(body)
	request.Header.Set("Content-Type", CBORRequestMediaType)
	request.Header.Set("Accept", CBORResponseMediaType)
	request.Header.Set("Naatre-Capabilities", CBORCapability)
	response := serve(handler, request)
	if response.Code != stdhttp.StatusOK || response.Header().Get("Content-Type") != CBORResponseMediaType {
		t.Fatalf("CBOR response = %d headers=%v body=%x", response.Code, response.Header(), response.Body.Bytes())
	}
	decoded, err := naatrecbor.DecodeJSON(response.Body.Bytes(), naatrecbor.Limits{})
	if err != nil || !strings.Contains(string(decoded), `"value":"ok"`) {
		t.Fatalf("decoded CBOR response = %s, %v", decoded, err)
	}
	if !varyContains(response.Header(), "Accept") || !varyContains(response.Header(), "Naatre-Capabilities") {
		t.Fatalf("CBOR response Vary = %v", response.Header().Values("Vary"))
	}

	request = newRequest(validRequest)
	request.Header.Set("Accept", CBORResponseMediaType)
	assertProblem(t, serve(handler, request), stdhttp.StatusNotAcceptable, "NOT_ACCEPTABLE")

	request = newRequest(validRequest)
	request.Header.Set("Accept", "application/*")
	response = serve(handler, request)
	if response.Header().Get("Content-Type") != ResponseMediaType {
		t.Fatalf("wildcard response media = %q", response.Header().Get("Content-Type"))
	}

	before := invocations.Load()
	request = newRequestBytes(body[:len(body)-1])
	request.Header.Set("Content-Type", CBORRequestMediaType)
	request.Header.Set("Naatre-Capabilities", CBORCapability)
	assertProblem(t, serve(handler, request), stdhttp.StatusBadRequest, "MALFORMED_CBOR")
	if invocations.Load() != before {
		t.Fatal("malformed CBOR invoked the executor")
	}

	disabled := testHandler(t, Config{})
	request = newRequestBytes(body)
	request.Header.Set("Content-Type", CBORRequestMediaType)
	request.Header.Set("Naatre-Capabilities", CBORCapability)
	assertProblem(t, serve(disabled, request), stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
}

func TestHandlerCompressedAndDecompressedLimits(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.MaxCompressedRequestBytes = 256
	limits.MaxDecompressedRequestBytes = int64(len(validRequest) + 16)
	handler := testHandler(t, Config{Limits: limits})

	compressed := gzipBytes(t, []byte(validRequest))
	request := newRequestBytes(compressed)
	request.Header.Set("Content-Encoding", "gzip")
	if response := serve(handler, request); response.Code != stdhttp.StatusOK {
		t.Fatalf("gzip response = %d %s", response.Code, response.Body.String())
	}

	request = newRequest(validRequest)
	request.Header.Set("Content-Encoding", "br")
	assertProblem(t, serve(handler, request), stdhttp.StatusUnsupportedMediaType, "UNSUPPORTED_CONTENT_ENCODING")

	request = newRequest(strings.Repeat("x", 257))
	assertProblem(t, serve(handler, request), stdhttp.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")

	request = newRequestBytes(gzipBytes(t, append([]byte(validRequest), bytes.Repeat([]byte(" "), 128)...)))
	request.Header.Set("Content-Encoding", "gzip")
	assertProblem(t, serve(handler, request), stdhttp.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")

	trailing := append(gzipBytes(t, []byte(validRequest)), gzipBytes(t, []byte(`{}`))...)
	request = newRequestBytes(trailing)
	request.Header.Set("Content-Encoding", "gzip")
	assertProblem(t, serve(handler, request), stdhttp.StatusBadRequest, "MALFORMED_JSON")
}

func TestHandlerResponseCompressionAndLimitFallback(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.CompressionMinBytes = 1
	handler := testHandler(t, Config{Limits: limits, Executor: func(context.Context, *protocol.Request) runtime.Outcome {
		return runtime.Outcome{Data: map[string]any{"value": strings.Repeat("a", 4096)}}
	}})
	request := newRequest(validRequest)
	request.Header.Set("Accept-Encoding", "gzip")
	response := serve(handler, request)
	if response.Code != stdhttp.StatusOK || response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response = %d headers=%v", response.Code, response.Header())
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(decoded, []byte(strings.Repeat("a", 64))) {
		t.Fatalf("decoded response error=%v body=%s", err, decoded)
	}

	request = newRequest(validRequest)
	request.Header.Set("Accept-Encoding", "gzip;q=0")
	if response = serve(handler, request); response.Header().Get("Content-Encoding") != "identity" {
		t.Fatalf("gzip q=0 encoding = %q", response.Header().Get("Content-Encoding"))
	}

	limits.MaxResponseBytes = 256
	handler = testHandler(t, Config{Limits: limits, Executor: func(context.Context, *protocol.Request) runtime.Outcome {
		return runtime.Outcome{Data: map[string]any{"value": strings.Repeat("secret", 4096)}}
	}})
	response = serve(handler, newRequest(validRequest))
	if response.Code != stdhttp.StatusInternalServerError || strings.Contains(response.Body.String(), "secret") || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
		t.Fatalf("bounded fallback = %d %s", response.Code, response.Body.String())
	}
}

func TestHandlerSlowClientDeadlineDisconnectAndShutdown(t *testing.T) {
	t.Parallel()

	body := newBlockingBody()
	handler := testHandler(t, Config{})
	request := newRequestBytes(nil)
	request.Body = body
	request.ContentLength = -1
	request.Header.Set("Naatre-Timeout-Ms", "10")
	response := serve(handler, request)
	assertProblem(t, response, stdhttp.StatusRequestTimeout, "REQUEST_TIMEOUT")
	if !body.wasClosed.Load() {
		t.Fatal("slow body was not closed on deadline")
	}

	started := make(chan struct{})
	cancelled := make(chan struct{})
	handler = testHandler(t, Config{Executor: func(ctx context.Context, _ *protocol.Request) runtime.Outcome {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return runtime.Outcome{}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	request = newRequest(validRequest).WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(handler, request) }()
	<-started
	cancel()
	response = <-done
	<-cancelled
	if response.Body.Len() != 0 {
		t.Fatalf("disconnect wrote a body: %s", response.Body.String())
	}

	handler = testHandler(t, Config{Executor: func(ctx context.Context, _ *protocol.Request) runtime.Outcome {
		<-ctx.Done()
		return runtime.Outcome{}
	}})
	request = newRequest(validRequest)
	request.Header.Set("Naatre-Timeout-Ms", "10")
	response = serve(handler, request)
	assertNaatreError(t, response, stdhttp.StatusGatewayTimeout, "RESOURCE_EXHAUSTED")

	shutdown := make(chan struct{})
	close(shutdown)
	handler = testHandler(t, Config{Shutdown: shutdown})
	assertProblem(t, serve(handler, newRequest(validRequest)), stdhttp.StatusServiceUnavailable, "OVERLOADED")

	shutdown = make(chan struct{})
	started = make(chan struct{})
	cancelled = make(chan struct{})
	handler = testHandler(t, Config{Shutdown: shutdown, Executor: func(ctx context.Context, _ *protocol.Request) runtime.Outcome {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return runtime.Outcome{}
	}})
	done = make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(handler, newRequest(validRequest)) }()
	<-started
	close(shutdown)
	response = <-done
	<-cancelled
	if response.Body.Len() != 0 {
		t.Fatalf("shutdown wrote a contradictory body: %s", response.Body.String())
	}
}

func TestHandlerCacheIsolationCORSAndRedirectSafety(t *testing.T) {
	t.Parallel()
	var executions atomic.Int64
	handler := testHandler(t, Config{CORS: CORS{AllowedOrigins: []string{"https://app.example"}, AllowCredentials: true}, Executor: func(context.Context, *protocol.Request) runtime.Outcome {
		executions.Add(1)
		return runtime.Outcome{Data: map[string]any{"ok": true}}
	}})
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		request := newRequest(validRequest)
		request.Header.Set("Naatre-Tenant", tenant)
		response := serve(handler, request)
		if response.Header().Get("Cache-Control") != "no-store" || !varyContains(response.Header(), "Accept-Encoding") || strings.Contains(response.Body.String(), tenant) {
			t.Fatalf("cache isolation failed for %s: headers=%v body=%s", tenant, response.Header(), response.Body.String())
		}
	}

	preflight := httptest.NewRequest(stdhttp.MethodOptions, DefaultPath, nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", stdhttp.MethodPost)
	preflight.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	response := serve(handler, preflight)
	if response.Code != stdhttp.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != "https://app.example" || executions.Load() != 2 {
		t.Fatalf("preflight = %d headers=%v executions=%d", response.Code, response.Header(), executions.Load())
	}

	request := newRequest(validRequest)
	request.Header.Set("Origin", "https://evil.example")
	assertProblem(t, serve(handler, request), stdhttp.StatusForbidden, "CORS_FORBIDDEN")
	if executions.Load() != 2 {
		t.Fatalf("denied origin executed callback %d times", executions.Load())
	}

	request = newRequest(validRequest)
	request.URL.Path = "/elsewhere"
	response = serve(handler, request)
	assertProblem(t, response, stdhttp.StatusNotFound, "NOT_FOUND")
	if response.Header().Get("Location") != "" {
		t.Fatalf("unexpected redirect: %v", response.Header())
	}
}

func TestHandlerRedactsAuthenticationAndExecutionDetails(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, Config{WWWAuthenticate: `Bearer realm="naatre"`, Authenticate: func(context.Context, *stdhttp.Request) (context.Context, error) {
		return nil, errors.New("Bearer credential-password")
	}})
	response := serve(handler, newRequest(validRequest))
	assertProblem(t, response, stdhttp.StatusUnauthorized, "UNAUTHENTICATED")
	if strings.Contains(response.Body.String(), "credential-password") || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("authentication response = %v %s", response.Header(), response.Body.String())
	}

	handler = testHandler(t, Config{Executor: func(context.Context, *protocol.Request) runtime.Outcome {
		return runtime.Outcome{Errors: []runtime.ExecutionError{{Code: "DATABASE_PASSWORD_FAILURE", Message: "postgres://user:secret@protected/resource", Details: map[string]any{"token": "secret"}}}}
	}})
	response = serve(handler, newRequest(validRequest))
	if response.Code != stdhttp.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
		t.Fatalf("unsafe execution mapping = %d %s", response.Code, response.Body.String())
	}
	for _, protected := range []string{"DATABASE_PASSWORD_FAILURE", "postgres", "secret", "protected", "token"} {
		if strings.Contains(response.Body.String(), protected) {
			t.Fatalf("response leaked %q: %s", protected, response.Body.String())
		}
	}
}

func TestHandlerRejectsMalformedHeadersAndCapabilities(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, Config{})
	request := newRequest(validRequest)
	request.Header.Add("Content-Type", RequestMediaType)
	assertProblem(t, serve(handler, request), stdhttp.StatusBadRequest, "MALFORMED_HEADER")

	request = newRequest(validRequest)
	request.Header.Set("Naatre-Principal", "forged")
	assertProblem(t, serve(handler, request), stdhttp.StatusBadRequest, "UNTRUSTED_IDENTITY_HEADER")

	request = newRequest(validRequest)
	request.Header.Set("Naatre-Capabilities", "feature.one")
	assertProblem(t, serve(handler, request), stdhttp.StatusBadRequest, "MALFORMED_HEADER")

	request = newRequest(validRequest)
	request.Header.Set("Accept-Encoding", "gzip;q=invalid")
	assertProblem(t, serve(handler, request), stdhttp.StatusBadRequest, "MALFORMED_HEADER")

	handler = testHandler(t, Config{Decode: protocol.DecodeOptions{Capabilities: map[string]bool{"feature.one": true}}})
	request = newRequest(`{"version":"1","capabilities":["feature.one"],"document":{"operations":[{"name":"Ping","kind":"query","select":[{"$call":{"name":"ping"}}]}]}}`)
	request.Header.Set("Naatre-Capabilities", "feature.one")
	response := serve(handler, request)
	if response.Code != stdhttp.StatusOK {
		t.Fatalf("matching capability response = %d %s", response.Code, response.Body.String())
	}
}

func TestHandlerMapsAdmissionFailuresToProblems(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		code   string
		status int
	}{{"RATE_LIMITED", stdhttp.StatusTooManyRequests}, {"OVERLOADED", stdhttp.StatusServiceUnavailable}} {
		handler := testHandler(t, Config{Executor: func(context.Context, *protocol.Request) runtime.Outcome {
			return runtime.Outcome{Errors: []runtime.ExecutionError{{Code: testCase.code, Message: "protected admission details"}}}
		}})
		response := serve(handler, newRequest(validRequest))
		assertProblem(t, response, testCase.status, testCase.code)
		if strings.Contains(response.Body.String(), "protected") {
			t.Fatalf("admission response leaked details: %s", response.Body.String())
		}
	}
}

func testHandler(t *testing.T, config Config) *Handler {
	t.Helper()
	if config.Executor == nil {
		config.Executor = func(context.Context, *protocol.Request) runtime.Outcome {
			return runtime.Outcome{Data: map[string]any{"ok": true}}
		}
	}
	if config.RequestID == nil {
		config.RequestID = func() string { return "server-1" }
	}
	handler, err := NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func newRequest(body string) *stdhttp.Request { return newRequestBytes([]byte(body)) }

func newRequestBytes(body []byte) *stdhttp.Request {
	request := httptest.NewRequest(stdhttp.MethodPost, DefaultPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", RequestMediaType)
	return request
}

func serve(handler stdhttp.Handler, request *stdhttp.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != ProblemMediaType || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("problem = %d headers=%v body=%s, want %d %s", response.Code, response.Header(), response.Body.String(), status, code)
	}
	for _, field := range []string{`"type":`, `"title":`, `"status":`} {
		if !strings.Contains(response.Body.String(), field) {
			t.Fatalf("problem lacks %s: %s", field, response.Body.String())
		}
	}
}

func assertNaatreError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != ResponseMediaType || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("Naatre error = %d headers=%v body=%s, want %d %s", response.Code, response.Header(), response.Body.String(), status, code)
	}
}

func gzipBytes(t *testing.T, input []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type blockingBody struct {
	closed    chan struct{}
	once      sync.Once
	wasClosed atomic.Bool
}

func newBlockingBody() *blockingBody { return &blockingBody{closed: make(chan struct{})} }
func (body *blockingBody) Read([]byte) (int, error) {
	<-body.closed
	return 0, errors.New("body closed")
}
func (body *blockingBody) Close() error {
	body.wasClosed.Store(true)
	body.once.Do(func() { close(body.closed) })
	return nil
}

func varyContains(header stdhttp.Header, value string) bool {
	var values []string
	for _, line := range header.Values("Vary") {
		for _, current := range strings.Split(line, ",") {
			values = append(values, strings.TrimSpace(current))
		}
	}
	return slices.Contains(values, value)
}
