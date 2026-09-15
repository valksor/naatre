package playground_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valksor/naatre/playground"
	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/tooling"
)

const operationDocument = `{"operations":[{"name":"GetAccount","kind":"query","select":[{"$call":{"name":"account","args":{"id":{"$literal":"acct-1"}},"select":[{"$field":{"name":"name"}},{"$field":{"name":"tags"}}]}}]}]}`

func TestPlaygroundPositiveScenarioIsDeterministicAndRevisionPinned(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, playground.DefaultLimits())
	body := `{"document":` + operationDocument + `,"operation":"GetAccount","seed":42,"scenario":"failure","variables":{"password":"secret-value"}}`
	first := serve(t, handler, http.MethodPost, "/v1/mock", body)
	second := serve(t, handler, http.MethodPost, "/v1/mock", body)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("deterministic responses = %d/%d\n%s\n%s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	output := first.Body.String()
	for _, required := range []string{`"profile":"naatre.playground-mock-1"`, `"scenario":"failure"`, `"schemaRevision":"playground-r1"`, `"schemaDigest":"`, `"documentDigest":"`, `"frames":[`, `MOCK_PARTIAL_FAILURE`, "redacted"} {
		if !strings.Contains(output, required) {
			t.Errorf("mock response omits %q: %s", required, output)
		}
	}
	if strings.Contains(output, "secret-value") {
		t.Fatalf("mock response leaked variables: %s", output)
	}
}

func TestAuthorizedSchemaBoundaryAndStableSafeFailures(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, playground.DefaultLimits())
	schemaResponse := serve(t, handler, http.MethodGet, "/v1/schema", "")
	if schemaResponse.Code != http.StatusOK || strings.Contains(schemaResponse.Body.String(), "Secret") {
		t.Fatalf("authorized schema response = %d %s", schemaResponse.Code, schemaResponse.Body.String())
	}
	secret := "Bearer protected-token"
	badDocument := `{"operations":[{"name":"` + secret + `","kind":"query","select":[]}]}`
	response := serve(t, handler, http.MethodPost, "/v1/mock", `{"document":`+badDocument+`,"operation":"`+secret+`","seed":1,"scenario":"success"}`)
	assertProblem(t, response, http.StatusBadRequest, playground.CodeMockRejected)
	if strings.Contains(response.Body.String(), "protected-token") || strings.Contains(response.Body.String(), "Secret") {
		t.Fatalf("public failure leaked protected details: %s", response.Body.String())
	}

	for _, testCase := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{http.MethodGet, "/v1/mock", "", http.StatusMethodNotAllowed, playground.CodeMethodNotAllowed},
		{http.MethodGet, "/unknown", "", http.StatusNotFound, playground.CodeNotFound},
		{http.MethodPost, "/v1/mock", `{}`, http.StatusBadRequest, playground.CodeInvalidRequest},
	} {
		assertProblem(t, serve(t, handler, testCase.method, testCase.path, testCase.body), testCase.status, testCase.code)
	}
}

func TestInspectionValidatesOriginsAndRedactsBoundedPayloads(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, playground.DefaultLimits())
	payload := strings.Repeat("x", 5000) + "secret-tail"
	body, err := json.Marshal(map[string]any{
		"target":  "https://user:password@api.example/v1/execute?access_token=secret-url",
		"headers": map[string]string{"Authorization": "Custom secret-header", "X-Trace": "safe"},
		"payload": map[string]string{"password": "secret-body", "value": payload},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serve(t, handler, http.MethodPost, "/v1/inspect", string(body))
	if response.Code != http.StatusOK {
		t.Fatalf("inspection = %d %s", response.Code, response.Body.String())
	}
	output := response.Body.String()
	for _, secret := range []string{"password@", "secret-url", "secret-header", "secret-body", "secret-tail"} {
		if strings.Contains(output, secret) {
			t.Fatalf("inspection leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, `"truncated":true`) || !strings.Contains(output, tooling.Redacted) {
		t.Fatalf("inspection did not report redaction and truncation: %s", output)
	}
	blocked := serve(t, handler, http.MethodPost, "/v1/inspect", `{"target":"https://blocked.example/run"}`)
	assertProblem(t, blocked, http.StatusBadRequest, playground.CodeInvalidTarget)
}

func TestCancellationAndResourceLimitsHaveStableCodes(t *testing.T) {
	t.Parallel()
	handler := testHandler(t, playground.DefaultLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/profile", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertProblem(t, response, 499, playground.CodeCancelled)

	oversized := serve(t, handler, http.MethodPost, "/v1/inspect", `{"target":"https://api.example/run","payload":"`+strings.Repeat("x", (1<<20))+`"}`)
	assertProblem(t, oversized, http.StatusRequestEntityTooLarge, playground.CodeRequestTooLarge)

	limits := playground.DefaultLimits()
	limits.MaxResponseBytes = 128
	limited := testHandler(t, limits)
	tooLarge := serve(t, limited, http.MethodGet, "/v1/schema", "")
	assertProblem(t, tooLarge, http.StatusInsufficientStorage, playground.CodeOutputTooLarge)
}

func TestMockHonorsSchemaCardinalityAndGeneratorResourceCeiling(t *testing.T) {
	t.Parallel()
	document := testSchema(t)
	input := tooling.MockInput{Document: []byte(operationDocument), Operation: "GetAccount", Seed: 7, Scenario: tooling.MockSuccess}
	result, err := tooling.GenerateMockContext(context.Background(), document, input, tooling.DefaultMockOptions())
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(map[string]any)
	account := data["account"].(map[string]any)
	tags := account["tags"].([]any)
	if len(tags) != 3 {
		t.Fatalf("schema-constrained mock items = %d, want 3", len(tags))
	}
	if name := account["name"].(string); len(name) != 5 {
		t.Fatalf("schema-constrained mock string = %q, want 5 bytes", name)
	}
	options := tooling.DefaultMockOptions()
	options.MaxItems = 2
	_, err = tooling.GenerateMockContext(context.Background(), document, input, options)
	var generation *tooling.MockGenerationError
	if !errors.As(err, &generation) || generation.Code != tooling.CodeMockResourceLimit {
		t.Fatalf("resource ceiling error = %#v, %v", generation, err)
	}
}

func TestPlaygroundUIExposesOwnedFeaturesWithoutExternalRequests(t *testing.T) {
	t.Parallel()
	response := serve(t, testHandler(t, playground.DefaultLimits()), http.MethodGet, "/", "")
	if response.Code != http.StatusOK || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("UI response = %d %#v", response.Code, response.Header())
	}
	for _, feature := range []string{"Authorized schema", "Variables", "Scenario", "stream frames", "Copy redacted SDK example", "/v1/mock", "/v1/inspect"} {
		if !strings.Contains(response.Body.String(), feature) {
			t.Errorf("UI omits %q", feature)
		}
	}
	if strings.Contains(response.Body.String(), "fetch('http") || strings.Contains(response.Body.String(), `fetch("http`) {
		t.Fatal("UI contains an external request")
	}
}

func testHandler(t testing.TB, limits playground.Limits) *playground.Handler {
	t.Helper()
	handler, err := playground.NewHandler(playground.Config{
		Schema: testSchema(t), AllowedOrigins: []string{"https://api.example"}, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func testSchema(t testing.TB) schema.Document {
	t.Helper()
	content := []byte(`{
  "version":"1","canonicalVersion":"c14n-1","revision":"playground-r1",
  "types":[
    {"id":"LookupInput","name":"LookupInput","kind":"input-object","input":true,"fields":[{"id":"LookupInput.id","name":"id","type":"ID","required":true}]},
    {"id":"Tags","name":"Tags","kind":"list","output":true,"element":"String","traits":[{"id":"naatre.constraints-1","semantics":"validation","value":{"minItems":3,"maxItems":3}}]},
    {"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String","traits":[{"id":"naatre.constraints-1","semantics":"validation","value":{"minLength":5,"maxLength":5}}]},{"id":"User.tags","name":"tags","type":"Tags"}]}
  ],
  "operations":[{"id":"query.account","name":"account","kind":"query","input":"LookupInput","output":"User","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1}],
  "members":[
    {"id":"User.name.resolver","name":"name","owner":"User","kind":"field","output":"String","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1},
    {"id":"User.tags.resolver","name":"tags","owner":"User","kind":"field","output":"Tags","effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"public","cost":1}
  ]
}`)
	document, err := schema.ParseDocument(content, schema.ImportOptions{SupportedTraits: map[string]bool{schema.ConstraintTraitID: true}})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func serve(t testing.TB, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertProblem(t testing.TB, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("problem = %d %#v %s, want %d %s", response.Code, response.Header(), response.Body.String(), status, code)
	}
}
