package openapiadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestCompileImportsApprovedOpenAPIOperationAndPublishesFidelity(t *testing.T) {
	t.Parallel()
	compiled, report, err := Compile(ImportConfig{
		Document:       lookupDocument,
		AdapterID:      "profiles.openapi",
		SchemaIdentity: "profiles.schema.v1",
		WireVersion:    "profiles.http.v1",
		Bindings: map[string]Binding{
			"lookupProfile": {
				Name: "lookup", Kind: protocol.Query, Metadata: readMetadata(),
				Invoker: interopadapter.InvokerFunc(func(context.Context, interopadapter.BackendRequest) (map[string]any, error) {
					return map[string]any{"id": "9223372036854775807", "name": "Ada"}, nil
				}),
			},
		},
		Limits: testLimits(),
	})
	if err != nil {
		t.Fatalf("Compile: %v; report=%#v", err, report)
	}
	if report.Profile != Profile || report.Status != "ready" || report.Protocol != interopadapter.OpenAPI || report.Specification != Specification || len(report.Diagnostics) != 0 {
		t.Fatalf("fidelity report = %#v", report)
	}
	if len(report.Operations) != 1 || report.Operations[0].ExternalName != "lookupProfile" || report.Operations[0].NaatreName != "lookup" {
		t.Fatalf("operation report = %#v", report.Operations)
	}
	if !hasMapping(report.Mappings, "scalar-ranges", interopadapter.ExplicitlyAdapted) || !hasMapping(report.Mappings, "nullable-optional", interopadapter.Lossless) {
		t.Fatalf("mapping report = %#v", report.Mappings)
	}
	input, ok := compiled.Types().Lookup("openapi.LookupInput.input")
	if !ok || input.Kind != schema.InputObjectType || !input.Fields["id"].Required || input.Fields["note"].Required || !input.Fields["note"].Nullable {
		t.Fatalf("imported input = %#v, %v", input, ok)
	}
	if input.Fields["id"].Type != schema.TypeID(schema.Int64) {
		t.Fatalf("int64 mapping = %q", input.Fields["id"].Type)
	}
	output, ok := compiled.Types().Lookup("openapi.LookupOutput.output")
	if !ok || output.Kind != schema.ObjectType || output.Fields["id"].Type != schema.TypeID(schema.Int64) {
		t.Fatalf("imported output = %#v, %v", output, ok)
	}
	registry := runtime.NewRegistry(compiled.Types())
	if err := compiled.Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	first, err := compiled.MarshalFidelityReport()
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiled.MarshalFidelityReport()
	if err != nil || !slices.Equal(first, second) || !json.Valid(first) {
		t.Fatalf("fidelity report is not canonical: %s / %v", second, err)
	}
}

func TestCompileFailsClosedBeforeInvocation(t *testing.T) {
	t.Parallel()
	var calls int
	binding := Binding{
		Name: "lookup", Kind: protocol.Query, Metadata: readMetadata(),
		Invoker: interopadapter.InvokerFunc(func(context.Context, interopadapter.BackendRequest) (map[string]any, error) {
			calls++
			return nil, nil
		}),
	}
	tests := []struct {
		name string
		doc  string
		code string
	}{
		{name: "revision", doc: string(lookupDocument), code: "OPENAPI_SPECIFICATION_UNSUPPORTED"},
		{name: "callback", doc: string(lookupDocument), code: "OPENAPI_CAPABILITY_UNSUPPORTED"},
		{name: "oneOf", doc: string(lookupDocument), code: "OPENAPI_SCHEMA_UNSUPPORTED"},
		{name: "ignored scalar keyword", doc: string(lookupDocument), code: "OPENAPI_SCHEMA_UNSUPPORTED"},
		{name: "nullable composite", doc: string(lookupDocument), code: "OPENAPI_SCHEMA_UNSUPPORTED"},
		{name: "nonportable component", doc: string(lookupDocument), code: "OPENAPI_SCHEMA_UNSUPPORTED"},
		{name: "head method", doc: string(lookupDocument), code: "OPENAPI_CAPABILITY_UNSUPPORTED"},
		{name: "remote reference", doc: string(lookupDocument), code: "OPENAPI_REFERENCE_BLOCKED"},
	}
	tests[0].doc = replaceOnce(t, tests[0].doc, `"openapi":"3.2.0"`, `"openapi":"3.1.0"`)
	tests[1].doc = replaceOnce(t, tests[1].doc, `"operationId":"lookupProfile"`, `"operationId":"lookupProfile","callbacks":{"leak":{}}`)
	tests[2].doc = replaceOnce(t, tests[2].doc, `"LookupInput":{"type":"object"`, `"LookupInput":{"oneOf":[{"type":"string"}],"type":"object"`)
	tests[3].doc = replaceOnce(t, tests[3].doc, `"type":"integer","format":"int64"`, `"type":"integer","format":"int64","items":{"type":"string"}`)
	tests[4].doc = replaceOnce(t, tests[4].doc, `"LookupInput":{"type":"object"`, `"LookupInput":{"type":["object","null"]`)
	tests[5].doc = replaceOnce(t, tests[5].doc, `"LookupOutput":{"type":"object"`, `"Lookup~1Output":{"type":"object"`)
	tests[6].doc = replaceOnce(t, tests[6].doc, `"post":`, `"head":`)
	tests[7].doc = replaceOnce(t, tests[7].doc, `"$ref":"#/components/schemas/LookupOutput"`, `"$ref":"https://schemas.example.test/LookupOutput"`)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, report, err := Compile(ImportConfig{
				Document: []byte(test.doc), AdapterID: "profiles.openapi", SchemaIdentity: "profiles.schema.v1", WireVersion: "profiles.http.v1",
				Bindings: map[string]Binding{"lookupProfile": binding}, Limits: testLimits(),
			})
			if compiled != nil || errorCode(err) != test.code || report.Status != "rejected" || len(report.Diagnostics) == 0 {
				t.Fatalf("Compile = %#v, %#v, %v", compiled, report, err)
			}
			if calls != 0 {
				t.Fatalf("rejected adapter invoked operation %d times", calls)
			}
			if text := err.Error(); text != "openapi adapter: "+test.code {
				t.Fatalf("public error leaked details: %q", text)
			}
			if errors.Unwrap(err) != nil {
				t.Fatal("public error exposed its protected cause")
			}
		})
	}
}

func TestCompileEnforcesOperationSchemaAndResourceBounds(t *testing.T) {
	t.Parallel()
	base := ImportConfig{
		Document: lookupDocument, AdapterID: "profiles.openapi", SchemaIdentity: "profiles.schema.v1", WireVersion: "profiles.http.v1",
		Bindings: map[string]Binding{"lookupProfile": {Name: "lookup", Kind: protocol.Query, Metadata: readMetadata(), Invoker: noopInvoker()}}, Limits: testLimits(),
	}
	tests := []struct {
		name   string
		mutate func(*ImportConfig)
		code   string
	}{
		{name: "document bytes", mutate: func(config *ImportConfig) { config.Limits.MaxDocumentBytes = len(config.Document) - 1 }, code: "OPENAPI_DOCUMENT_LIMIT"},
		{name: "operations", mutate: func(config *ImportConfig) { config.Limits.MaxOperations = 0 }, code: "OPENAPI_LIMIT_INVALID"},
		{name: "properties", mutate: func(config *ImportConfig) { config.Limits.MaxProperties = 1 }, code: "OPENAPI_PROPERTY_LIMIT"},
		{name: "fanout", mutate: func(config *ImportConfig) { config.Limits.MaxFanOut = 0 }, code: "OPENAPI_LIMIT_INVALID"},
		{name: "http limit widening", mutate: func(config *ImportConfig) {
			binding := config.Bindings["lookupProfile"]
			binding.Invoker = nil
			config.Bindings["lookupProfile"] = binding
			config.HTTP = &HTTPConfig{
				BaseURL: "https://profiles.example.test", AllowedOrigins: []string{"https://profiles.example.test"},
				MaxResponseBytes: config.Limits.MaxResponseBytes + 1,
			}
		}, code: "OPENAPI_LIMIT_INVALID"},
		{name: "missing policy", mutate: func(config *ImportConfig) {
			binding := config.Bindings["lookupProfile"]
			binding.Metadata.AuthorizationPolicy = ""
			config.Bindings["lookupProfile"] = binding
		}, code: "ADAPTER_POLICY_REQUIRED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			config.Bindings = map[string]Binding{"lookupProfile": base.Bindings["lookupProfile"]}
			test.mutate(&config)
			compiled, report, err := Compile(config)
			if compiled != nil || errorCode(err) != test.code || report.Status != "rejected" {
				t.Fatalf("Compile = %#v, %#v, %v", compiled, report, err)
			}
		})
	}
}

func TestParseSecurityRejectsMalformedOrAmbiguousRequirements(t *testing.T) {
	t.Parallel()
	schemes := map[string]SecurityScheme{
		"bearerAuth": {Type: "http", Scheme: "bearer"},
		"basicAuth":  {Type: "http", Scheme: "basic"},
	}
	for name, raw := range map[string]jsonRaw{
		"null security":       jsonRaw(`null`),
		"null scopes":         jsonRaw(`[{"bearerAuth":null}]`),
		"shared header and":   jsonRaw(`[{"bearerAuth":[],"basicAuth":[]}]`),
		"unknown requirement": jsonRaw(`[{"missing":[]}]`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSecurity(raw, schemes); errorCode(err) != "OPENAPI_SECURITY_UNSUPPORTED" {
				t.Fatalf("parseSecurity = %v", err)
			}
		})
	}
	if _, err := parseSecurityScheme(jsonRaw(`{"type":"apiKey","in":"header","name":"Content-Type"}`)); errorCode(err) != "OPENAPI_SECURITY_UNSUPPORTED" {
		t.Fatalf("protected API-key header = %v", err)
	}
}

func TestSchemaCompilerPreservesNullableCachedReference(t *testing.T) {
	t.Parallel()
	compiler := schemaCompiler{
		catalog: schema.NewCatalog(), components: map[string]jsonRaw{"MaybeNote": jsonRaw(`{"type":["string","null"]}`)},
		limits: testLimits(), compiled: make(map[string]compiledSchema), active: make(map[string]bool),
	}
	reference := jsonRaw(`{"$ref":"#/components/schemas/MaybeNote"}`)
	for attempt := 0; attempt < 2; attempt++ {
		identifier, nullable, err := compiler.compile(reference, "note", true, 0)
		if err != nil || identifier != schema.TypeID(schema.String) || !nullable {
			t.Fatalf("compile attempt %d = %q, nullable=%v, err=%v", attempt, identifier, nullable, err)
		}
	}
}

func TestExportProducesCanonicalOpenAPIAndRoundTripsSupportedSchema(t *testing.T) {
	t.Parallel()
	document := portableDocument(t)
	config := ExportConfig{
		Document: document, AdapterID: "profiles.export", WireVersion: "profiles.http.v1",
		Title: "Profiles", Version: "1.0.0", Limits: testLimits(),
		Bindings: map[string]ExportBinding{
			"lookup": {Method: "post", Path: "/profiles/lookup", SuccessStatus: "200", Security: []SecurityRequirement{{Scheme: "bearerAuth"}}},
		},
		SecuritySchemes: map[string]SecurityScheme{"bearerAuth": {Type: "http", Scheme: "bearer"}},
	}
	first, firstReport, err := Export(config)
	if err != nil {
		t.Fatalf("Export: %v; report=%#v", err, firstReport)
	}
	second, secondReport, err := Export(config)
	if err != nil || !slices.Equal(first, second) || !reflect.DeepEqual(firstReport, secondReport) || !json.Valid(first) {
		t.Fatalf("nondeterministic export: %s / %v", second, err)
	}
	var wire map[string]any
	if err := json.Unmarshal(first, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["openapi"] != "3.2.0" || firstReport.Status != "ready" || firstReport.Direction != interopadapter.SchemaExport {
		t.Fatalf("export = %#v, %#v", wire, firstReport)
	}
	compiled, importedReport, err := Compile(ImportConfig{
		Document: first, AdapterID: "profiles.roundtrip", SchemaIdentity: document.Revision(), WireVersion: "profiles.http.v1",
		Bindings: map[string]Binding{"lookup": {Name: "lookupRoundTrip", Kind: protocol.Query, Metadata: readMetadata(), Invoker: noopInvoker()}}, Limits: testLimits(),
	})
	if err != nil || compiled == nil || importedReport.Status != "ready" {
		t.Fatalf("round-trip import = %#v, %#v, %v", compiled, importedReport, err)
	}
}

func TestExportRejectsOperationRuntimeCannotConsume(t *testing.T) {
	t.Parallel()
	config := ExportConfig{
		Document: portableScalarOutputDocument(t), AdapterID: "profiles.export", WireVersion: "profiles.http.v1",
		Title: "Profiles", Version: "1.0.0", Limits: testLimits(),
		Bindings: map[string]ExportBinding{"lookup": {Method: "get", Path: "/profiles/lookup", SuccessStatus: "200"}},
	}
	_, report, err := Export(config)
	if errorCode(err) != "OPENAPI_RESPONSE_UNSUPPORTED" || report.Status != "rejected" {
		t.Fatalf("Export = %#v, %v", report, err)
	}
	config.Document = portableDocument(t)
	config.Bindings["lookup"] = ExportBinding{Method: "head", Path: "/profiles/lookup", SuccessStatus: "200"}
	_, report, err = Export(config)
	if errorCode(err) != "OPENAPI_METHOD_UNSUPPORTED" || report.Status != "rejected" {
		t.Fatalf("HEAD Export = %#v, %v", report, err)
	}
}

func hasMapping(mappings []interopadapter.Mapping, feature string, classification interopadapter.Classification) bool {
	for _, mapping := range mappings {
		if mapping.Feature == feature && mapping.Classification == classification {
			return true
		}
	}
	return false
}

func readMetadata() runtime.Metadata {
	return runtime.Metadata{
		Effect: runtime.ReadEffect, Idempotency: runtime.IdempotencyIdempotent, Cost: 7,
		ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchIneligible,
		Transaction: runtime.TransactionNone, AuthorizationPolicy: "profiles.read",
	}
}

func testLimits() Limits {
	return Limits{MaxDocumentBytes: 64 << 10, MaxOperations: 8, MaxSchemas: 16, MaxProperties: 32, MaxDepth: 16, MaxFanOut: 16, MaxRequestBytes: 8 << 10, MaxResponseBytes: 16 << 10}
}

func noopInvoker() interopadapter.Invoker {
	return interopadapter.InvokerFunc(func(context.Context, interopadapter.BackendRequest) (map[string]any, error) {
		return map[string]any{}, nil
	})
}

func errorCode(err error) string {
	var failure interface{ PublicCode() string }
	if errors.As(err, &failure) {
		return failure.PublicCode()
	}
	var core *interopadapter.Error
	if errors.As(err, &core) {
		return core.Code
	}
	return ""
}
