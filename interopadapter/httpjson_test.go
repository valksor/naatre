package interopadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHTTPJSONInvokerPreservesBoundariesAndForwardsOnlyApprovedAuthentication(t *testing.T) {
	t.Parallel()
	type observation struct {
		Request       BackendRequest
		Authorization string
		Cookie        string
		Trace         string
		Deadline      string
	}
	observed := make(chan observation, 1)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(&url.URL{Scheme: "https", Host: "api.example"}, []*http.Cookie{{Name: "implicit", Value: "secret"}})
	client := &http.Client{Jar: jar, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var backend BackendRequest
		if err := json.NewDecoder(request.Body).Decode(&backend); err != nil {
			return nil, err
		}
		observed <- observation{
			Request: backend, Authorization: request.Header.Get("Authorization"), Cookie: request.Header.Get("Cookie"),
			Trace: request.Header.Get("X-Trace"), Deadline: request.Header.Get("Naatre-Deadline"),
		}
		return jsonResponse(http.StatusOK, `{"data":{"profile":{"id":"u-1","name":"Ada"}},"errors":[]}`), nil
	})}
	invoker, err := NewHTTPJSONInvoker(HTTPJSONConfig{
		Endpoint: "https://api.example/lookup", AllowedOrigins: []string{"https://api.example"}, Client: client,
		ForwardHeaders: []string{"X-Trace", "Authorization"}, MaxRequestBytes: 4096, MaxResponseBytes: 4096,
		Metadata: func(context.Context) http.Header {
			return http.Header{"Authorization": {"Bearer secret"}, "Cookie": {"session=secret"}, "X-Trace": {"trace-1"}}
		},
	})
	if err != nil {
		t.Fatalf("NewHTTPJSONInvoker: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancel()
	input := json.RawMessage(`{"int64Min":"-9223372036854775808","uint64Max":"18446744073709551615","note":null}`)
	result, err := invoker.Invoke(ctx, BackendRequest{Operation: "lookupProfile", Input: input, Projection: []string{"profile.id", "profile.name"}})
	if err != nil || result["profile"].(map[string]any)["name"] != "Ada" {
		t.Fatalf("Invoke = %#v, %v", result, err)
	}
	got := <-observed
	if string(got.Request.Input) != string(input) || !slices.Equal(got.Request.Projection, []string{"profile.id", "profile.name"}) {
		t.Fatalf("request changed scalar or projection fidelity: %#v", got.Request)
	}
	if got.Authorization != "Bearer secret" || got.Trace != "trace-1" || got.Cookie != "" || got.Deadline == "" {
		t.Fatalf("forwarded metadata = %#v", got)
	}
}

func TestHTTPJSONInvokerRejectsRedirectUnsafeErrorsAndOversizedResponses(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/redirect":
			response := jsonResponse(http.StatusFound, `{}`)
			response.Header.Set("Location", "/target")
			return response, nil
		case "/partial":
			return jsonResponse(http.StatusOK, `{"data":{"profile":{"name":"Ada"}},"errors":[{"code":"UPSTREAM_DENIED","path":["profile","secret"]}]}`), nil
		case "/unsafe":
			return jsonResponse(http.StatusOK, `{"data":{},"errors":[{"code":"bad code","path":[1.5]}]}`), nil
		case "/negative-index":
			return jsonResponse(http.StatusOK, `{"data":{},"errors":[{"code":"UPSTREAM_DENIED","path":[-1]}]}`), nil
		default:
			return jsonResponse(http.StatusOK, `{"data":{"payload":"0123456789"},"errors":[]}`), nil
		}
	})}
	for _, test := range []struct {
		path    string
		maximum int64
		code    string
	}{
		{path: "/redirect", maximum: 4096, code: "ADAPTER_REDIRECT_BLOCKED"},
		{path: "/partial", maximum: 4096, code: "ADAPTER_PARTIAL_FAILURE"},
		{path: "/unsafe", maximum: 4096, code: "ADAPTER_RESPONSE_INVALID"},
		{path: "/negative-index", maximum: 4096, code: "ADAPTER_RESPONSE_INVALID"},
		{path: "/large", maximum: 8, code: "ADAPTER_RESPONSE_LIMIT"},
	} {
		t.Run(strings.TrimPrefix(test.path, "/"), func(t *testing.T) {
			invoker, err := NewHTTPJSONInvoker(HTTPJSONConfig{Endpoint: "https://api.example" + test.path, AllowedOrigins: []string{"https://api.example"}, Client: client, MaxRequestBytes: 4096, MaxResponseBytes: test.maximum})
			if err != nil {
				t.Fatal(err)
			}
			_, err = invoker.Invoke(context.Background(), BackendRequest{Operation: "lookup", Projection: []string{}})
			if errorCode(err) != test.code || strings.Contains(err.Error(), "UPSTREAM_DENIED") || strings.Contains(err.Error(), "api.example") {
				t.Fatalf("error = %v, code %s", err, errorCode(err))
			}
		})
	}
}

func TestHTTPJSONInvokerPropagatesCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	invoker, err := NewHTTPJSONInvoker(HTTPJSONConfig{Endpoint: "https://api.example/wait", AllowedOrigins: []string{"https://api.example"}, Client: client, MaxRequestBytes: 1024, MaxResponseBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, invokeErr := invoker.Invoke(ctx, BackendRequest{Operation: "wait", Projection: []string{}})
		done <- invokeErr
	}()
	<-started
	cancel()
	if err := <-done; errorCode(err) != "ADAPTER_CANCELLED" {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestHTTPJSONConfigFailsClosed(t *testing.T) {
	t.Parallel()
	_, err := NewHTTPJSONInvoker(HTTPJSONConfig{Endpoint: "https://api.example/v1", AllowedOrigins: []string{"https://other.example"}, MaxRequestBytes: 1, MaxResponseBytes: 1})
	if errorCode(err) != "ADAPTER_EGRESS_DENIED" {
		t.Fatalf("egress error = %v", err)
	}
	_, err = NewHTTPJSONInvoker(HTTPJSONConfig{Endpoint: "https://api.example/v1", AllowedOrigins: []string{"https://api.example"}, ForwardHeaders: []string{"Cookie"}, MaxRequestBytes: 1, MaxResponseBytes: 1})
	if errorCode(err) != "ADAPTER_METADATA_POLICY_INVALID" {
		t.Fatalf("metadata error = %v", err)
	}
	var adapterFailure *Error
	if !errors.As(err, &adapterFailure) {
		t.Fatalf("error type = %T", err)
	}
	_, err = NewHTTPJSONInvoker(HTTPJSONConfig{Endpoint: "https://api.example/v1", AllowedOrigins: []string{"https://api.example"}, ForwardHeaders: []string{"Bad Header"}, MaxRequestBytes: 1, MaxResponseBytes: 1})
	if errorCode(err) != "ADAPTER_METADATA_POLICY_INVALID" {
		t.Fatalf("invalid header error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
