package runtime_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestTypedCollectionQueryCursorRejectsEveryScopeMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 14, 0, 0, 0, 0, time.UTC)
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "active", Keys: map[string][]byte{"active": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, Now: func() time.Time { return now }, MaxPageSize: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	filter := schema.CollectionFilterExpression{Kind: schema.CollectionFilterPredicate, Field: "User.age", Operator: schema.FilterGreaterEqual, Value: &schema.CollectionFilterValue{Type: schema.TypeID(schema.Int64), Literal: json.RawMessage(`"18"`)}}
	sorts := []schema.CollectionSortTerm{
		{Field: "User.name", Direction: schema.SortAscending, Nulls: schema.NullsLast},
		{Field: "User.age", Direction: schema.SortDescending, Nulls: schema.NullsFirst},
	}
	options := runtime.CursorScopeOptions{Tenant: "tenant-a", Authorization: "policy-r1", SnapshotPolicy: runtime.SnapshotPagination, Snapshot: "snapshot-r1"}
	scope, err := runtime.NewCollectionQueryCursorScope("users", filter, nil, sorts, options)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := codec.Encode(scope, runtime.CursorForward, runtime.CursorPosition{SortKey: "Ada/42", TieBreaker: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	changedFilter := filter
	changedFilter.Value = &schema.CollectionFilterValue{Type: schema.TypeID(schema.Int64), Literal: json.RawMessage(`"19"`)}
	changedSort := append([]schema.CollectionSortTerm(nil), sorts...)
	changedSort[1].Direction = schema.SortAscending
	tests := []struct {
		name    string
		filter  schema.CollectionFilterExpression
		sorts   []schema.CollectionSortTerm
		options runtime.CursorScopeOptions
	}{
		{name: "filter", filter: changedFilter, sorts: sorts, options: options},
		{name: "multi-key sort", filter: filter, sorts: changedSort, options: options},
		{name: "tenant", filter: filter, sorts: sorts, options: runtime.CursorScopeOptions{Tenant: "tenant-b", Authorization: "policy-r1", SnapshotPolicy: runtime.SnapshotPagination, Snapshot: "snapshot-r1"}},
		{name: "authorization", filter: filter, sorts: sorts, options: runtime.CursorScopeOptions{Tenant: "tenant-a", Authorization: "policy-r2", SnapshotPolicy: runtime.SnapshotPagination, Snapshot: "snapshot-r1"}},
		{name: "snapshot", filter: filter, sorts: sorts, options: runtime.CursorScopeOptions{Tenant: "tenant-a", Authorization: "policy-r1", SnapshotPolicy: runtime.SnapshotPagination, Snapshot: "snapshot-r2"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed, scopeErr := runtime.NewCollectionQueryCursorScope("users", test.filter, nil, test.sorts, test.options)
			if scopeErr != nil {
				t.Fatal(scopeErr)
			}
			if _, decodeErr := codec.Decode(cursor, changed, runtime.CursorForward); !errors.Is(decodeErr, runtime.ErrInvalidCursor) {
				t.Fatalf("Decode error = %v, want INVALID_CURSOR", decodeErr)
			}
		})
	}
}

func TestTypedCollectionQueryCursorBindsResolvedVariableResults(t *testing.T) {
	t.Parallel()
	filter := schema.CollectionFilterExpression{Kind: schema.CollectionFilterPredicate, Field: "User.age", Operator: schema.FilterGreaterEqual, Value: &schema.CollectionFilterValue{Type: schema.TypeID(schema.Int64), Variable: "minimumAge"}}
	sorts := []schema.CollectionSortTerm{{Field: "User.age", Direction: schema.SortAscending, Nulls: schema.NullsLast}}
	options := runtime.CursorScopeOptions{Tenant: "tenant-a"}
	left, err := runtime.NewCollectionQueryCursorScope("users", filter, map[string]json.RawMessage{"minimumAge": json.RawMessage(`"18"`)}, sorts, options)
	if err != nil {
		t.Fatal(err)
	}
	right, err := runtime.NewCollectionQueryCursorScope("users", filter, map[string]json.RawMessage{"minimumAge": json.RawMessage(`"19"`)}, sorts, options)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "active", Keys: map[string][]byte{"active": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, Now: func() time.Time { return time.Date(2026, time.September, 14, 0, 0, 0, 0, time.UTC) }, MaxPageSize: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := codec.Encode(left, runtime.CursorForward, runtime.CursorPosition{SortKey: "18", TieBreaker: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(cursor, right, runtime.CursorForward); !errors.Is(err, runtime.ErrInvalidCursor) {
		t.Fatalf("Decode error = %v, want INVALID_CURSOR", err)
	}
	if _, err := runtime.NewCollectionQueryCursorScope("users", filter, nil, sorts, options); err == nil {
		t.Fatal("missing resolved variable accepted")
	}
}

func TestTypedCollectionQueryCursorCanonicalizesLiteralObjects(t *testing.T) {
	t.Parallel()
	leftFilter := schema.CollectionFilterExpression{Kind: schema.CollectionFilterPredicate, Field: "User.location", Operator: schema.FilterGeoWithin, Value: &schema.CollectionFilterValue{Type: "Geo", Literal: json.RawMessage(`{"lat":57,"lon":24}`)}}
	rightFilter := leftFilter
	rightFilter.Value = &schema.CollectionFilterValue{Type: "Geo", Literal: json.RawMessage(`{ "lon": 24, "lat": 57 }`)}
	left, err := runtime.NewCollectionQueryCursorScope("users", leftFilter, nil, []schema.CollectionSortTerm{{Field: "User.id", Direction: schema.SortAscending, Nulls: schema.NullsLast}}, runtime.CursorScopeOptions{Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := runtime.NewCollectionQueryCursorScope("users", rightFilter, nil, []schema.CollectionSortTerm{{Field: "User.id", Direction: schema.SortAscending, Nulls: schema.NullsLast}}, runtime.CursorScopeOptions{Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("equivalent literal objects produced different cursor scopes")
	}
}
