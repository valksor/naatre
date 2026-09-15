package openrpcadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const validDescription = `{
  "openrpc":"1.4.1",
  "info":{"title":"Profiles","version":"2026-09-15"},
  "methods":[
    {"name":"profile.echo","paramStructure":"by-name","params":[{"name":"value","required":true,"schema":{"type":["string","null"]}}],"result":{"name":"value","schema":{"type":"string"}},"errors":[{"code":-40001,"message":"Denied"}]},
    {"name":"profile.changed","paramStructure":"by-name","params":[{"name":"revision","required":true,"schema":{"type":"integer"}}],"result":{"name":"accepted","schema":{"type":"boolean"}}}
  ],
  "components":{"schemas":{"Contact":{"oneOf":[{"type":"object","properties":{"kind":{"const":"email"},"email":{"type":"string"}},"required":["kind","email"],"additionalProperties":false},{"type":"object","properties":{"kind":{"const":"phone"},"phone":{"type":"string"}},"required":["kind","phone"],"additionalProperties":false}]}}}
}`

func TestDescriptionMappingPublishesExactPolicyAndCanonicalExport(t *testing.T) {
	t.Parallel()
	config := mappingConfig(interopadapter.SchemaImport)
	description, report, err := MapDescription([]byte(validDescription), config, Limits{})
	if err != nil || report.Status != "ready" || report.Specification != Specification || report.Profile != Profile {
		t.Fatalf("MapDescription = %#v, %#v, %v", description, report, err)
	}
	var echoPolicy interopadapter.PolicyClaims
	for _, operation := range report.Operations {
		if operation.ExternalName == "profile.echo" {
			echoPolicy = operation.Policy
		}
	}
	if len(report.Operations) != 2 || echoPolicy.Effect != runtime.ReadEffect || echoPolicy.RetrySafe || echoPolicy.Idempotency != runtime.IdempotencyIdempotent {
		t.Fatalf("application policy was inferred or changed: %#v", report.Operations)
	}
	if !hasMapping(report.Mappings, "scalar-ranges", interopadapter.Lossless) || !hasMapping(report.Mappings, "notifications", interopadapter.ExplicitlyAdapted) {
		t.Fatalf("fidelity mappings = %#v", report.Mappings)
	}

	exportConfig := mappingConfig(interopadapter.SchemaExport)
	canonical, exportReport, err := ExportDescription(description, exportConfig, Limits{})
	if err != nil || exportReport.Status != "ready" || !json.Valid(canonical) || bytes.Contains(canonical, []byte("\n")) {
		t.Fatalf("ExportDescription = %s, %#v, %v", canonical, exportReport, err)
	}
	roundTrip, err := ParseDescription(canonical, Limits{})
	if err != nil || roundTrip.Methods[0].Name != description.Methods[0].Name || string(roundTrip.Methods[0].Params[0].Schema) != string(description.Methods[0].Params[0].Schema) {
		t.Fatalf("description round trip = %#v, %v", roundTrip, err)
	}
}

func TestDescriptionRejectsUnsupportedMalformedAndResourceBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  string
		limits Limits
		code   string
	}{
		{name: "revision", input: strings.Replace(validDescription, `"1.4.1"`, `"1.3.2"`, 1), code: "OPENRPC_SPECIFICATION_UNSUPPORTED"},
		{name: "unknown description field", input: strings.Replace(validDescription, `"openrpc":"1.4.1",`, `"openrpc":"1.4.1","servers":[],`, 1), code: "OPENRPC_DESCRIPTION_INVALID"},
		{name: "unsupported schema", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"allOf":[{"type":"string"}]}},"errors"`, 1), code: "OPENRPC_SCHEMA_UNSUPPORTED"},
		{name: "required type", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"type":"object","required":"name"}},"errors"`, 1), code: "OPENRPC_SCHEMA_INVALID"},
		{name: "negative min length", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"type":"string","minLength":-1}},"errors"`, 1), code: "OPENRPC_SCHEMA_INVALID"},
		{name: "zero multiple", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"type":"number","multipleOf":0}},"errors"`, 1), code: "OPENRPC_SCHEMA_INVALID"},
		{name: "unique items type", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"type":"array","items":{"type":"string"},"uniqueItems":"yes"}},"errors"`, 1), code: "OPENRPC_SCHEMA_INVALID"},
		{name: "duplicate enum", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"type":"string","enum":["a","a"]}},"errors"`, 1), code: "OPENRPC_SCHEMA_INVALID"},
		{name: "remote reference", input: strings.Replace(validDescription, `{"type":"string"}},"errors"`, `{"$ref":"https://schemas.example/secret"}},"errors"`, 1), code: "OPENRPC_REFERENCE_BLOCKED"},
		{name: "ambiguous oneof", input: strings.Replace(validDescription, `"const":"phone"`, `"const":"email"`, 1), code: "OPENRPC_SCHEMA_UNSUPPORTED"},
		{name: "by position", input: strings.Replace(validDescription, `"by-name"`, `"by-position"`, 1), code: "OPENRPC_PARAM_STRUCTURE_UNSUPPORTED"},
		{name: "duplicate key", input: strings.Replace(validDescription, `"openrpc":"1.4.1",`, `"openrpc":"1.4.1","openrpc":"1.4.1",`, 1), code: "OPENRPC_DESCRIPTION_INVALID"},
		{name: "byte limit", input: validDescription, limits: Limits{MaxDescriptionBytes: 32}, code: "OPENRPC_DESCRIPTION_LIMIT"},
		{name: "method limit", input: validDescription, limits: Limits{MaxMethods: 1}, code: "OPENRPC_METHOD_LIMIT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseDescription([]byte(test.input), test.limits)
			if errorCode(err) != test.code || strings.Contains(err.Error(), "schemas.example") {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestClientPreservesScalarIDDeadlineAndExplicitAuthentication(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("Cookie") != "" {
			return nil, errors.New("authentication mapping changed")
		}
		if _, ok := request.Context().Deadline(); !ok {
			return nil, errors.New("deadline missing")
		}
		var rpcRequest Request
		if err := json.NewDecoder(request.Body).Decode(&rpcRequest); err != nil {
			return nil, err
		}
		return rpcHTTPResponse(http.StatusOK, `{"jsonrpc":"2.0","result":"18446744073709551615","id":`+string(rpcRequest.ID)+`}`), nil
	})}
	consumer, report, err := NewClient(ClientConfig{
		Endpoint: "https://rpc.example/v1", HTTPClient: client, Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeConsume),
		Authorize: func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer secret")
			return nil
		},
	})
	if err != nil || report.Status != "ready" {
		t.Fatalf("NewClient: %#v, %v", report, err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancel()
	result, err := consumer.Call(ctx, "profile.echo", json.RawMessage(`{"value":null}`))
	if err != nil || string(result) != `"18446744073709551615"` || requests.Load() != 1 {
		t.Fatalf("Call = %s, %v, requests=%d", result, err, requests.Load())
	}
	if consumer.FidelityReport().Operations[0].Policy.Cost != 7 {
		t.Fatal("client lost published policy")
	}
}

func TestClientBatchCorrelatesOutOfOrderResultsAndNotifications(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var calls []Request
		if err := json.NewDecoder(request.Body).Decode(&calls); err != nil {
			return nil, err
		}
		body := `[{"jsonrpc":"2.0","error":{"code":-40001,"message":"Internal error","data":"PROFILE_DENIED"},"id":` + string(calls[2].ID) + `},{"jsonrpc":"2.0","result":0,"id":` + string(calls[0].ID) + `}]`
		return rpcHTTPResponse(http.StatusOK, body), nil
	})}
	config := mappingConfig(interopadapter.RuntimeConsume)
	config.Bindings[1].AllowNotification = true
	consumer, _, err := NewClient(ClientConfig{Endpoint: "https://rpc.example/v1", HTTPClient: client, Description: []byte(validDescription), Mapping: config})
	if err != nil {
		t.Fatal(err)
	}
	results, err := consumer.CallBatch(context.Background(), []BatchCall{
		{Method: "profile.echo", Params: json.RawMessage(`{"value":"x"}`)},
		{Method: "profile.changed", Params: json.RawMessage(`{"revision":1}`), Notification: true},
		{Method: "profile.echo", Params: json.RawMessage(`{"value":"denied"}`)},
	})
	if err != nil || len(results) != 3 || string(results[0].Result) != "0" || !results[1].Notification || errorCode(results[2].Error) != "JSONRPC_REMOTE_PROFILE_DENIED" {
		t.Fatalf("CallBatch = %#v, %v", results, err)
	}
}

func TestClientRejectsUnsafeErrorsEnvelopesRedirectsAndCancellation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body, code string
		status           int
	}{
		{name: "unsafe error", body: `{"jsonrpc":"2.0","error":{"code":-40001,"message":"Bearer secret"},"id":1}`, code: "JSONRPC_ERROR_UNSAFE", status: 200},
		{name: "both result and error", body: `{"jsonrpc":"2.0","result":null,"error":{"code":-32603,"message":"Internal error"},"id":1}`, code: "JSONRPC_ENVELOPE_INVALID", status: 200},
		{name: "wrong id", body: `{"jsonrpc":"2.0","result":true,"id":2}`, code: "JSONRPC_ID_MISMATCH", status: 200},
		{name: "redirect", body: ``, code: "JSONRPC_HTTP_STATUS", status: 302},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return rpcHTTPResponse(test.status, test.body), nil })}
			consumer, _, err := NewClient(ClientConfig{Endpoint: "https://rpc.example/v1", HTTPClient: client, Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeConsume)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = consumer.Call(context.Background(), "profile.echo", json.RawMessage(`{"value":"x"}`))
			if errorCode(err) != test.code || strings.Contains(err.Error(), "Bearer secret") {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}

	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	consumer, _, err := NewClient(ClientConfig{Endpoint: "https://rpc.example/v1", HTTPClient: client, Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeConsume)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := consumer.Call(ctx, "profile.echo", json.RawMessage(`{"value":"x"}`))
		done <- callErr
	}()
	<-started
	cancel()
	if err := <-done; errorCode(err) != "JSONRPC_CANCELLED" {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestHandlerPreservesBatchNotificationIDsErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	var notifications atomic.Int64
	methods := []ExposedMethod{
		{Binding: mappingConfig(interopadapter.RuntimeExpose).Bindings[0], Handle: func(_ context.Context, params json.RawMessage) (json.RawMessage, *ResponseError) {
			return slices.Clone(params), nil
		}},
		{Binding: mappingConfig(interopadapter.RuntimeExpose).Bindings[1], Handle: func(_ context.Context, _ json.RawMessage) (json.RawMessage, *ResponseError) {
			notifications.Add(1)
			return json.RawMessage("true"), nil
		}},
	}
	methods[1].Binding.AllowNotification = true
	handler, report, err := NewHandler(HandlerConfig{Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeExpose), Methods: methods})
	if err != nil || report.Status != "ready" {
		t.Fatalf("NewHandler: %#v, %v", report, err)
	}

	body := `[{"jsonrpc":"2.0","method":"profile.echo","params":{"value":null},"id":"a"},{"jsonrpc":"2.0","method":"profile.changed","params":{"revision":1}},{"jsonrpc":"2.0","method":"profile.echo","params":{"value":"null-id"},"id":null},{"jsonrpc":"2.0","method":"missing","id":9223372036854775807},{"jsonrpc":"2.0","method":"profile.echo","params":1,"id":3}]`
	recorder := serve(handler, body)
	if recorder.Code != http.StatusOK || notifications.Load() != 1 {
		t.Fatalf("status=%d notifications=%d body=%s", recorder.Code, notifications.Load(), recorder.Body.String())
	}
	var responses []Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &responses); err != nil || len(responses) != 4 {
		t.Fatalf("responses = %#v, %v", responses, err)
	}
	if string(responses[0].ID) != `"a"` || string(responses[0].Result) != `{"value":null}` || string(responses[1].ID) != "null" || string(responses[2].ID) != `9223372036854775807` || responses[2].Error.Code != CodeMethodNotFound || responses[3].Error.Code != CodeInvalidParams {
		t.Fatalf("batch fidelity = %#v", responses)
	}

	bad := serve(handler, `{"jsonrpc":"2.0","method":"profile.echo","method":"profile.changed","id":1}`)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"code":-32700`) {
		t.Fatalf("duplicate-key response = %d %s", bad.Code, bad.Body.String())
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "https://rpc.example", strings.NewReader(`{"jsonrpc":"2.0","method":"profile.echo","params":{},"id":9}`)).WithContext(cancelled)
	request.Header.Set("Content-Type", "application/json")
	cancelRecorder := httptest.NewRecorder()
	handler.ServeHTTP(cancelRecorder, request)
	if !strings.Contains(cancelRecorder.Body.String(), `"code":-32002`) {
		t.Fatalf("cancel response = %s", cancelRecorder.Body.String())
	}
}

func TestHandlerAuthenticationAndResourceFailuresAreSafe(t *testing.T) {
	t.Parallel()
	method := ExposedMethod{Binding: mappingConfig(interopadapter.RuntimeExpose).Bindings[0], Handle: func(context.Context, json.RawMessage) (json.RawMessage, *ResponseError) {
		return json.RawMessage("true"), nil
	}}
	handler, _, err := NewHandler(HandlerConfig{Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeExpose), Methods: []ExposedMethod{method}, Authenticate: func(context.Context, *http.Request) error { return errors.New("Bearer secret internal auth") }, Limits: Limits{MaxRequestBytes: 64}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := serve(handler, `{"jsonrpc":"2.0","method":"profile.echo","id":1}`)
	if recorder.Code != http.StatusUnauthorized || strings.Contains(recorder.Body.String(), "secret") || !strings.Contains(recorder.Body.String(), `"code":-32001`) {
		t.Fatalf("auth response = %d %s", recorder.Code, recorder.Body.String())
	}

	method.Handle = func(context.Context, json.RawMessage) (json.RawMessage, *ResponseError) {
		return nil, NewResponseError(-40001, "INVALID code with secret")
	}
	handler, _, err = NewHandler(HandlerConfig{Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeExpose), Methods: []ExposedMethod{method}})
	if err != nil {
		t.Fatal(err)
	}
	recorder = serve(handler, `{"jsonrpc":"2.0","method":"profile.echo","id":1}`)
	if strings.Contains(recorder.Body.String(), "secret") || !strings.Contains(recorder.Body.String(), `"code":-32603`) {
		t.Fatalf("unsafe application error = %s", recorder.Body.String())
	}

	method.Handle = func(context.Context, json.RawMessage) (json.RawMessage, *ResponseError) {
		return json.RawMessage(`"` + strings.Repeat("x", 256) + `"`), nil
	}
	handler, _, err = NewHandler(HandlerConfig{Description: []byte(validDescription), Mapping: mappingConfig(interopadapter.RuntimeExpose), Methods: []ExposedMethod{method}, Limits: Limits{MaxBatch: 1, MaxRequestBytes: 512, MaxResponseBytes: 128}})
	if err != nil {
		t.Fatal(err)
	}
	oversized := serve(handler, `{"jsonrpc":"2.0","method":"profile.echo","params":{"padding":"`+strings.Repeat("x", 512)+`"},"id":1}`)
	if oversized.Code != http.StatusRequestEntityTooLarge || !strings.Contains(oversized.Body.String(), `"code":-32003`) {
		t.Fatalf("request limit response = %d %s", oversized.Code, oversized.Body.String())
	}
	batch := serve(handler, `[{"jsonrpc":"2.0","method":"profile.echo","id":1},{"jsonrpc":"2.0","method":"profile.echo","id":2}]`)
	if batch.Code != http.StatusRequestEntityTooLarge || !strings.Contains(batch.Body.String(), `"code":-32003`) {
		t.Fatalf("batch limit response = %d %s", batch.Code, batch.Body.String())
	}
	responseLimit := serve(handler, `{"jsonrpc":"2.0","method":"profile.echo","id":1}`)
	if !strings.Contains(responseLimit.Body.String(), `"code":-32003`) || strings.Contains(responseLimit.Body.String(), strings.Repeat("x", 64)) {
		t.Fatalf("response limit response = %s", responseLimit.Body.String())
	}
}

func mappingConfig(direction interopadapter.Direction) MappingConfig {
	return MappingConfig{AdapterID: "profiles.openrpc", SchemaIdentity: "profiles.schema.1", WireVersion: "jsonrpc.2", Direction: direction, Bindings: []MethodBinding{
		{ExternalName: "profile.echo", Descriptor: testDescriptor("echo", runtime.ReadEffect, runtime.IdempotencyIdempotent)},
		{ExternalName: "profile.changed", Descriptor: testDescriptor("changed", runtime.WriteEffect, runtime.IdempotencyNonIdempotent)},
	}}
}

func testDescriptor(name string, effect runtime.Effect, idempotency runtime.IdempotencyPolicy) runtime.Descriptor {
	kind := protocol.Query
	if effect == runtime.WriteEffect {
		kind = protocol.Mutation
	}
	return runtime.Descriptor{Name: name, Scope: runtime.RootScope, Kind: kind, Member: runtime.CallMember, Input: schema.TypeID("Input"), Output: schema.TypeID("Output"), Metadata: runtime.Metadata{Effect: effect, ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible, Transaction: runtime.TransactionNone, AuthorizationPolicy: "profiles.call", Idempotency: idempotency, Cost: 7}}
}

func hasMapping(mappings []interopadapter.Mapping, feature string, classification interopadapter.Classification) bool {
	for _, mapping := range mappings {
		if mapping.Feature == feature && mapping.Classification == classification {
			return true
		}
	}
	return false
}

func serve(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "https://rpc.example", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func rpcHTTPResponse(status int, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}
