package schema_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestCollectionQueryDescriptorCanonicalRoundTripAndImmutability(t *testing.T) {
	t.Parallel()
	document := parseCollectionQueryDocument(t)
	operations := document.Operations()
	if len(operations) != 1 || operations[0].Collection == nil || operations[0].Collection.Query == nil {
		t.Fatalf("collection query descriptor = %#v", operations)
	}
	query := operations[0].Collection.Query
	if query.Capability != "collection.query-1" || len(query.Fields) != 3 || query.Fields[0].ID != "User.age" || query.Fields[1].ID != "User.name" || query.Fields[2].ID != "User.tags" {
		t.Fatalf("normalized query descriptor = %#v", query)
	}
	if got := query.Fields[1].Operators; len(got) != 4 || got[0] != schema.FilterEqual || got[3] != schema.FilterRegex {
		t.Fatalf("normalized operators = %v", got)
	}
	canonical, err := document.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(canonical, []byte(`"capability":"collection.query-1"`)) || !bytes.Contains(canonical, []byte(`"tieBreaker":{"caseSensitivity":"sensitive","collation":"unicode-code-point","type":"ID"}`)) {
		t.Fatalf("canonical collection query = %s", canonical)
	}
	query.Fields[0].Path[0] = "mutated"
	query.Fields[1].Operators[0] = "mutated"
	query.Fields[1].Sort.Directions[0] = "mutated"
	again, _ := document.CanonicalJSON()
	if !bytes.Equal(again, canonical) {
		t.Fatal("schema document mutated through collection query accessors")
	}
}

func TestCollectionQueryDescriptorRejectsInvalidContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{name: "unknown type", old: `"type":"String"`, new: `"type":"Unknown"`, want: "unknown type"},
		{name: "duplicate identity", old: `"id":"User.name"`, new: `"id":"User.age"`, want: "duplicate collection query field identity"},
		{name: "duplicate path", old: `"path":["name"]`, new: `"path":["age"]`, want: "duplicate collection query field path"},
		{name: "zero depth", old: `"maxDepth":8`, new: `"maxDepth":0`, want: "positive query limits"},
		{name: "unbounded cost", old: `"maxCost":100`, new: `"maxCost":1048577`, want: "query cost exceeds"},
		{name: "nullable operator mismatch", old: `"operators":["eq","gte","in"]`, new: `"operators":["eq","gte","in","is-null"]`, want: "requires nullable values"},
		{name: "missing operator mismatch", old: `"operators":["eq","gte","in"]`, new: `"operators":["eq","exists","gte","in"]`, want: "requires missing values"},
		{name: "list operator mismatch", old: `"operators":["all","any"]`, new: `"operators":["eq"]`, want: "requires any or all"},
		{name: "list equality mismatch", old: `"operators":["all","any"]`, new: `"operators":["all","any","eq"]`, want: "incompatible equality or membership type"},
		{name: "collection tie breaker", old: `"tieBreaker":{"caseSensitivity":"sensitive","collation":"unicode-code-point","type":"ID"}`, new: `"tieBreaker":{"caseSensitivity":"sensitive","collation":"unicode-code-point","type":"Tags"}`, want: "tie breaker requires a scalar or enum"},
		{name: "regex capability", old: `"capabilities":["collection.regex-1"]`, new: `"capabilities":[]`, want: "regex operator requires"},
		{name: "invalid collation", old: `"collation":"unicode-code-point"`, new: `"collation":"host-default"`, want: "invalid collection sort collation"},
	}
	canonical, err := parseCollectionQueryDocument(t).CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(canonical, []byte(test.old), []byte(test.new), 1)
			if bytes.Equal(changed, canonical) {
				t.Fatalf("test replacement %q was not found", test.old)
			}
			_, parseErr := schema.ParseDocument(changed, schema.ImportOptions{})
			if parseErr == nil || !strings.Contains(parseErr.Error(), test.want) {
				t.Fatalf("ParseDocument error = %v, want %q", parseErr, test.want)
			}
		})
	}
}

func TestCollectionQueryRejectsIncompatibleOperatorTypes(t *testing.T) {
	t.Parallel()
	document := parseCollectionQueryDocument(t)
	types, err := document.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*schema.CollectionQueryDescriptor)
		want string
	}{
		{name: "ordered Boolean", edit: func(query *schema.CollectionQueryDescriptor) {
			query.Fields[0].Type = schema.TypeID(schema.Boolean)
		}, want: "incompatible sort type"},
		{name: "geospatial String", edit: func(query *schema.CollectionQueryDescriptor) {
			query.Fields[1].Operators = append(query.Fields[1].Operators, schema.FilterGeoWithin)
			query.Fields[1].Capabilities = append(query.Fields[1].Capabilities, "collection.geospatial-1")
		}, want: "incompatible geospatial operator type"},
		{name: "numeric Unicode collation", edit: func(query *schema.CollectionQueryDescriptor) {
			query.Fields[0].Sort.Collation = "unicode-code-point"
		}, want: "incompatible sort ordering policy"},
		{name: "numeric ID tie breaker", edit: func(query *schema.CollectionQueryDescriptor) {
			query.TieBreaker.Collation = "numeric"
		}, want: "incompatible ordering policy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query := document.Operations()[0].Collection.Query
			test.edit(query)
			if err := schema.ValidateCollectionQueryTypes(query, types); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateCollectionQueryTypes error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCollectionQueryCaseInsensitiveSortRequiresCapability(t *testing.T) {
	t.Parallel()
	query := parseCollectionQueryDocument(t).Operations()[0].Collection.Query
	query.Fields[1].Sort.CaseSensitivity = "insensitive"
	if err := schema.ValidateCollectionQueryDescriptor(query); err == nil || !strings.Contains(err.Error(), schema.CollectionCaseFoldCapability) {
		t.Fatalf("ValidateCollectionQueryDescriptor error = %v", err)
	}
	query.Fields[1].Capabilities = append(query.Fields[1].Capabilities, schema.CollectionCaseFoldCapability)
	if err := schema.ValidateCollectionQueryDescriptor(query); err != nil {
		t.Fatalf("case-fold capability rejected: %v", err)
	}
}

func TestFilteredDiscoveryPrunesUnauthorizedCollectionQueryFields(t *testing.T) {
	t.Parallel()
	document := parseCollectionQueryDocument(t)
	filtered, err := document.Filter(schema.Visibility{
		Types:            map[schema.TypeID]bool{"User": true, "Users": true},
		Fields:           map[string]bool{"User.age": true, "User.name": true, "User.tags": true},
		Operations:       map[string]bool{"query.users": true},
		CollectionFields: map[string]bool{"User.age": true, "User.tags": true},
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	query := filtered.Operations()[0].Collection.Query
	if len(query.Fields) != 1 || query.Fields[0].ID != "User.age" {
		t.Fatalf("filtered collection fields = %#v", query.Fields)
	}
	canonical, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"User.name", "User.tags", "regex"} {
		if bytes.Contains(canonical, []byte(hidden)) {
			t.Fatalf("filtered discovery leaked %q: %s", hidden, canonical)
		}
	}
	if !slices.Equal(filtered.Operations()[0].Capabilities, []string{schema.CollectionQueryCapability}) {
		t.Fatalf("filtered capability = %v", filtered.Operations()[0].Capabilities)
	}
}

func TestFilteredDiscoveryRemovesEmptyCollectionQueryCapability(t *testing.T) {
	t.Parallel()
	document := parseCollectionQueryDocument(t)
	filtered, err := document.Filter(schema.Visibility{
		Types:      map[schema.TypeID]bool{"User": true, "Users": true, "Tags": true},
		Fields:     map[string]bool{"User.age": true, "User.name": true, "User.tags": true},
		Operations: map[string]bool{"query.users": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := filtered.Operations()[0]
	if operation.Collection.Query != nil || slices.Contains(operation.Capabilities, schema.CollectionQueryCapability) {
		t.Fatalf("empty filtered query remains advertised: %#v", operation)
	}
}

func TestSchemaDiffClassifiesCollectionQueryChanges(t *testing.T) {
	t.Parallel()
	before := parseCollectionQueryDocument(t)
	canonical, err := before.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(canonical, []byte(`"maxCost":100`), []byte(`"maxCost":99`), 1)
	after, err := schema.ParseDocument(changed, schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	diff := schema.DiffDocuments(before, after)
	if !slices.ContainsFunc(diff.Changes, func(change schema.SchemaChange) bool {
		return change.Path == "operation:query.users/collection/query" && change.Classification == schema.ChangeDangerous
	}) {
		t.Fatalf("query diff = %#v", diff.Changes)
	}
}

func parseCollectionQueryDocument(t testing.TB) schema.Document {
	t.Helper()
	document, err := schema.ParseDocument([]byte(`{
		"version":"1","canonicalVersion":"c14n-1","revision":"collection-query-r1",
		"types":[
			{"id":"Tags","kind":"list","output":true,"element":"String"},
			{"id":"User","kind":"object","output":true,"fields":[{"id":"User.age","name":"age","type":"Int64"},{"id":"User.name","name":"name","type":"String","nullable":true},{"id":"User.tags","name":"tags","type":"Tags"}]},
			{"id":"Users","kind":"list","output":true,"element":"User"}
		],
		"operations":[{
			"id":"query.users","name":"users","kind":"query","output":"Users","effect":"read","capabilities":["collection.query-1"],
			"collection":{"maxPageSize":25,"query":{
				"capability":"collection.query-1",
				"fields":[
					{"id":"User.tags","path":["tags"],"type":"Tags","elementType":"String","operators":["any","all"],"cost":3,"indexed":true},
					{"id":"User.name","path":["name"],"type":"String","nullable":true,"missing":true,"operators":["regex","ne","eq","is-null"],"cost":2,"indexed":true,"capabilities":["collection.regex-1"],"sort":{"directions":["desc","asc"],"nulls":["last","first"],"collation":"unicode-code-point","caseSensitivity":"sensitive"}},
					{"id":"User.age","path":["age"],"type":"Int64","operators":["in","gte","eq"],"cost":1,"indexed":true,"sort":{"directions":["asc"],"nulls":["last"],"collation":"numeric","caseSensitivity":"sensitive"}}
				],
				"tieBreaker":{"type":"ID","collation":"unicode-code-point","caseSensitivity":"sensitive"},
				"limits":{"maxDepth":8,"maxPredicates":32,"maxMembership":50,"maxSortKeys":3,"maxCost":100}
			}}
		}]
	}`), schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	return document
}
