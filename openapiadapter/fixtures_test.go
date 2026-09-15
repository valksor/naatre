package openapiadapter

import (
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

var lookupDocument = []byte(`{
  "openapi":"3.2.0",
  "jsonSchemaDialect":"https://json-schema.org/draft/2020-12/schema",
  "info":{"title":"Profiles","version":"1.0.0"},
  "paths":{"/profiles/lookup":{"post":{
    "operationId":"lookupProfile",
    "requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/LookupInput"}}}},
    "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"$ref":"#/components/schemas/LookupOutput"}}}},"default":{"description":"problem","content":{"application/problem+json":{"schema":{"type":"object","additionalProperties":false,"properties":{"code":{"type":"string"}}}}}}}
  }}},
  "components":{"schemas":{
    "LookupInput":{"type":"object","additionalProperties":false,"required":["id"],"properties":{"id":{"type":"integer","format":"int64"},"note":{"type":["string","null"]}}},
    "LookupOutput":{"type":"object","additionalProperties":false,"required":["id","name"],"properties":{"id":{"type":"integer","format":"int64"},"name":{"type":"string"},"note":{"type":["string","null"]}}}
  }}
}`)

func portableDocument(t testing.TB) schema.Document {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "LookupInput", Kind: schema.InputObjectType, Input: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.Int64), Required: true}, "note": {Type: schema.TypeID(schema.String), Nullable: true},
		}},
		{ID: "LookupOutput", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"id": {Type: schema.TypeID(schema.Int64), Required: true}, "name": {Type: schema.TypeID(schema.String), Required: true}, "note": {Type: schema.TypeID(schema.String), Nullable: true},
		}},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	document, err := schema.ExportDocument(snapshot, []schema.OperationDescriptor{{
		ID: "query.lookup", Name: "lookup", Kind: protocol.Query, Input: "LookupInput", Output: "LookupOutput",
		Effect: "read", ThreadSafety: "thread-safe", Batching: "ineligible", Transaction: "none",
		AuthorizationPolicy: "profiles.read", Idempotency: "idempotent", Cost: 7,
	}}, nil, schema.ExportOptions{Revision: "profiles.schema.v1"})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func portableScalarOutputDocument(t testing.TB) schema.Document {
	t.Helper()
	snapshot, err := schema.NewCatalog().Freeze()
	if err != nil {
		t.Fatal(err)
	}
	document, err := schema.ExportDocument(snapshot, []schema.OperationDescriptor{{
		ID: "query.lookup", Name: "lookup", Kind: protocol.Query, Output: schema.TypeID(schema.String),
		Effect: "read", ThreadSafety: "thread-safe", Batching: "ineligible", Transaction: "none",
		AuthorizationPolicy: "profiles.read", Idempotency: "idempotent", Cost: 1,
	}}, nil, schema.ExportOptions{Revision: "profiles.scalar-output.v1"})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func replaceOnce(t testing.TB, value, old, replacement string) string {
	t.Helper()
	if !strings.Contains(value, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return strings.Replace(value, old, replacement, 1)
}
