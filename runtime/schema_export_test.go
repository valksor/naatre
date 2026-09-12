package runtime_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestRuntimeSnapshotExportsIndependentPortableSchemaByteIdentically(t *testing.T) {
	t.Parallel()

	authored, err := schema.ParseDocument([]byte(`{
		"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1",
		"capabilities":["core.language-1"],
		"types":[{"id":"User","name":"User","kind":"object","output":true,"description":"Account profile","fields":[{"id":"User.name","name":"name","type":"String","description":"Display name"}]}],
		"operations":[{"id":"query.user","name":"user","kind":"query","input":"String","output":"User","description":"Look up a user","deprecation":{"reason":"use account","replacement":"query.account"},"effect":"read","deterministic":true,"cacheable":true,"retrySafe":true,"threadSafety":"thread-safe","batching":"eligible","transaction":"none","authorizationPolicy":"user.read","cost":2,"capabilities":["core.language-1"],"traits":[{"id":"vendor.docs-1","semantics":"documentation","value":{"owner":"runtime"}}],"source":{"uri":"urn:source:query.user","line":7}}],
		"members":[{"id":"User.name.resolver","name":"name","owner":"User","kind":"field","output":"String","description":"Resolve display name","effect":"read","deterministic":true,"threadSafety":"thread-safe","batching":"ineligible","transaction":"none","authorizationPolicy":"user.read"}]
	}`), schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	types, err := authored.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	registry := runtime.NewRegistry(types)
	rootDescriptor := runtime.Descriptor{
		Name: "user", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "User", Description: "Look up a user",
		Deprecation:  &schema.Deprecation{Reason: "use account", Replacement: "query.account"},
		Capabilities: []string{"core.language-1"},
		Traits:       []schema.TraitDescriptor{{ID: "vendor.docs-1", Semantics: schema.TraitDocumentation, Value: []byte(`{"owner":"runtime"}`)}},
		Source:       &schema.SourceMetadata{URI: "urn:source:query.user", Line: 7},
		Metadata: runtime.Metadata{
			Effect: runtime.ReadEffect, Deterministic: true, Cacheable: true, RetrySafe: true,
			ThreadSafety: runtime.ThreadSafe, Batching: runtime.BatchEligible, Transaction: runtime.TransactionNone,
			AuthorizationPolicy: "user.read", Cost: 2,
		},
	}
	if err := registry.Register(runtime.Bind(rootDescriptor, func(_ context.Context, _ string) (map[string]any, error) {
		return map[string]any{"name": "Ada"}, nil
	})); err != nil {
		t.Fatalf("Register root: %v", err)
	}
	rootDescriptor.Deprecation.Reason = "mutated"
	rootDescriptor.Capabilities[0] = "mutated"
	rootDescriptor.Traits[0].Value[0] = '['
	rootDescriptor.Source.URI = "mutated"
	if err := registry.Register(runtime.BindField(runtime.Descriptor{
		Name: "name", Scope: runtime.ObjectScope, Owner: "User", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), Description: "Resolve display name",
		Metadata: runtime.Metadata{
			Effect: runtime.ReadEffect, Deterministic: true, ThreadSafety: runtime.ThreadSafe,
			Batching: runtime.BatchIneligible, Transaction: runtime.TransactionNone, AuthorizationPolicy: "user.read",
		},
	}, func(_ context.Context, source map[string]any) (string, error) { return source["name"].(string), nil })); err != nil {
		t.Fatalf("Register member: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	returned, ok := snapshot.Root(protocol.Query, "user")
	if !ok {
		t.Fatal("missing frozen root descriptor")
	}
	returned.Deprecation.Reason = "mutated again"
	returned.Capabilities[0] = "mutated-again"
	returned.Traits[0].Value[0] = '['
	returned.Source.URI = "mutated-again"
	exported, err := snapshot.ExportSchema(authored.ExportOptions())
	if err != nil {
		t.Fatalf("ExportSchema: %v", err)
	}
	want, _ := authored.CanonicalJSON()
	got, _ := exported.CanonicalJSON()
	if !bytes.Equal(got, want) {
		t.Fatalf("runtime export differs from independent schema:\n%s\n%s", got, want)
	}
	if err := snapshot.ValidateSchema(authored); err != nil {
		t.Fatalf("ValidateSchema: %v", err)
	}

	changed := bytes.Replace(want, []byte(`"cost":2`), []byte(`"cost":3`), 1)
	changedDocument, err := schema.ParseDocument(changed, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument(changed): %v", err)
	}
	if err := snapshot.ValidateSchema(changedDocument); err == nil || !strings.Contains(err.Error(), "manifest does not match") {
		t.Fatalf("ValidateSchema changed cost error = %v", err)
	}
}
