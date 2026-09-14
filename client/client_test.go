package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

func TestExecutePreservesPartialDataUsesAuthAndSupportsGzip(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/execute" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Content-Type") != RequestMediaType || request.Header.Get("Accept") != ResponseMediaType {
			t.Errorf("media headers = %#v", request.Header)
		}
		if request.Header.Get("Authorization") != "Bearer local-test" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if _, err := protocol.DecodeRequest(body, protocol.DecodeOptions{}); err != nil {
			t.Errorf("DecodeRequest: %v", err)
		}
		writer.Header().Set("Content-Type", ResponseMediaType)
		writer.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(writer)
		_, _ = compressed.Write([]byte(`{"requestId":"server-1","data":{"value":"ok"},"errors":[{"code":"PARTIAL","message":"one field failed","path":["other"],"retryable":false}],"capabilities":[],"extensions":{}}`))
		_ = compressed.Close()
	}))
	defer server.Close()

	httpClient := *server.Client()
	client, err := New(Config{
		Endpoint:   server.URL + "/v1/execute",
		HTTPClient: &httpClient,
		Authenticate: func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer local-test")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := client.Execute(context.Background(), validRequest(t))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls.Load() != 1 || result.StatusCode != http.StatusOK {
		t.Fatalf("calls/status = %d/%d", calls.Load(), result.StatusCode)
	}
	data, present := result.Data()
	if !present || !bytes.Equal(data, []byte(`{"value":"ok"}`)) {
		t.Fatalf("data = %s, present %v", data, present)
	}
	if failures := result.Errors(); len(failures) != 1 || failures[0].Code() != "PARTIAL" {
		t.Fatalf("errors = %#v", failures)
	}
}

func TestExecuteRetainsProblemAndReturnsStableCode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", ProblemMediaType)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"type":"about:blank","title":"busy","status":429,"code":"RATE_LIMITED","detail":"do not echo this secret"}`))
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL + "/v1/execute", HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := client.Execute(context.Background(), validRequest(t))
	assertClientCode(t, err, "REMOTE_PROBLEM")
	if result == nil || result.Problem == nil || result.Problem.Code != "RATE_LIMITED" || result.Problem.Detail == "" {
		t.Fatalf("problem = %#v", result)
	}
	if bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Fatalf("error leaked remote detail: %v", err)
	}
}

func TestExecuteBoundsMalformedResponsesAndClosesBodies(t *testing.T) {
	t.Parallel()
	closed := atomic.Bool{}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{ResponseMediaType}},
			Body:       &trackedBody{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 65)), closed: &closed},
		}, nil
	})
	client, err := New(Config{
		Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport},
		MaxCompressedBytes: 64, MaxDecompressedBytes: 64,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Execute(context.Background(), validRequest(t))
	assertClientCode(t, err, "RESPONSE_LIMIT_EXCEEDED")
	if !closed.Load() {
		t.Fatal("response body was not closed")
	}

	malformed := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{ResponseMediaType}}, Body: io.NopCloser(bytes.NewBufferString(`{"data":`))}, nil
	})
	client, err = New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: malformed}})
	if err != nil {
		t.Fatalf("New malformed client: %v", err)
	}
	_, err = client.Execute(context.Background(), validRequest(t))
	assertClientCode(t, err, "MALFORMED_RESPONSE")
}

func TestExecuteEnforcesDeadlineAuthenticationAndMediaParameters(t *testing.T) {
	t.Parallel()
	deadlineTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client, err := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: deadlineTransport}})
	if err != nil {
		t.Fatalf("New deadline client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err = client.Execute(ctx, validRequest(t))
	assertClientCode(t, err, "DEADLINE_EXCEEDED")

	authFailure := errors.New("credential provider failed with protected material")
	client, err = New(Config{
		Endpoint: "https://example.test/v1/execute",
		Authenticate: func(context.Context, *http.Request) error {
			return authFailure
		},
	})
	if err != nil {
		t.Fatalf("New auth client: %v", err)
	}
	_, err = client.Execute(context.Background(), validRequest(t))
	assertClientCode(t, err, "AUTHENTICATION_FAILED")
	if strings.Contains(err.Error(), "protected") {
		t.Fatalf("authentication error leaked cause: %v", err)
	}

	missingVersion := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/vnd.naatre.response+json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"requestId":"s","data":{},"capabilities":[],"extensions":{}}`)),
		}, nil
	})
	client, err = New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: missingVersion}})
	if err != nil {
		t.Fatalf("New media client: %v", err)
	}
	_, err = client.Execute(context.Background(), validRequest(t))
	assertClientCode(t, err, "UNSUPPORTED_MEDIA_TYPE")
}

func TestExecuteRejectsDecompressionExpansion(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	_, _ = compressor.Write(bytes.Repeat([]byte("x"), 129))
	_ = compressor.Close()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":     []string{ResponseMediaType},
				"Content-Encoding": []string{"gzip"},
			},
			Body: io.NopCloser(bytes.NewReader(encoded.Bytes())),
		}, nil
	})
	client, err := New(Config{
		Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: transport},
		MaxCompressedBytes: 1024, MaxDecompressedBytes: 128,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Execute(context.Background(), validRequest(t))
	assertClientCode(t, err, "RESPONSE_LIMIT_EXCEEDED")
}

func TestExecuteCancellationAndRedirectCredentialPolicy(t *testing.T) {
	t.Parallel()
	canceled := make(chan struct{})
	started := make(chan struct{})
	cancelTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		close(canceled)
		return nil, request.Context().Err()
	})
	client, err := New(Config{Endpoint: "https://example.test/v1/execute", HTTPClient: &http.Client{Transport: cancelTransport}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	executionError := make(chan error, 1)
	operation := validRequest(t)
	go func() {
		_, executeErr := client.Execute(ctx, operation)
		executionError <- executeErr
	}()
	<-started
	cancel()
	err = <-executionError
	assertClientCode(t, err, "CANCELED")
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("server request context was not canceled")
	}

	receivedAuthorization := make(chan string, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAuthorization <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", ResponseMediaType)
		_, _ = writer.Write([]byte(`{"requestId":"redirected","data":{"ok":true},"capabilities":[],"extensions":{}}`))
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Redirect(writer, &http.Request{}, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	redirectClient := *origin.Client()
	client, err = New(Config{
		Endpoint:   origin.URL,
		HTTPClient: &redirectClient,
		Authenticate: func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer must-not-forward")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New redirect client: %v", err)
	}
	if _, err := client.Execute(context.Background(), validRequest(t)); err != nil {
		t.Fatalf("redirect Execute: %v", err)
	}
	if authorization := <-receivedAuthorization; authorization != "" {
		t.Fatalf("cross-origin authorization forwarded: %q", authorization)
	}
}

func TestNewClonesDecodeOptionsAndReadBoundedAvoidsOverflow(t *testing.T) {
	t.Parallel()
	options := protocol.DecodeOptions{
		Capabilities:               map[string]bool{"core": true},
		Extensions:                 map[string]protocol.ExtensionSupport{"example": {Version: "1.0.0"}},
		IgnorableExtensionMetadata: map[string]bool{"example": true},
		ExtensionNamespaces:        map[string]bool{"example": true},
	}
	configured, err := New(Config{Endpoint: "https://example.test/v1/execute", DecodeOptions: options})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	delete(options.Capabilities, "core")
	delete(options.Extensions, "example")
	delete(options.IgnorableExtensionMetadata, "example")
	delete(options.ExtensionNamespaces, "example")
	if !configured.decodeOptions.Capabilities["core"] || configured.decodeOptions.Extensions["example"].Version != "1.0.0" || !configured.decodeOptions.IgnorableExtensionMetadata["example"] || !configured.decodeOptions.ExtensionNamespaces["example"] {
		t.Fatalf("decode options changed through caller maps: %#v", configured.decodeOptions)
	}
	payload, err := readBounded(strings.NewReader("x"), math.MaxInt64)
	if err != nil || string(payload) != "x" {
		t.Fatalf("readBounded maximum = %q, %v", payload, err)
	}
}

func validRequest(t *testing.T) Request {
	t.Helper()
	request := Request{}
	document, decodeErr := protocol.DecodeDocument([]byte(testDocument), protocol.Limits{})
	if decodeErr == nil {
		request, decodeErr = NewRequest(document, "Ping")
	}
	if decodeErr != nil {
		t.Fatalf("build valid request: %v", decodeErr)
	}
	return request
}

func assertClientCode(t *testing.T, err error, code string) {
	t.Helper()
	var clientError *Error
	if errors.As(err, &clientError) && clientError.Code == code {
		return
	}
	t.Fatalf("error = %v, want client code %s", err, code)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type trackedBody struct {
	io.Reader
	closed *atomic.Bool
}

func (body *trackedBody) Close() error {
	body.closed.Store(true)
	return nil
}
