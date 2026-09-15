package conformance_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/valksor/naatre/normalizedcache"
	"github.com/valksor/naatre/schema"
)

type normalizedCacheFixture struct {
	Schema         string `json:"$schema"`
	Profile        string `json:"profile"`
	FixtureVersion string `json:"fixtureVersion"`
	SchemaMetadata struct {
		EntityTypes           string `json:"entityTypes"`
		IdentityFields        string `json:"identityFields"`
		AliasesAffectIdentity bool   `json:"aliasesAffectIdentity"`
		FederationRequired    bool   `json:"federationRequired"`
	} `json:"schemaMetadata"`
	ScopeBindings []string `json:"scopeBindings"`
	FieldIdentity []string `json:"fieldIdentity"`
	FieldStates   []string `json:"fieldStates"`
	Cacheability  []string `json:"cacheability"`
	Freshness     []string `json:"freshness"`
	MergeCases    []struct {
		Name     string `json:"name"`
		Input    string `json:"input"`
		Expected string `json:"expected"`
	} `json:"mergeCases"`
	OptimisticOutcomes []struct {
		Outcome  string `json:"outcome"`
		Expected string `json:"expected"`
	} `json:"optimisticOutcomes"`
	ScopeInvalidations []string `json:"scopeInvalidations"`
	EvidenceOwnership  struct {
		ReferenceLanguage        string `json:"referenceLanguage"`
		ReferencePackage         string `json:"referencePackage"`
		SDKEvidence              string `json:"sdkEvidence"`
		CompleteMatrixOwnerIssue int    `json:"completeMatrixOwnerIssue"`
	} `json:"evidenceOwnership"`
}

func TestNormalizedCacheFixtureAndSchema(t *testing.T) {
	t.Parallel()
	var fixture normalizedCacheFixture
	readFixture(t, "normalized-cache.json", &fixture)
	if fixture.Schema != "normalized-cache.schema.json" || fixture.Profile != "sdk.normalized-cache-1" || fixture.FixtureVersion != "1.0.0" ||
		fixture.SchemaMetadata.EntityTypes != "declared-output-objects-only" || fixture.SchemaMetadata.IdentityFields != "present-non-null-public-scalars" ||
		fixture.SchemaMetadata.AliasesAffectIdentity || fixture.SchemaMetadata.FederationRequired {
		t.Fatalf("normalized cache fixture header = %#v", fixture)
	}
	assertExactStrings(t, "normalized cache scopes", fixture.ScopeBindings, []string{"subject", "tenant", "authorizationRevision", "schemaRevision"})
	assertExactStrings(t, "normalized field identity", fixture.FieldIdentity, []string{"schemaName", "arguments", "locale", "representation", "schemaRevision"})
	assertExactStrings(t, "normalized field states", fixture.FieldStates, []string{"absent", "skipped", "pending", "failed", "null", "value"})
	assertExactStrings(t, "normalized cacheability", fixture.Cacheability, []string{"no-store", "private"})
	assertExactStrings(t, "normalized freshness", fixture.Freshness, []string{"fresh", "stale", "miss"})
	assertExactStrings(t, "normalized scope invalidations", fixture.ScopeInvalidations, []string{"logout", "revoked", "tenant-switch"})
	if fixture.EvidenceOwnership.ReferenceLanguage != "go" || fixture.EvidenceOwnership.ReferencePackage != "github.com/valksor/naatre/normalizedcache" ||
		fixture.EvidenceOwnership.SDKEvidence != "owned-by-each-sdk" || fixture.EvidenceOwnership.CompleteMatrixOwnerIssue != 69 {
		t.Fatalf("normalized cache evidence ownership = %#v", fixture.EvidenceOwnership)
	}

	wantCases := []string{
		"alias-and-location-share-identity", "divergent-revision-default", "divergent-revision-explicit-policy", "explicit-null-replaces-value",
		"out-of-order-known-revision", "pagination-arguments-separate-windows", "partial-marker-preserves-value", "unknown-field",
	}
	gotCases := make([]string, 0, len(fixture.MergeCases))
	for _, test := range fixture.MergeCases {
		if test.Input == "" || test.Expected == "" {
			t.Fatalf("incomplete normalized merge case %#v", test)
		}
		gotCases = append(gotCases, test.Name)
	}
	slices.Sort(gotCases)
	if !slices.Equal(gotCases, wantCases) {
		t.Fatalf("normalized merge cases = %v, want %v", gotCases, wantCases)
	}
	if len(fixture.OptimisticOutcomes) != 4 || fixture.OptimisticOutcomes[2].Outcome != "timed-out" || fixture.OptimisticOutcomes[2].Expected != "pending-reconciliation" ||
		fixture.OptimisticOutcomes[3].Outcome != "unknown" || fixture.OptimisticOutcomes[3].Expected != "pending-reconciliation" {
		t.Fatalf("normalized optimistic outcomes = %#v", fixture.OptimisticOutcomes)
	}

	schemaBytes, err := os.ReadFile("../../conformance/v1/normalized-cache.schema.json")
	if err != nil || !json.Valid(schemaBytes) {
		t.Fatalf("normalized cache JSON Schema = %v, valid=%t", err, json.Valid(schemaBytes))
	}
	var schemaHeader struct {
		Draft   string `json:"$schema"`
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(schemaBytes, &schemaHeader); err != nil || schemaHeader.Draft != "https://json-schema.org/draft/2020-12/schema" || schemaHeader.Profile != "sdk.normalized-cache.schema-1" {
		t.Fatalf("normalized cache JSON Schema header = %#v, %v", schemaHeader, err)
	}

	assertNormalizedCacheReference(t)
}

func assertNormalizedCacheReference(t *testing.T) {
	t.Helper()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "User", Kind: schema.ObjectType, Output: true, Entity: &schema.EntityDescriptor{Keys: []string{"id"}}, Fields: map[string]schema.FieldDescriptor{
		"id": {Type: schema.TypeID(schema.ID)}, "name": {Type: schema.TypeID(schema.String)},
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	scope := normalizedcache.Scope{Subject: "reader", Tenant: "tenant-a", AuthorizationRevision: "auth-r1"}
	entity, err := normalizedcache.Identify(snapshot, "User", scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	if err != nil {
		t.Fatal(err)
	}
	name, err := normalizedcache.NewFieldKey("name", json.RawMessage(`{}`), "en", "default", "schema-r1")
	if err != nil {
		t.Fatal(err)
	}
	cache := normalizedcache.New(snapshot)
	if _, err := cache.Merge(normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 1, Fields: []normalizedcache.FieldPatch{{
		Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage(`"known"`),
	}}}, normalizedcache.ConflictReject); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Merge(normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 2, Fields: []normalizedcache.FieldPatch{{
		Key: name, State: normalizedcache.FieldPending,
	}}}, normalizedcache.ConflictReject); err != nil {
		t.Fatal(err)
	}
	field, ok := cache.Lookup(entity, name)
	if !ok || field.State != normalizedcache.FieldValue || string(field.Value) != `"known"` {
		t.Fatalf("portable partial merge = %#v, %t", field, ok)
	}
}
