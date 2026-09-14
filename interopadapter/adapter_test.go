package interopadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestCompilePublishesCanonicalFidelityAndRegistersApprovedOperation(t *testing.T) {
	t.Parallel()
	types := adapterTypes(t)
	requests := make(chan BackendRequest, 2)
	config := adapterConfig(types, Operation{
		ExternalName: "lookupProfile", Approved: true, Descriptor: lookupDescriptor("lookup"),
		Invoker: InvokerFunc(func(_ context.Context, request BackendRequest) (map[string]any, error) {
			requests <- request
			return map[string]any{"profile": map[string]any{"id": "u-1", "name": "Ada"}}, nil
		}),
	})
	compiled, report, err := Compile(config)
	if err != nil {
		t.Fatalf("Compile: %v, report=%#v", err, report)
	}
	if report.Status != "ready" || len(report.Diagnostics) != 0 || len(report.Operations) != 1 {
		t.Fatalf("fidelity report = %#v", report)
	}
	if report.Operations[0].Policy.Idempotency != runtime.IdempotencyIdempotent || report.Operations[0].Policy.Cost != 7 || report.Operations[0].Policy.RetrySafe || report.Operations[0].Policy.Cacheable {
		t.Fatalf("policy was inferred or changed: %#v", report.Operations[0].Policy)
	}
	first, err := compiled.MarshalFidelityReport()
	if err != nil {
		t.Fatalf("MarshalFidelityReport: %v", err)
	}
	second, err := compiled.MarshalFidelityReport()
	if err != nil || !slices.Equal(first, second) || !json.Valid(first) {
		t.Fatalf("fidelity report is not deterministic JSON: %s / %v", second, err)
	}

	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register adapter: %v", err)
	}
	registerProjectionFields(t, registry)
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	for _, test := range []struct {
		name      string
		arguments string
		input     string
	}{
		{name: "absent", arguments: `"id":{"$literal":"u-1"}`, input: `{"id":"u-1"}`},
		{name: "null", arguments: `"id":{"$literal":"u-1"},"note":{"$literal":null}`, input: `{"id":"u-1","note":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := decodeAdapterRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{`+test.arguments+`},"select":[{"$field":{"name":"profile","select":[{"$field":{"name":"name","as":"display"}},{"$field":{"name":"id"}}]}}]}}]}]}}`)
			plan, err := runtime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			outcome := plan.Execute(context.Background())
			if len(outcome.Errors) != 0 || !reflect.DeepEqual(outcome.Data, map[string]any{"lookup": map[string]any{"profile": map[string]any{"display": "Ada", "id": "u-1"}}}) {
				t.Fatalf("adapter outcome = %#v", outcome)
			}
			backend := <-requests
			if string(backend.Input) != test.input || !slices.Equal(backend.Projection, []string{"profile.id", "profile.name"}) {
				t.Fatalf("backend request = %#v", backend)
			}
		})
	}
}

func TestCompileRejectsUnsupportedUnapprovedAndInferredPolicy(t *testing.T) {
	t.Parallel()
	types := adapterTypes(t)
	var calls atomic.Int64
	operation := Operation{
		ExternalName: "xVendorAdmin", Approved: false, Descriptor: lookupDescriptor("admin"),
		Invoker: InvokerFunc(func(context.Context, BackendRequest) (map[string]any, error) {
			calls.Add(1)
			return nil, nil
		}),
	}
	config := adapterConfig(types, operation)
	for index := range config.Mappings {
		if config.Mappings[index].Feature == "streaming" {
			config.Mappings[index] = Mapping{Feature: "streaming", Classification: Unsupported, Resolution: "upstream is server-streaming but destination is unary"}
		}
	}
	config.Operations[0].Descriptor.Metadata.Idempotency = ""
	compiled, report, err := Compile(config)
	if compiled != nil || err == nil || report.Status != "rejected" {
		t.Fatalf("Compile = %#v, %#v, %v", compiled, report, err)
	}
	codes := make([]string, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		codes = append(codes, diagnostic.Code)
	}
	for _, required := range []string{"ADAPTER_MAPPING_UNSUPPORTED", "ADAPTER_OPERATION_UNAPPROVED", "ADAPTER_POLICY_REQUIRED"} {
		if !slices.Contains(codes, required) {
			t.Fatalf("diagnostics %v omit %s", codes, required)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected adapter invoked unapproved operation %d times", calls.Load())
	}
	encoded, marshalErr := MarshalFidelityReport(report)
	if marshalErr != nil || !json.Valid(encoded) || !bytes.Contains(encoded, []byte(`"status":"rejected"`)) {
		t.Fatalf("rejected fidelity report is not machine-readable: %s / %v", encoded, marshalErr)
	}
}

func TestCompileRejectsUnknownProtocol(t *testing.T) {
	t.Parallel()
	config := adapterConfig(adapterTypes(t), Operation{
		ExternalName: "lookupProfile", Approved: true, Descriptor: lookupDescriptor("lookup"),
		Invoker: InvokerFunc(func(context.Context, BackendRequest) (map[string]any, error) { return map[string]any{}, nil }),
	})
	config.Protocol = "vendor-protocol"
	config.Specification = ""
	compiled, report, err := Compile(config)
	if compiled != nil || errorCode(err) != "ADAPTER_SPECIFICATION_UNSUPPORTED" || report.Status != "rejected" {
		t.Fatalf("Compile = %#v, %#v, %v", compiled, report, err)
	}
}

func TestRuntimePreservesPartialFailureBetweenImportedOperations(t *testing.T) {
	t.Parallel()
	types := adapterTypes(t)
	good := Operation{
		ExternalName: "lookupProfile", Approved: true, Descriptor: lookupDescriptor("good"),
		Invoker: InvokerFunc(func(context.Context, BackendRequest) (map[string]any, error) {
			return map[string]any{"profile": map[string]any{"id": "u-1", "name": "Ada"}}, nil
		}),
	}
	bad := Operation{
		ExternalName: "brokenProfile", Approved: true, Descriptor: lookupDescriptor("broken"),
		Invoker: InvokerFunc(func(context.Context, BackendRequest) (map[string]any, error) {
			return nil, adapterError("ADAPTER_PARTIAL_FAILURE", errors.New("secret upstream body"))
		}),
	}
	compiled, _, err := Compile(adapterConfig(types, good, bad))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	registry := runtime.NewRegistry(types)
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	registerProjectionFields(t, registry)
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	request := decodeAdapterRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"good","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"profile","select":[{"$field":{"name":"name"}}]}}]}},{"$call":{"name":"broken","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"profile","select":[{"$field":{"name":"name"}}]}}]}}]}]}}`)
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(context.Background())
	if _, ok := outcome.Data["good"]; !ok || len(outcome.Errors) != 1 || !reflect.DeepEqual(outcome.Errors[0].Path, []any{"broken"}) || outcome.Errors[0].Message == "secret upstream body" {
		t.Fatalf("partial outcome = %#v", outcome)
	}
}

func TestProjectionIsDeterministicBoundedAndRejectsCompositionControls(t *testing.T) {
	t.Parallel()
	request := decodeAdapterRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$field":{"name":"profile","select":[{"$field":{"name":"name","as":"z"}},{"$field":{"name":"id"}},{"$field":{"name":"name","as":"a"}}]}}]}}]}]}}`)
	root := request.Document().Operations()[0].Selections()[0]
	projection, err := Projection(root.Selections(), 4)
	if err != nil || !slices.Equal(projection, []string{"profile.id", "profile.name"}) {
		t.Fatalf("Projection = %v, %v", projection, err)
	}
	if _, err := Projection(root.Selections(), 3); errorCode(err) != "ADAPTER_FANOUT_LIMIT" {
		t.Fatalf("fan-out error = %v", err)
	}
	pageRequest := decodeAdapterRequestWithOptions(t, `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"lookup","args":{"id":{"$literal":"u-1"}},"select":[{"$page":{"as":"first","first":1,"select":[{"$field":{"name":"profile"}}]}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	page := pageRequest.Document().Operations()[0].Selections()[0]
	if _, err := Projection(page.Selections(), 4); errorCode(err) != "ADAPTER_SELECTION_UNSUPPORTED" {
		t.Fatalf("control selection error = %v", err)
	}
}

func adapterConfig(types schema.Snapshot, operations ...Operation) Config {
	mappings := make([]Mapping, 0, len(requiredFeatures))
	for _, feature := range requiredFeatures {
		mapping := Mapping{Feature: feature, Classification: Lossless}
		if feature == "authentication" || feature == "field-masks-projections" || feature == "partial-failure" || feature == "redirects-egress" {
			mapping.Classification = ExplicitlyAdapted
			mapping.Resolution = "configured and verified by the application"
		}
		if feature == "cost" || feature == "idempotency" || feature == "transactions" {
			mapping.Classification = ApplicationSupplied
			mapping.Resolution = "declared Naatre operation metadata"
		}
		mappings = append(mappings, mapping)
	}
	return Config{
		AdapterID: "profiles.import", Protocol: OpenAPI, Specification: OpenAPISpecification,
		Direction: RuntimeConsume, SchemaIdentity: "profiles.schema.v1", WireVersion: "openapi.wire.3.2.0",
		Types: types, Mappings: mappings, Operations: operations, MaxOperations: 8, MaxFanOut: 16,
	}
}

func lookupDescriptor(name string) runtime.Descriptor {
	return runtime.Descriptor{
		Name: name, Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: "LookupInput", Output: "LookupResult",
		Metadata: runtime.Metadata{
			Effect: runtime.ReadEffect, Deterministic: false, Cacheable: false, RetrySafe: false,
			ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible,
			Transaction: runtime.TransactionNone, AuthorizationPolicy: "profiles.read",
			Idempotency: runtime.IdempotencyIdempotent, Cost: 7,
		},
	}
}

func adapterTypes(t testing.TB) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "LookupInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.ID), Required: true}, "note": {Type: schema.TypeID(schema.String), Nullable: true},
		}},
		{ID: "Profile", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.ID)}, "name": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "LookupResult", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"profile": {Type: "Profile"},
		}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register type: %v", err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze types: %v", err)
	}
	return types
}

func registerProjectionFields(t testing.TB, registry *runtime.Registry) {
	t.Helper()
	metadata := runtime.Metadata{
		Effect: runtime.ReadEffect, Deterministic: true, Cacheable: false, RetrySafe: false,
		ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible,
		Transaction: runtime.TransactionNone, AuthorizationPolicy: "profiles.read",
		Idempotency: runtime.IdempotencyIdempotent, Cost: 1,
	}
	definitions := []runtime.Definition{
		runtime.BindField[map[string]any, map[string]any](runtime.Descriptor{Name: "profile", Scope: runtime.ObjectScope, Owner: "LookupResult", Member: runtime.FieldMember, Output: "Profile", Metadata: metadata}, func(_ context.Context, source map[string]any) (map[string]any, error) {
			return source["profile"].(map[string]any), nil
		}),
		runtime.BindField[map[string]any, string](runtime.Descriptor{Name: "id", Scope: runtime.ObjectScope, Owner: "Profile", Member: runtime.FieldMember, Output: schema.TypeID(schema.ID), Metadata: metadata}, func(_ context.Context, source map[string]any) (string, error) { return source["id"].(string), nil }),
		runtime.BindField[map[string]any, string](runtime.Descriptor{Name: "name", Scope: runtime.ObjectScope, Owner: "Profile", Member: runtime.FieldMember, Output: schema.TypeID(schema.String), Metadata: metadata}, func(_ context.Context, source map[string]any) (string, error) { return source["name"].(string), nil }),
	}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			t.Fatalf("Register projection field: %v", err)
		}
	}
}

func decodeAdapterRequest(t testing.TB, input string) *protocol.Request {
	t.Helper()
	return decodeAdapterRequestWithOptions(t, input, protocol.DecodeOptions{})
}

func decodeAdapterRequestWithOptions(t testing.TB, input string, options protocol.DecodeOptions) *protocol.Request {
	t.Helper()
	request, err := protocol.DecodeRequest([]byte(input), options)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return request
}

func errorCode(err error) string {
	var adapterFailure *Error
	if errors.As(err, &adapterFailure) {
		return adapterFailure.Code
	}
	return ""
}
