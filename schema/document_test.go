package schema_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func TestSchemaDocumentCanonicalRoundTripHashAndSnapshot(t *testing.T) {
	t.Parallel()
	snapshot := portableSchemaSnapshot(t)
	document, err := schema.ExportDocument(snapshot, portableOperations(), portableMembers(), schema.ExportOptions{
		Revision:     "schema-r1",
		Capabilities: []string{"vendor.audit-1", "core.language-1"},
		Retired:      []schema.RetiredIdentity{{ID: "LegacyUser", Name: "LegacyUser", Reason: "replaced by User"}},
		References:   []schema.SchemaReference{{URI: "urn:example:base", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Revision: "base-r1"}},
		Traits:       []schema.TraitDescriptor{{ID: "vendor.docs-1", Semantics: schema.TraitDocumentation, Value: json.RawMessage(`{"owner":"platform"}`)}},
	})
	if err != nil {
		t.Fatalf("ExportDocument: %v", err)
	}
	canonical, err := document.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	parsed, err := schema.ParseDocument(canonical, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	again, err := parsed.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, again) {
		t.Fatalf("canonical round trip = %s, want %s (%v)", again, canonical, err)
	}
	leftHash, err := document.Hash()
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := parsed.Hash()
	if err != nil || leftHash != rightHash || leftHash.Algorithm != "sha-256" || leftHash.CanonicalVersion != "c14n-1" {
		t.Fatalf("hashes = %#v %#v (%v)", leftHash, rightHash, err)
	}
	imported, err := parsed.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	user, ok := imported.Lookup("User")
	if !ok || user.Name != "User" || user.Description == "" || user.Entity == nil || !slices.Equal(user.Entity.Keys, []string{"id"}) {
		t.Fatalf("imported user metadata = %#v", user)
	}
	field := user.Fields["name"]
	if field.ID != "User.name" || field.Deprecation == nil || field.Deprecation.Replacement != "displayName" {
		t.Fatalf("imported field metadata = %#v", field)
	}
	roles, ok := imported.Lookup("Role")
	if !ok || !roles.Open || len(roles.EnumMembers) != 2 || !slices.ContainsFunc(roles.EnumMembers, func(member schema.EnumMemberDescriptor) bool {
		return member.ID == "Role.ADMIN" && member.Deprecation != nil
	}) {
		t.Fatalf("imported open enum = %#v", roles)
	}
	result, ok := imported.Lookup("SearchResult")
	if !ok || !result.Open || len(result.VariantMembers) != 2 {
		t.Fatalf("imported open union = %#v", result)
	}

	types := parsed.Types()
	types[0].Name = "mutated"
	operations := parsed.Operations()
	operations[0].Name = "mutated"
	stable, _ := parsed.CanonicalJSON()
	if !bytes.Equal(stable, canonical) {
		t.Fatal("schema document mutated through accessors")
	}
}

func TestSchemaDocumentRoundTripsEmptyAndNonIdentifierEnumSpellings(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{
		ID: "Status", Name: "Status", Kind: schema.EnumType, Input: true,
		EnumValues: []string{"", "needs review", "READY"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	document, err := schema.ExportDocument(snapshot, nil, nil, schema.ExportOptions{Revision: "enum-spellings-r1"})
	if err != nil {
		t.Fatalf("ExportDocument: %v", err)
	}
	canonical, err := document.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	parsed, err := schema.ParseDocument(canonical, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	imported, err := parsed.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, spelling := range []string{"", "needs review", "READY"} {
		value, coerceErr := schema.CoerceInput(imported, "Status", json.RawMessage(strconv.Quote(spelling)), false)
		if coerceErr != nil {
			t.Fatalf("CoerceInput(%q): %v", spelling, coerceErr)
		}
		actual, known, ok := value.Enum()
		if !ok || !known || actual != spelling {
			t.Fatalf("enum %q state = %q, %t, %t", spelling, actual, known, ok)
		}
	}
}

func TestPortableSchemaRejectsInvalidCollectionContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{
			name:     "member requires positive maximum",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"member-max-r1","types":[{"id":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String"}]},{"id":"Users","kind":"list","output":true,"element":"User"}],"operations":[],"members":[{"id":"User.friends.resolver","name":"friends","owner":"User","kind":"field","output":"Users","effect":"read","collection":{"maxPageSize":0}}]}`,
			want:     "positive maximum page size",
		},
		{
			name:     "operation requires collection output",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"operation-output-r1","types":[],"operations":[{"id":"query.name","name":"name","kind":"query","output":"String","effect":"read","collection":{"maxPageSize":10}}],"members":[]}`,
			want:     "collection output type",
		},
		{
			name:     "member requires collection output",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"member-output-r1","types":[{"id":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String"}]}],"operations":[],"members":[{"id":"User.name.resolver","name":"name","owner":"User","kind":"field","output":"String","effect":"read","collection":{"maxPageSize":10}}]}`,
			want:     "collection output type",
		},
		{
			name:     "total count cost is bounded",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"collection-cost-r1","types":[{"id":"Users","kind":"list","output":true,"element":"String"}],"operations":[{"id":"query.users","name":"users","kind":"query","output":"Users","effect":"read","collection":{"maxPageSize":10,"totalCountCost":1048577}}],"members":[]}`,
			want:     "total count cost exceeds",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := schema.ParseDocument([]byte(test.document), schema.ImportOptions{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseDocument error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSchemaDiffClassifiesCollectionContractChanges(t *testing.T) {
	t.Parallel()
	before := collectionDiffDocument(t, "collections-r1", 10, 10, 2)
	after := collectionDiffDocument(t, "collections-r2", 5, 20, 3)
	diff := schema.DiffDocuments(before, after)
	want := map[string]schema.ChangeClassification{
		"operation:query.users/collection/maxPageSize":           schema.ChangeBreaking,
		"operation:query.users/collection/totalCountCost":        schema.ChangeDangerous,
		"member:User.friends.resolver/collection/maxPageSize":    schema.ChangeAdditive,
		"member:User.friends.resolver/collection/totalCountCost": schema.ChangeDangerous,
	}
	for path, classification := range want {
		if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
			return change.Path == path && change.Classification == classification
		}) {
			t.Errorf("missing %s %s in %#v", path, classification, diff.Changes)
		}
	}
}

func collectionDiffDocument(t testing.TB, revision string, operationMaximum, memberMaximum, totalCountCost uint64) schema.Document {
	t.Helper()
	return parseSchemaDocument(t, fmt.Sprintf(`{"version":"1","canonicalVersion":"c14n-1","revision":%q,"types":[{"id":"User","kind":"object","output":true,"maxDepth":2,"fields":[{"id":"User.friends","name":"friends","type":"Users"}]},{"id":"Users","kind":"list","output":true,"element":"User","maxDepth":2}],"operations":[{"id":"query.users","name":"users","kind":"query","output":"Users","effect":"read","collection":{"maxPageSize":%d,"totalCountCost":%d}}],"members":[{"id":"User.friends.resolver","name":"friends","owner":"User","kind":"field","output":"Users","effect":"read","collection":{"maxPageSize":%d,"totalCountCost":%d}}]}`,
		revision, operationMaximum, totalCountCost, memberMaximum, totalCountCost))
}

func TestSchemaExportCanonicalizesDirectiveDefaultsBeforeExposure(t *testing.T) {
	t.Parallel()
	catalog := schema.NewCatalog()
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	directive := schema.DirectiveDescriptor{
		ID: "vendor.defaulted", Name: "defaulted", Version: "1", Capability: "vendor.defaulted-1",
		Locations: []protocol.SelectionKind{protocol.CallSelection}, Phases: []schema.DirectivePhase{schema.DirectiveValidation},
		Effect: "read", Deterministic: true, Compatibility: schema.ChangeDangerous,
		Arguments: []schema.DirectiveArgumentDescriptor{{
			ID: "vendor.defaulted.value", Name: "value", Type: schema.TypeID(schema.Float64), Default: json.RawMessage(`1.0`),
		}},
	}
	document, err := schema.ExportDocument(snapshot, nil, nil, schema.ExportOptions{Revision: "directive-default-r1", Directives: []schema.DirectiveDescriptor{directive}})
	if err != nil {
		t.Fatalf("ExportDocument: %v", err)
	}
	actual := document.Directives()[0].Arguments[0].Default
	if string(actual) != "1" {
		t.Fatalf("canonical default = %s", actual)
	}
}

func TestSchemaDocumentRejectsInvalidDirectiveContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{
			name:     "output-only argument type",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"directive-output-r1","types":[{"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String"}]}],"operations":[],"members":[],"directives":[{"id":"vendor.bad","name":"bad","version":"1","capability":"vendor.bad-1","locations":["call"],"arguments":[{"id":"vendor.bad.user","name":"user","type":"User"}],"phases":["validation"],"effect":"read","cost":0,"deterministic":true,"compatibility":"dangerous"}]}`,
			want:     "non-input",
		},
		{
			name:     "uninvocable names",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"directive-name-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.bad","name":"vendor.bad","version":"1","capability":"vendor.bad-1","locations":["call"],"arguments":[{"id":"vendor.bad.value","name":"bad.value","type":"String"}],"phases":["validation"],"effect":"read","cost":0,"deterministic":true,"compatibility":"dangerous"}]}`,
			want:     "invalid directive descriptor",
		},
		{
			name:     "unknown effect",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"directive-effect-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.bad","name":"bad","version":"1","capability":"vendor.bad-1","locations":["call"],"phases":["validation"],"effect":"concealed-write","cost":0,"deterministic":true,"compatibility":"dangerous"}]}`,
			want:     "invalid effect",
		},
		{
			name:     "nondeterministic planning",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"directive-planning-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.bad","name":"bad","version":"1","capability":"vendor.bad-1","locations":["call"],"phases":["validation","planning"],"effect":"read","cost":0,"deterministic":false,"compatibility":"dangerous"}]}`,
			want:     "planning phase requires deterministic behavior",
		},
		{
			name:     "write without execution",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"directive-write-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.bad","name":"bad","version":"1","capability":"vendor.bad-1","locations":["call"],"phases":["validation"],"effect":"write","cost":0,"deterministic":true,"compatibility":"dangerous"}]}`,
			want:     "write effect requires the execution phase",
		},
		{
			name:     "cost above portable maximum",
			document: `{"version":"1","canonicalVersion":"c14n-1","revision":"directive-cost-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.bad","name":"bad","version":"1","capability":"vendor.bad-1","locations":["call"],"phases":["validation"],"effect":"read","cost":1048577,"deterministic":true,"compatibility":"dangerous"}]}`,
			want:     "cost exceeds the portable maximum",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := schema.ParseDocument([]byte(test.document), schema.ImportOptions{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("directive contract error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSchemaDocumentRejectsUnknownCriticalTraitsAndRetiredReuse(t *testing.T) {
	t.Parallel()
	critical := []byte(`{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","traits":[{"id":"vendor.execute-1","semantics":"execution","value":{}}],"types":[],"operations":[],"members":[]}`)
	if _, err := schema.ParseDocument(critical, schema.ImportOptions{}); err == nil || !strings.Contains(err.Error(), "unsupported critical trait") {
		t.Fatalf("critical trait error = %v", err)
	}
	if _, err := schema.ParseDocument(critical, schema.ImportOptions{SupportedTraits: map[string]bool{"vendor.execute-1": true}}); err != nil {
		t.Fatalf("supported critical trait: %v", err)
	}
	reused := []byte(`{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","retired":[{"id":"User","name":"OldUser","reason":"removed"}],"types":[{"id":"User","name":"NewUser","kind":"object","output":true,"fields":[{"id":"User.id","name":"id","type":"ID"}]}],"operations":[],"members":[]}`)
	if _, err := schema.ParseDocument(reused, schema.ImportOptions{}); err == nil || !strings.Contains(err.Error(), "reuses retired identity") {
		t.Fatalf("retired identity error = %v", err)
	}
}

func TestSchemaDocumentFilteringPreservesIntegrityWithoutHiddenNames(t *testing.T) {
	t.Parallel()
	document, err := schema.ExportDocument(portableSchemaSnapshot(t), portableOperations(), portableMembers(), schema.ExportOptions{Revision: "schema-r1"})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := document.Filter(schema.Visibility{
		Types:      map[schema.TypeID]bool{"User": true, "Role": true, "LookupInput": true},
		Fields:     map[string]bool{"User.id": true, "User.name": true, "LookupInput.id": true},
		EnumValues: map[string]bool{"Role.USER": true},
		Operations: map[string]bool{"query.user": true},
		Members:    map[string]bool{"User.id.resolver": true, "User.name.resolver": true},
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	canonical, err := filtered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"Admin", "adminSecret", "SearchResult", "query.search", "Role.ADMIN"} {
		if bytes.Contains(canonical, []byte(hidden)) {
			t.Fatalf("filtered discovery leaked %q: %s", hidden, canonical)
		}
	}
	if _, err := filtered.Snapshot(); err != nil {
		t.Fatalf("filtered snapshot has dangling references: %v", err)
	}
	if len(filtered.Types()) != 3 || len(filtered.Operations()) != 1 || len(filtered.Members()) != 2 {
		t.Fatalf("filtered sizes: types=%d operations=%d members=%d", len(filtered.Types()), len(filtered.Operations()), len(filtered.Members()))
	}
}

func TestSchemaDocumentFilteringDropsPartialInputsAndPrunesDanglingOutputs(t *testing.T) {
	t.Parallel()
	document := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"filter-r1",
		"types":[
			{"id":"Secret","name":"Secret","kind":"object","output":true,"fields":[{"id":"Secret.value","name":"value","type":"String"}]},
			{"id":"Args","name":"Args","kind":"input-object","input":true,"fields":[{"id":"Args.id","name":"id","type":"ID"},{"id":"Args.secret","name":"secret","type":"String"}]},
			{"id":"Public","name":"Public","kind":"object","output":true,"fields":[{"id":"Public.id","name":"id","type":"ID"},{"id":"Public.secret","name":"secret","type":"Secret"}]}
		],
		"operations":[{"id":"query.public","name":"public","kind":"query","input":"Args","output":"Public","effect":"read"}],"members":[]}`)
	filtered, err := document.Filter(schema.Visibility{
		Types:      map[schema.TypeID]bool{"Args": true, "Public": true},
		Fields:     map[string]bool{"Args.id": true, "Public.id": true, "Public.secret": true},
		Operations: map[string]bool{"query.public": true},
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	canonical, _ := filtered.CanonicalJSON()
	if bytes.Contains(canonical, []byte("Secret")) || bytes.Contains(canonical, []byte("secret")) || bytes.Contains(canonical, []byte("Args")) {
		t.Fatalf("filtered document leaked hidden or incomplete declarations: %s", canonical)
	}
	if len(filtered.Types()) != 1 || filtered.Types()[0].ID != "Public" || len(filtered.Types()[0].Fields) != 1 || len(filtered.Operations()) != 0 {
		t.Fatalf("filtered declarations = %#v %#v", filtered.Types(), filtered.Operations())
	}
}

func TestSchemaDocumentFilteringRequiresExplicitDirectiveVisibility(t *testing.T) {
	t.Parallel()
	document := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"directive-filter-r1",
		"types":[],"operations":[],"members":[],
		"directives":[
			{"id":"vendor.audit","name":"audit","version":"1","capability":"vendor.audit-1","locations":["call"],"arguments":[{"id":"vendor.audit.level","name":"level","type":"String"}],"phases":["validation"],"effect":"read","cost":1,"deterministic":true,"compatibility":"dangerous"},
			{"id":"vendor.secret","name":"secret","version":"1","capability":"vendor.secret-1","locations":["call"],"phases":["validation"],"effect":"read","cost":1,"deterministic":true,"compatibility":"dangerous"}
		]
	}`)
	filtered, err := document.Filter(schema.Visibility{Directives: map[string]bool{"vendor.audit": true}})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	directives := filtered.Directives()
	if len(directives) != 1 || directives[0].ID != "vendor.audit" {
		t.Fatalf("directives = %#v", directives)
	}
	canonical, err := filtered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte("vendor.secret")) {
		t.Fatalf("filtered discovery leaked hidden directive: %s", canonical)
	}
}

func TestSchemaDocumentFilteringRequiresExtensionVisibilityAndPrunesHiddenNames(t *testing.T) {
	t.Parallel()
	document := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"extension-filter-r1",
		"types":[],"operations":[],"members":[],
		"directives":[
			{"id":"vendor.audit","name":"audit","version":"1","capability":"vendor.audit-1","locations":["call"],"phases":["validation"],"effect":"read","cost":1,"deterministic":true,"compatibility":"dangerous"},
			{"id":"vendor.private","name":"privateHook","version":"1","capability":"vendor.private-1","locations":["call"],"phases":["validation"],"effect":"read","cost":1,"deterministic":true,"compatibility":"dangerous"}
		],
		"extensions":[
			{"id":"com.example.public","version":"1.0.0","capability":"com.example.public-1","implementation":"com.example.public.impl-1","points":["validation"],"directives":["audit","privateHook"],"deterministic":true,"sideEffects":"none","costBehavior":"none","compatibility":"additive","security":"closed view","before":["org.example.private"]},
			{"id":"org.example.private","version":"1.0.0","capability":"org.example.private-1","implementation":"org.example.private.impl-1","points":["validation"],"deterministic":true,"sideEffects":"none","costBehavior":"none","compatibility":"additive","security":"private view"}
		]
	}`)

	denied, err := document.Filter(schema.Visibility{})
	if err != nil {
		t.Fatalf("Filter deny all: %v", err)
	}
	if len(denied.Extensions()) != 0 {
		t.Fatalf("deny-all extensions = %#v", denied.Extensions())
	}

	filtered, err := document.Filter(schema.Visibility{
		Extensions: map[string]bool{"com.example.public": true},
		Directives: map[string]bool{"vendor.audit": true},
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	extensions := filtered.Extensions()
	if len(extensions) != 1 || extensions[0].ID != "com.example.public" || !slices.Equal(extensions[0].Directives, []string{"audit"}) ||
		len(extensions[0].Before) != 0 {
		t.Fatalf("extensions = %#v", extensions)
	}
	canonical, err := filtered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"org.example.private", "privateHook", "vendor.private"} {
		if bytes.Contains(canonical, []byte(hidden)) {
			t.Fatalf("filtered discovery leaked %q: %s", hidden, canonical)
		}
	}
}

func TestSchemaDiffClassifiesLifecycleAndCompatibilityChanges(t *testing.T) {
	t.Parallel()
	before := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1",
		"types":[
			{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"ID"}]},
			{"id":"Role","name":"Role","kind":"enum","output":true,"open":true,"enumMembers":[{"id":"Role.USER","name":"USER"}]},
			{"id":"Result","name":"Result","kind":"union","output":true,"open":true,"variantMembers":[{"id":"Result.User","type":"User"}]},
			{"id":"Removed","name":"Removed","kind":"object","output":true,"fields":[{"id":"Removed.id","name":"id","type":"ID"}]},
			{"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String","nullable":true}]}
		],
		"operations":[{"id":"query.user","name":"user","kind":"query","input":"Input","output":"User","effect":"read"}],"members":[]}`)
	after := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"schema-r2",
		"types":[
			{"id":"Admin","name":"Admin","kind":"object","output":true,"fields":[{"id":"Admin.id","name":"id","type":"ID"}]},
			{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"ID","default":"anonymous"},{"id":"Input.scope","name":"scope","type":"String","required":true}]},
			{"id":"Role","name":"AccessRole","kind":"enum","output":true,"open":false,"enumMembers":[{"id":"Role.USER","name":"USER"}]},
			{"id":"Result","name":"Result","kind":"union","output":true,"open":true,"variantMembers":[{"id":"Result.User","type":"User"},{"id":"Result.Admin","type":"Admin"}]},
			{"id":"User","name":"User","kind":"object","output":true,"fields":[{"id":"User.name","name":"name","type":"String","nullable":false,"deprecation":{"reason":"use displayName","replacement":"displayName"}}]}
		],
		"operations":[{"id":"query.user","name":"user","kind":"query","input":"Input","output":"User","effect":"write"}],"members":[]}`)
	diff := schema.DiffDocuments(before, after)
	want := map[string]schema.ChangeClassification{
		"type:Removed":                schema.ChangeBreaking,
		"type:Role/name":              schema.ChangeBreaking,
		"type:Role/open":              schema.ChangeBreaking,
		"field:Input.scope":           schema.ChangeBreaking,
		"field:User.name/nullable":    schema.ChangeBreaking,
		"field:Input.id/default":      schema.ChangeBehaviorOnly,
		"field:User.name/deprecation": schema.ChangeBehaviorOnly,
		"variant:Result.Admin":        schema.ChangeAdditive,
		"operation:query.user/effect": schema.ChangeDangerous,
	}
	got := make(map[string]schema.ChangeClassification)
	for _, change := range diff.Changes {
		got[change.Path] = change.Classification
	}
	for path, classification := range want {
		if got[path] != classification {
			t.Errorf("change %s = %q, want %q; all=%#v", path, got[path], classification, diff.Changes)
		}
	}
	if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
		return change.Path == "type:Admin" && change.Classification == schema.ChangeAdditive
	}) {
		t.Fatalf("added type not classified: %#v", diff.Changes)
	}
}

func TestSchemaDiffDetectsRetiredReuseTypeChangesAndBehaviorConstraints(t *testing.T) {
	t.Parallel()
	before := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1",
		"types":[{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"String","cost":1}]}],
		"operations":[{"id":"query.value","name":"value","kind":"query","input":"Input","output":"String","effect":"read","authorizationPolicy":"old.policy","cost":1}],
		"members":[],"retired":[{"id":"Legacy","name":"Legacy","reason":"removed"}]}`)
	after := parseSchemaDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"schema-r2",
		"types":[
			{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"ID","cost":2}]},
			{"id":"Legacy","name":"Legacy","kind":"object","output":true,"fields":[{"id":"Legacy.id","name":"id","type":"ID"}]}
		],
		"operations":[{"id":"query.value","name":"value","kind":"query","input":"Input","output":"String","effect":"read","authorizationPolicy":"new.policy","cost":2}],"members":[]}`)
	diff := schema.DiffDocuments(before, after)
	want := map[string]schema.ChangeClassification{
		"identity:Legacy/reuse":                     schema.ChangeBreaking,
		"field:Input.id/type":                       schema.ChangeBreaking,
		"field:Input.id/cost":                       schema.ChangeDangerous,
		"operation:query.value/authorizationPolicy": schema.ChangeDangerous,
		"operation:query.value/cost":                schema.ChangeDangerous,
	}
	for path, classification := range want {
		if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
			return change.Path == path && change.Classification == classification
		}) {
			t.Errorf("missing %s=%s in %#v", path, classification, diff.Changes)
		}
	}
}

func TestSchemaDiffClassifiesScalarCanonicalizationAsDangerous(t *testing.T) {
	t.Parallel()
	makeDocument := func(revision string, descriptor schema.TypeDescriptor) schema.Document {
		catalog := schema.NewCatalog()
		if err := catalog.RegisterScalar(descriptor); err != nil {
			t.Fatalf("RegisterScalar: %v", err)
		}
		snapshot, err := catalog.Freeze()
		if err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		document, err := schema.ExportDocument(snapshot, nil, nil, schema.ExportOptions{Revision: revision})
		if err != nil {
			t.Fatalf("ExportDocument: %v", err)
		}
		return document
	}
	beforeScalar := customScalarDescriptor("Slug", []schema.JSONShape{schema.JSONString}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"Alpha"`), Canonical: json.RawMessage(`"alpha"`)},
		{Input: json.RawMessage(`"Beta"`), Canonical: json.RawMessage(`"beta"`)},
	})
	afterScalar := customScalarDescriptor("Slug", []schema.JSONShape{schema.JSONString, schema.JSONNumber}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"Alpha"`), Canonical: json.RawMessage(`"Alpha"`)},
		{Input: json.RawMessage(`7`), Canonical: json.RawMessage(`7`)},
	})
	diff := schema.DiffDocuments(makeDocument("scalar-r1", beforeScalar), makeDocument("scalar-r2", afterScalar))
	if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
		return change.Path == "type:Slug/scalar" && change.Classification == schema.ChangeDangerous
	}) {
		t.Fatalf("scalar canonicalization change = %#v", diff.Changes)
	}
}

func TestPortableSchemaIncludesDirectiveSemanticsInIdentityAndDiff(t *testing.T) {
	t.Parallel()
	before := parseSchemaDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"directives-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.audit","name":"audit","version":"1","capability":"vendor.audit-1","repeatable":true,"locations":["call","field"],"arguments":[{"id":"vendor.audit.level","name":"level","type":"String","required":true}],"phases":["validation","execution","response"],"effect":"read","cost":3,"deterministic":true,"compatibility":"dangerous"}]}`)
	after := parseSchemaDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"directives-r2","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.audit","name":"audit","version":"2","capability":"vendor.audit-2","repeatable":true,"locations":["call","field"],"arguments":[{"id":"vendor.audit.level","name":"level","type":"String","required":true}],"phases":["validation","execution","response"],"effect":"read","cost":3,"deterministic":true,"compatibility":"dangerous"}]}`)

	beforeHash, err := before.Hash()
	if err != nil {
		t.Fatal(err)
	}
	afterHash, err := after.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash == afterHash {
		t.Fatal("directive version and capability did not change schema identity")
	}
	diff := schema.DiffDocuments(before, after)
	if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
		return change.Path == "directive:vendor.audit/version" && change.Classification == schema.ChangeBreaking
	}) {
		t.Fatalf("directive diff = %#v", diff.Changes)
	}
}

func TestSchemaDiffClassifiesDirectiveLocationExpansionAsAdditive(t *testing.T) {
	t.Parallel()
	before := parseSchemaDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"directives-r1","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.audit","name":"audit","version":"1","capability":"vendor.audit-1","locations":["call"],"phases":["validation"],"effect":"read","cost":1,"deterministic":true,"compatibility":"dangerous"}]}`)
	after := parseSchemaDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"directives-r2","types":[],"operations":[],"members":[],"directives":[{"id":"vendor.audit","name":"audit","version":"1","capability":"vendor.audit-1","locations":["call","field"],"phases":["validation"],"effect":"read","cost":1,"deterministic":true,"compatibility":"dangerous"}]}`)
	diff := schema.DiffDocuments(before, after)
	if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
		return change.Path == "directive:vendor.audit/locations" && change.Classification == schema.ChangeAdditive
	}) {
		t.Fatalf("directive location diff = %#v", diff.Changes)
	}
}

func portableSchemaSnapshot(t testing.TB) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	descriptors := []schema.TypeDescriptor{
		{ID: "LookupInput", Name: "LookupInput", Kind: schema.InputObjectType, Input: true, Description: "Lookup arguments", Fields: map[string]schema.FieldDescriptor{
			"id": {ID: "LookupInput.id", Type: schema.TypeID(schema.ID), Required: true},
		}},
		{ID: "Role", Name: "Role", Kind: schema.EnumType, Output: true, Open: true, EnumValues: []string{"ADMIN", "USER"}, EnumMembers: []schema.EnumMemberDescriptor{
			{ID: "Role.ADMIN", Name: "ADMIN", Deprecation: &schema.Deprecation{Reason: "use USER", Replacement: "Role.USER", Sunset: "2027-01-01T00:00:00Z"}},
			{ID: "Role.USER", Name: "USER"},
		}},
		{ID: "User", Name: "User", Kind: schema.ObjectType, Output: true, MaxDepth: 2, Description: "Account profile", Entity: &schema.EntityDescriptor{Keys: []string{"id"}}, Fields: map[string]schema.FieldDescriptor{
			"id":     {ID: "User.id", Type: schema.TypeID(schema.ID), Description: "Stable account ID"},
			"name":   {ID: "User.name", Type: schema.TypeID(schema.String), Description: "Display name", Deprecation: &schema.Deprecation{Reason: "use displayName", Replacement: "displayName"}},
			"role":   {ID: "User.role", Type: "Role"},
			"friend": {ID: "User.friend", Type: "User", Nullable: true},
		}},
		{ID: "Admin", Name: "Admin", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"id":          {ID: "Admin.id", Type: schema.TypeID(schema.ID)},
			"adminSecret": {ID: "Admin.adminSecret", Type: schema.TypeID(schema.String)},
		}},
		{ID: "SearchResult", Name: "SearchResult", Kind: schema.UnionType, Output: true, Open: true, Variants: []schema.TypeID{"Admin", "User"}, VariantMembers: []schema.VariantMemberDescriptor{
			{ID: "SearchResult.Admin", Type: "Admin"}, {ID: "SearchResult.User", Type: "User"},
		}},
	}
	for _, descriptor := range descriptors {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("Register(%s): %v", descriptor.ID, err)
		}
	}
	slug := customScalarDescriptor("Slug", []schema.JSONShape{schema.JSONString}, []schema.ScalarConformanceVector{
		{Input: json.RawMessage(`"Alpha"`), Canonical: json.RawMessage(`"alpha"`)},
		{Input: json.RawMessage(`"Beta"`), Canonical: json.RawMessage(`"beta"`)},
	})
	slug.Name, slug.Description = "Slug", "Portable lowercase slug"
	if err := catalog.RegisterScalar(slug); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func portableOperations() []schema.OperationDescriptor {
	return []schema.OperationDescriptor{
		{ID: "query.search", Name: "search", Kind: protocol.Query, Input: "LookupInput", Output: "SearchResult", Effect: "read", AuthorizationPolicy: "search.read", Cost: 5},
		{ID: "query.user", Name: "user", Kind: protocol.Query, Input: "LookupInput", Output: "User", Effect: "read", AuthorizationPolicy: "user.read", Cost: 2},
	}
}

func portableMembers() []schema.MemberDescriptor {
	return []schema.MemberDescriptor{
		{ID: "User.id.resolver", Name: "id", Owner: "User", Kind: "field", Output: schema.TypeID(schema.ID), Effect: "read"},
		{ID: "User.name.resolver", Name: "name", Owner: "User", Kind: "field", Output: schema.TypeID(schema.String), Effect: "read", Deprecation: &schema.Deprecation{Reason: "use displayName", Replacement: "User.displayName"}},
	}
}

func parseSchemaDocument(t testing.TB, input string) schema.Document {
	t.Helper()
	document, err := schema.ParseDocument([]byte(input), schema.ImportOptions{})
	switch err {
	case nil:
		return document
	default:
		t.Fatalf("ParseDocument: %v", err)
		return schema.Document{}
	}
}
