package openapiadapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/schema"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type errorReadCloser struct{ err error }

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (errorReadCloser) Close() error               { return nil }

func TestHTTPInvokerPreservesScalarsNullCredentialsAndProjection(t *testing.T) {
	t.Parallel()
	var received []byte
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://profiles.example.test/v1/profiles/lookup?fields=id%2Cname" {
			t.Fatalf("request target = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer top-secret" || request.Header.Get("X-Internal") != "" || request.Header.Get("Cookie") != "" {
			t.Fatalf("forwarded headers = %#v", request.Header)
		}
		var err error
		received, err = io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response(http.StatusOK, `{"id":9223372036854775807,"name":"Ada","note":null}`, "application/json"), nil
	})}
	invoker, err := newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test/v1", AllowedOrigins: []string{"https://profiles.example.test"}, Client: client,
		CredentialHeaders: []string{"Authorization"}, Credentials: func(context.Context) http.Header {
			return http.Header{"Authorization": {"Bearer top-secret"}, "X-Internal": {"metadata"}, "Cookie": {"session=secret"}}
		},
		MaxRequestBytes: 1024, MaxResponseBytes: 1024,
	}, httpOperation{
		id: "lookupProfile", method: http.MethodPost, path: "/profiles/lookup", fieldMaskQuery: "fields",
		requestSchema: mustRawSchema(t, `{"$ref":"#/components/schemas/LookupInput"}`), responseSchema: mustRawSchema(t, `{"$ref":"#/components/schemas/LookupOutput"}`),
		components: mustComponents(t, lookupDocument), successStatus: http.StatusOK, securityRequired: true, securityHeaders: [][]string{{"Authorization"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := invoker.Invoke(context.Background(), interopadapter.BackendRequest{
		Operation: "lookupProfile", Input: []byte(`{"id":"9223372036854775807","note":null}`), Projection: []string{"name", "id", "name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(received) != `{"id":9223372036854775807,"note":null}` {
		t.Fatalf("request body = %s", received)
	}
	if output["id"] != "9223372036854775807" || output["note"] != nil {
		t.Fatalf("response body = %#v", output)
	}
}

func TestHTTPInvokerRejectsRedirectStatusOversizeAndCredentialMisconfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fn   roundTripFunc
		code string
	}{
		{name: "redirect", fn: func(*http.Request) (*http.Response, error) {
			return response(http.StatusFound, `{}`, "application/json; charset=utf-8", "https://evil.example.test"), nil
		}, code: "OPENAPI_REDIRECT_BLOCKED"},
		{name: "status", fn: func(*http.Request) (*http.Response, error) {
			return response(http.StatusUnauthorized, `{"token":"top-secret","internal":"metadata"}`, "application/problem+json"), nil
		}, code: "OPENAPI_UPSTREAM_STATUS"},
		{name: "undeclared success status", fn: func(*http.Request) (*http.Response, error) {
			return response(http.StatusCreated, `{"name":"Ada"}`, "application/json"), nil
		}, code: "OPENAPI_UPSTREAM_STATUS"},
		{name: "oversize", fn: func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, `{"name":"01234567890123456789"}`, "application/json"), nil
		}, code: "OPENAPI_RESPONSE_LIMIT"},
		{name: "read cancellation", fn: func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: errorReadCloser{err: context.Canceled},
			}, nil
		}, code: "OPENAPI_CANCELLED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker, err := newHTTPInvoker(HTTPConfig{
				BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"}, Client: &http.Client{Transport: test.fn},
				MaxRequestBytes: 1024, MaxResponseBytes: 16,
			}, httpOperation{id: "lookupProfile", method: http.MethodPost, path: "/lookup", successStatus: http.StatusOK, responseSchema: []byte(`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}}}`)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = invoker.Invoke(context.Background(), interopadapter.BackendRequest{Operation: "lookupProfile"})
			if errorCode(err) != test.code || strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), "evil.example") || strings.Contains(err.Error(), "internal") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	_, err := newHTTPInvoker(HTTPConfig{
		BaseURL: "https://user:secret@profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"}, MaxRequestBytes: 1, MaxResponseBytes: 1,
	}, httpOperation{id: "lookup", method: http.MethodGet, path: "/"})
	if errorCode(err) != "OPENAPI_ENDPOINT_INVALID" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("endpoint error = %v", err)
	}
	_, err = newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
		MaxRequestBytes: maxHTTPBodyBytes + 1, MaxResponseBytes: 1,
	}, httpOperation{id: "lookup", method: http.MethodGet, path: "/", successStatus: http.StatusOK})
	if errorCode(err) != "OPENAPI_LIMIT_INVALID" {
		t.Fatalf("body ceiling error = %v", err)
	}
}

func TestHTTPInvokerPropagatesCancellationWithoutListener(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var cancelled atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		cancelled.Store(true)
		return nil, request.Context().Err()
	})}
	invoker, err := newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"}, Client: client,
		MaxRequestBytes: 1024, MaxResponseBytes: 1024,
	}, httpOperation{id: "lookup", method: http.MethodPost, path: "/lookup", successStatus: http.StatusOK, responseSchema: []byte(`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, invokeErr := invoker.Invoke(ctx, interopadapter.BackendRequest{Operation: "lookup"})
		done <- invokeErr
	}()
	<-started
	cancel()
	if err := <-done; errorCode(err) != "OPENAPI_CANCELLED" || !cancelled.Load() {
		t.Fatalf("cancellation = %v, observed=%v", err, cancelled.Load())
	}
}

func TestHTTPInvokerEnforcesBoundedFanOut(t *testing.T) {
	t.Parallel()
	invoker, err := newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, `{}`, "application/json"), nil
		})},
		MaxRequestBytes: 1024, MaxResponseBytes: 1024, MaxProjectionFields: 2,
	}, httpOperation{id: "lookup", method: http.MethodPost, path: "/lookup", fieldMaskQuery: "fields", successStatus: http.StatusOK, responseSchema: []byte(`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = invoker.Invoke(context.Background(), interopadapter.BackendRequest{Operation: "lookup", Input: []byte(`{}`), Projection: []string{"a", "b", "c"}})
	if errorCode(err) != "OPENAPI_FANOUT_LIMIT" {
		t.Fatalf("fan-out error = %v", err)
	}
}

func TestHTTPInvokerRequiresCredentialsMatchingDeclaredSecurity(t *testing.T) {
	t.Parallel()
	operation := httpOperation{
		id: "secure", method: http.MethodGet, path: "/secure", successStatus: http.StatusOK, securityRequired: true,
		securityHeaders: [][]string{{"Authorization"}},
		responseSchema:  []byte(`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}}}`),
	}
	_, err := newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
		CredentialHeaders: []string{"X-Api-Key"}, Credentials: func(context.Context) http.Header { return http.Header{"X-Api-Key": {"secret"}} },
		MaxRequestBytes: 1024, MaxResponseBytes: 1024,
	}, operation)
	if errorCode(err) != "OPENAPI_CREDENTIALS_REQUIRED" {
		t.Fatalf("mismatched security headers = %v", err)
	}
	invoker, err := newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
		CredentialHeaders: []string{"Authorization"}, Credentials: func(context.Context) http.Header { return http.Header{"Authorization": {""}} },
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("transport invoked without credentials")
			return nil, nil
		})},
		MaxRequestBytes: 1024, MaxResponseBytes: 1024,
	}, operation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = invoker.Invoke(context.Background(), interopadapter.BackendRequest{Operation: "secure"})
	if errorCode(err) != "OPENAPI_CREDENTIALS_REQUIRED" {
		t.Fatalf("missing credential value = %v", err)
	}
	_, err = newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
		CredentialHeaders: []string{"X-Forwarded-For"}, Credentials: func(context.Context) http.Header {
			return http.Header{"X-Forwarded-For": {"127.0.0.1"}}
		},
		MaxRequestBytes: 1024, MaxResponseBytes: 1024,
	}, operation)
	if errorCode(err) != "OPENAPI_CREDENTIAL_HEADER_INVALID" {
		t.Fatalf("forwarding credential header = %v", err)
	}
	_, err = newHTTPInvoker(HTTPConfig{
		BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
		CredentialHeaders: []string{"Authorization", "authorization"}, Credentials: func(context.Context) http.Header {
			return http.Header{"Authorization": {"Bearer secret"}}
		},
		MaxRequestBytes: 1024, MaxResponseBytes: 1024,
	}, operation)
	if errorCode(err) != "OPENAPI_CREDENTIAL_HEADER_INVALID" {
		t.Fatalf("duplicate credential header = %v", err)
	}
}

func TestScalarMappingsKeepIDDistinctFromString(t *testing.T) {
	t.Parallel()
	exported, ok := exportScalar(schema.TypeID(schema.ID))
	if !ok || exported["type"] != "string" || exported["format"] != "id" {
		t.Fatalf("exported ID = %#v, %v", exported, ok)
	}
	format := jsonRaw(`"id"`)
	imported, err := scalarType("string", format)
	if err != nil || imported != schema.TypeID(schema.ID) {
		t.Fatalf("imported ID = %q, %v", imported, err)
	}
	if _, err := scalarType("number", jsonRaw(`"float"`)); errorCode(err) != "OPENAPI_SCHEMA_UNSUPPORTED" {
		t.Fatalf("narrow float mapping = %v", err)
	}
}

func response(status int, body, contentType string, location ...string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", contentType)
	if len(location) != 0 {
		header.Set("Location", location[0])
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewBufferString(body))}
}

func mustRawSchema(t testing.TB, value string) []byte {
	t.Helper()
	return []byte(value)
}

func mustComponents(t testing.TB, document []byte) map[string]jsonRaw {
	t.Helper()
	parsed, _, err := parseDocument(document, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	return parsed.components
}

func TestProjectionNormalization(t *testing.T) {
	got, err := normalizeProjection([]string{"name", "id", "name"}, 3)
	if err != nil || !slices.Equal(got, []string{"id", "name"}) {
		t.Fatalf("projection = %v, %v", got, err)
	}
	if _, err := normalizeProjection([]string{"a", "b"}, 1); !errors.Is(err, errFanOut) {
		t.Fatalf("limit error = %v", err)
	}
}
