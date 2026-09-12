package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestExecutorConsumesPortablePositiveLanguageVectors(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "language.json"))
	if err != nil {
		t.Fatalf("read language fixture: %v", err)
	}
	var fixture struct {
		Vectors []struct {
			Name             string          `json:"name"`
			Document         json.RawMessage `json:"document"`
			Valid            bool            `json:"valid"`
			RequestVariables json.RawMessage `json:"requestVariables"`
			Capabilities     []string        `json:"capabilities"`
			ExpectedData     json.RawMessage `json:"expectedData"`
			ExpectedErrors   []struct {
				Code string `json:"code"`
				Path []any  `json:"path"`
			} `json:"expectedErrors"`
			HandlerStarts *int `json:"handlerStarts"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode language fixture: %v", err)
	}
	executed := 0
	for _, vector := range fixture.Vectors {
		if !vector.Valid {
			continue
		}
		executed++
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			snapshot, starts := portableExecutionRegistry(t, vector.Name)
			request := decodePortableExecutionRequest(t, vector.Document, vector.RequestVariables, vector.Capabilities)
			plan, err := runtime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			outcome := plan.Execute(context.Background())
			if vector.HandlerStarts != nil && int(starts.Load()) != *vector.HandlerStarts {
				t.Fatalf("handler starts = %d, want %d", starts.Load(), *vector.HandlerStarts)
			}
			if len(outcome.Errors) != len(vector.ExpectedErrors) {
				t.Fatalf("errors = %#v, want %#v", outcome.Errors, vector.ExpectedErrors)
			}
			for index, expected := range vector.ExpectedErrors {
				if outcome.Errors[index].Code != expected.Code {
					t.Fatalf("error %d = %#v, want %#v", index, outcome.Errors[index], expected)
				}
				assertJSONEqual(t, outcome.Errors[index].Path, expected.Path)
			}
			if len(vector.ExpectedData) != 0 && !bytes.Equal(vector.ExpectedData, []byte("null")) {
				assertCanonicalJSONEqual(t, outcome.Data, vector.ExpectedData)
			}
		})
	}
	if executed != 15 {
		t.Fatalf("executed positive language vectors = %d, want 15", executed)
	}
}

func portableExecutionRegistry(t testing.TB, vector string) (runtime.Snapshot, *atomic.Int64) {
	t.Helper()
	catalog := schema.NewCatalog()
	if err := catalog.RegisterScalar(portableJSONValueType()); err != nil {
		t.Fatalf("register portable JSON scalar: %v", err)
	}
	registerType := func(descriptor schema.TypeDescriptor) {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("register type %s: %v", descriptor.ID, err)
		}
	}
	registerType(schema.TypeDescriptor{ID: "ValueInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
		"value": {Type: schema.TypeID(schema.String), Required: true},
	}})
	registerType(schema.TypeDescriptor{ID: "OptionalInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
		"value": {Type: schema.TypeID(schema.String), Default: json.RawMessage(`"default"`)},
	}})
	registerType(schema.TypeDescriptor{ID: "LookupInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
		"id": {Type: schema.TypeID(schema.ID), Required: true},
	}})
	registerType(schema.TypeDescriptor{ID: "UserValueInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
		"user": {Type: "JSONValue", Required: true},
	}})
	registerType(schema.TypeDescriptor{ID: "EchoInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
		"value": {Type: "JSONValue", Required: true},
	}})
	registerType(schema.TypeDescriptor{ID: "PortableProfile", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
		"displayName": {Type: schema.TypeID(schema.String), Required: true},
	}})
	registerType(schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
		"id":          {Type: schema.TypeID(schema.ID)},
		"displayName": {Type: schema.TypeID(schema.String)},
		"profile":     {Type: "PortableProfile"},
		"summary":     {Type: schema.TypeID(schema.String)},
	}})
	objectUsers := vector != "slice-bounds-clamp-to-length" && vector != "out-of-range-index-is-path-error"
	usersElement := schema.TypeID("User")
	if !objectUsers {
		usersElement = schema.TypeID(schema.ID)
	}
	registerType(schema.TypeDescriptor{ID: "PortableUsers", Kind: schema.ListType, Output: true, Element: usersElement})
	registerType(schema.TypeDescriptor{ID: "CompareInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
		"item": {Type: "User", Required: true}, "owner": {Type: "PortableUsers", Required: true},
	}})
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("freeze types: %v", err)
	}
	registry := runtime.NewRegistry(types)
	starts := &atomic.Int64{}
	registerPortableExecutionDefinitions(t, registry, vector, starts, objectUsers)
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	return snapshot, starts
}

func registerPortableExecutionDefinitions(t testing.TB, registry *runtime.Registry, vector string, starts *atomic.Int64, objectUsers bool) {
	t.Helper()
	register := func(definition runtime.Definition) {
		if err := registry.Register(definition); err != nil {
			t.Fatalf("register definition: %v", err)
		}
	}
	root := func(name string, input, output schema.TypeID) runtime.Descriptor {
		return runtime.Descriptor{Name: name, Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
			Input: input, Output: output, Metadata: completeMetadata(runtime.ReadEffect)}
	}
	if vector == "nested-explicit-map-and-list-metadata" || vector == "empty-list-map" ||
		vector == "collection-index-slice-page-and-current" || vector == "fragment-parallel-unnest-and-expression-contexts" ||
		vector == "slice-bounds-clamp-to-length" || vector == "out-of-range-index-is-path-error" {
		name := "users"
		if vector == "empty-list-map" {
			name = "emptyUsers"
		}
		register(runtime.BindInvocation[[]any](root(name, schema.TypeID(schema.String), "PortableUsers"), func(context.Context, runtime.Invocation) ([]any, error) {
			starts.Add(1)
			switch vector {
			case "empty-list-map":
				return []any{}, nil
			case "fragment-parallel-unnest-and-expression-contexts":
				return []any{map[string]any{"id": "u-1", "summary": "active"}}, nil
			case "slice-bounds-clamp-to-length", "out-of-range-index-is-path-error":
				return []any{"u-1", "u-2"}, nil
			case "collection-index-slice-page-and-current":
				return []any{map[string]any{"id": "u-1"}, map[string]any{"id": "u-2"}}, nil
			default:
				return []any{map[string]any{"id": "u-1", "displayName": "Ada"}, map[string]any{"id": "u-2", "displayName": "Lin"}}, nil
			}
		}))
	}
	if objectUsers {
		for _, field := range []string{"id", "displayName", "summary"} {
			field := field
			output := schema.TypeID(schema.String)
			if field == "id" {
				output = schema.TypeID(schema.ID)
			}
			register(runtime.BindField[map[string]any, string](runtime.Descriptor{
				Name: field, Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
				Output: output, Metadata: completeMetadata(runtime.ReadEffect),
			}, func(_ context.Context, source map[string]any) (string, error) {
				starts.Add(1)
				return source[field].(string), nil
			}))
		}
		register(runtime.BindField[map[string]any, map[string]any](runtime.Descriptor{
			Name: "profile", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
			Output: "PortableProfile", Metadata: completeMetadata(runtime.ReadEffect),
		}, func(_ context.Context, source map[string]any) (map[string]any, error) {
			starts.Add(1)
			return source["profile"].(map[string]any), nil
		}))
		register(runtime.BindField[map[string]any, string](runtime.Descriptor{
			Name: "displayName", Scope: runtime.ObjectScope, Owner: "PortableProfile", Member: runtime.FieldMember,
			Output: schema.TypeID(schema.String), Metadata: completeMetadata(runtime.ReadEffect),
		}, func(_ context.Context, source map[string]any) (string, error) {
			starts.Add(1)
			return source["displayName"].(string), nil
		}))
	}

	switch vector {
	case "pipeline-prior-result-and-current":
		register(runtime.BindInvocation[json.RawMessage](root("user", "LookupInput", "JSONValue"), func(context.Context, runtime.Invocation) (json.RawMessage, error) {
			starts.Add(1)
			return json.RawMessage(`{"id":"u-1"}`), nil
		}))
		register(runtime.BindInvocation[map[string]any](root("useUser", "UserValueInput", "User"), func(context.Context, runtime.Invocation) (map[string]any, error) {
			starts.Add(1)
			return map[string]any{"id": "u-1", "profile": map[string]any{"displayName": "Ada"}}, nil
		}))
	case "fragment-parallel-unnest-and-expression-contexts":
		register(runtime.BindCall[map[string]any, schema.InputValue, bool](runtime.Descriptor{
			Name: "compare", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.CallMember,
			Input: "CompareInput", Output: schema.TypeID(schema.Boolean), Metadata: completeMetadata(runtime.ReadEffect),
		}, func(context.Context, map[string]any, schema.InputValue) (bool, error) {
			starts.Add(1)
			return true, nil
		}))
	case "literal-var-shaped-map-is-inert", "object-member-order-is-non-semantic":
		register(runtime.Bind[schema.InputValue, json.RawMessage](root("echo", "EchoInput", "JSONValue"), func(_ context.Context, input schema.InputValue) (json.RawMessage, error) {
			starts.Add(1)
			members, _ := input.Object()
			value, _ := members["value"].Scalar()
			return value.MarshalJSON()
		}))
	case "alias-and-binding-are-independent":
		register(runtime.BindInvocation[json.RawMessage](root("user", schema.TypeID(schema.String), "JSONValue"), func(context.Context, runtime.Invocation) (json.RawMessage, error) {
			starts.Add(1)
			return json.RawMessage(`{"id":"u-1"}`), nil
		}))
		register(runtime.Bind[schema.InputValue, bool](root("consume", "UserValueInput", schema.TypeID(schema.Boolean)), func(context.Context, schema.InputValue) (bool, error) {
			starts.Add(1)
			return true, nil
		}))
	case "null-result-rejects-non-null-consumer":
		descriptor := root("nullableUser", schema.TypeID(schema.String), "JSONValue")
		descriptor.OutputNullable = true
		register(runtime.BindInvocation[*json.RawMessage](descriptor, func(context.Context, runtime.Invocation) (*json.RawMessage, error) {
			starts.Add(1)
			return nil, nil
		}))
		register(runtime.Bind[schema.InputValue, bool](root("consumeRequired", "UserValueInput", schema.TypeID(schema.Boolean)), func(context.Context, schema.InputValue) (bool, error) {
			starts.Add(1)
			return true, nil
		}))
	case "failed-result-is-unavailable":
		register(runtime.BindInvocation[string](root("create", schema.TypeID(schema.String), schema.TypeID(schema.String)), func(context.Context, runtime.Invocation) (string, error) {
			starts.Add(1)
			return "", errors.New("portable handler failure")
		}))
		register(runtime.Bind[schema.InputValue, string](root("consume", "ValueInput", schema.TypeID(schema.String)), func(context.Context, schema.InputValue) (string, error) {
			starts.Add(1)
			return "consumed", nil
		}))
	case "skipped-result-is-unavailable":
		register(runtime.BindInvocation[string](root("load", schema.TypeID(schema.String), schema.TypeID(schema.String)), func(context.Context, runtime.Invocation) (string, error) {
			starts.Add(1)
			return "loaded", nil
		}))
		register(runtime.Bind[schema.InputValue, string](root("consume", "ValueInput", schema.TypeID(schema.String)), func(context.Context, schema.InputValue) (string, error) {
			starts.Add(1)
			return "consumed", nil
		}))
	case "missing-variable-rejects-required-consumer":
		register(runtime.Bind[schema.InputValue, string](root("consumeRequired", "ValueInput", schema.TypeID(schema.String)), func(context.Context, schema.InputValue) (string, error) {
			starts.Add(1)
			return "consumed", nil
		}))
	case "missing-variable-omits-optional-argument":
		register(runtime.Bind[schema.InputValue, string](root("consumeOptional", "OptionalInput", schema.TypeID(schema.String)), func(_ context.Context, input schema.InputValue) (string, error) {
			starts.Add(1)
			members, _ := input.Object()
			value, _ := members["value"].Scalar()
			raw, _ := value.MarshalJSON()
			var result string
			_ = json.Unmarshal(raw, &result)
			return result, nil
		}))
	}
}

func portableJSONValueType() schema.TypeDescriptor {
	return schema.TypeDescriptor{
		ID: "JSONValue", Kind: schema.ScalarType, Input: true, Output: true,
		Scalar: &schema.ScalarDescriptor{
			AcceptedWireShapes: []schema.JSONShape{schema.JSONBoolean, schema.JSONString, schema.JSONNumber, schema.JSONArray, schema.JSONObject},
			Validator:          schema.ScalarValidatorShape, Serializer: schema.ScalarSerializerIdentity,
			Canonicalizer: schema.ScalarCanonicalJSON, CanonicalProfile: "c14n-1",
			Conformance: []schema.ScalarConformanceVector{
				{Input: json.RawMessage(`{"b":2,"a":1}`), Canonical: json.RawMessage(`{"a":1,"b":2}`)},
				{Input: json.RawMessage(`["x"]`), Canonical: json.RawMessage(`["x"]`)},
			},
		},
	}
}

func decodePortableExecutionRequest(t testing.TB, document, variables json.RawMessage, capabilities []string) *protocol.Request {
	t.Helper()
	envelope := map[string]any{"version": "1", "document": json.RawMessage(document)}
	if len(variables) != 0 && !bytes.Equal(variables, []byte("null")) {
		envelope["variables"] = json.RawMessage(variables)
	}
	if len(capabilities) != 0 {
		envelope["capabilities"] = capabilities
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	supported := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		supported[capability] = true
	}
	request, err := protocol.DecodeRequest(encoded, protocol.DecodeOptions{Capabilities: supported})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return request
}

func assertGoldenData(t testing.TB, name string, data map[string]any) {
	t.Helper()
	expected, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	actual, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal golden data: %v", err)
	}
	actualCanonical, actualErr := protocol.CanonicalizeJSON(actual, protocol.Limits{})
	expectedCanonical, expectedErr := protocol.CanonicalizeJSON(expected, protocol.Limits{})
	if actualErr != nil || expectedErr != nil || !bytes.Equal(actualCanonical, expectedCanonical) {
		t.Fatalf("golden %s = %s, want %s (%v, %v)", name, actual, expected, actualErr, expectedErr)
	}
}
