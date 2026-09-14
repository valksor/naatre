package collectionquery_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/collectionquery"
	"github.com/valksor/naatre/schema"
)

func TestReferenceEvaluatorDistinguishesMissingNullAndValues(t *testing.T) {
	t.Parallel()
	query := queryContract()
	tests := []struct {
		name   string
		row    collectionquery.Row
		filter schema.CollectionFilterExpression
		want   bool
	}{
		{name: "missing comparison is false", row: row("1"), filter: predicate("User.name", schema.FilterEqual, schema.String, `"Ada"`), want: false},
		{name: "missing inequality is also false", row: row("1"), filter: predicate("User.name", schema.FilterNotEqual, schema.String, `"Ada"`), want: false},
		{name: "missing is not null", row: row("1"), filter: unaryPredicate("User.name", schema.FilterIsNull), want: false},
		{name: "missing does not exist", row: row("1"), filter: unaryPredicate("User.name", schema.FilterNotExists), want: true},
		{name: "explicit null exists", row: row("1", "name", nil), filter: unaryPredicate("User.name", schema.FilterExists), want: true},
		{name: "explicit null is null", row: row("1", "name", nil), filter: unaryPredicate("User.name", schema.FilterIsNull), want: true},
		{name: "null comparison is false", row: row("1", "name", nil), filter: predicate("User.name", schema.FilterNotEqual, schema.String, `"Ada"`), want: false},
		{name: "value comparison", row: row("1", "name", "Ada"), filter: predicate("User.name", schema.FilterEqual, schema.String, `"Ada"`), want: true},
		{name: "boolean not remains two-valued", row: row("1"), filter: schema.CollectionFilterExpression{Kind: schema.CollectionFilterNot, Child: ptrFilter(predicate("User.name", schema.FilterEqual, schema.String, `"Ada"`))}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := collectionquery.Evaluate(queryTypes(t), query, test.filter, test.row, nil)
			if err != nil || got != test.want {
				t.Fatalf("Evaluate = %t, %v; want %t", got, err, test.want)
			}
		})
	}
}

func TestReferenceEvaluatorDefinesListAnyAllAndEmptyList(t *testing.T) {
	t.Parallel()
	query := queryContract()
	vip := predicate("@", schema.FilterEqual, schema.String, `"vip"`)
	tests := []struct {
		name string
		row  collectionquery.Row
		kind schema.CollectionFilterKind
		want bool
	}{
		{name: "any match", row: row("1", "tags", []any{"new", "vip"}), kind: schema.CollectionFilterAny, want: true},
		{name: "all match", row: row("1", "tags", []any{"vip", "vip"}), kind: schema.CollectionFilterAll, want: true},
		{name: "all non-match", row: row("1", "tags", []any{"vip", "new"}), kind: schema.CollectionFilterAll, want: false},
		{name: "empty any false", row: row("1", "tags", []any{}), kind: schema.CollectionFilterAny, want: false},
		{name: "empty all true", row: row("1", "tags", []any{}), kind: schema.CollectionFilterAll, want: true},
		{name: "missing all false", row: row("1"), kind: schema.CollectionFilterAll, want: false},
		{name: "null all false", row: row("1", "tags", nil), kind: schema.CollectionFilterAll, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filter := schema.CollectionFilterExpression{Kind: test.kind, Field: "User.tags", Predicate: &vip}
			got, err := collectionquery.Evaluate(queryTypes(t), query, filter, test.row, nil)
			if err != nil || got != test.want {
				t.Fatalf("Evaluate = %t, %v; want %t", got, err, test.want)
			}
		})
	}
}

func TestFilterPreflightRejectsHiddenUnsupportedAndOverBudgetBeforeEvaluation(t *testing.T) {
	t.Parallel()
	query := queryContract()
	tests := []struct {
		name   string
		filter schema.CollectionFilterExpression
		code   string
		secret string
	}{
		{name: "hidden field", filter: predicate("User.salary", schema.FilterEqual, schema.Int64, `1`), code: collectionquery.CodeUnsupported, secret: "salary"},
		{name: "unsupported operator", filter: predicate("User.age", schema.FilterRegex, schema.String, `".*"`), code: collectionquery.CodeUnsupported, secret: "regex"},
		{name: "oversized membership", filter: predicate("User.age", schema.FilterIn, schema.Int64, `[1,2,3,4,5]`), code: collectionquery.CodeResourceExhausted},
		{name: "deep predicate", filter: nestedNot(predicate("User.age", schema.FilterEqual, schema.Int64, `1`), 5), code: collectionquery.CodeResourceExhausted},
		{name: "outer join from list scope", filter: schema.CollectionFilterExpression{Kind: schema.CollectionFilterAny, Field: "User.tags", Predicate: ptrFilter(predicate("User.name", schema.FilterEqual, schema.String, `"Ada"`))}, code: collectionquery.CodeUnsupported},
		{name: "trailing literal", filter: predicate("User.age", schema.FilterEqual, schema.Int64, `1 2`), code: collectionquery.CodeInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := collectionquery.Prepare(queryTypes(t), query, test.filter, nil)
			var diagnostic *collectionquery.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != test.code {
				t.Fatalf("Prepare error = %v, want code %s", err, test.code)
			}
			if test.secret != "" && strings.Contains(strings.ToLower(err.Error()), test.secret) {
				t.Fatalf("safe error leaked %q: %v", test.secret, err)
			}
		})
	}
}

func TestFilterPreflightCoercesExactSchemaTypes(t *testing.T) {
	t.Parallel()
	types := queryTypes(t)
	query := queryContract()
	query.Fields = append(query.Fields, schema.CollectionQueryFieldDescriptor{
		ID: "User.status", Path: []string{"status"}, Type: "Status",
		Operators: []schema.FilterOperator{schema.FilterEqual, schema.FilterIn}, Cost: 1, Indexed: true,
	})
	tests := []struct {
		name      string
		filter    schema.CollectionFilterExpression
		variables collectionquery.Variables
	}{
		{name: "invalid Int64 literal", filter: predicate("User.age", schema.FilterEqual, schema.Int64, `"not-an-int"`)},
		{name: "wrong Int64 wire shape", filter: predicate("User.age", schema.FilterEqual, schema.Int64, `7`)},
		{name: "invalid Int64 variable", filter: variablePredicate("User.age", schema.FilterEqual, schema.TypeID(schema.Int64), "age"), variables: collectionquery.Variables{"age": json.RawMessage(`"not-an-int"`)}},
		{name: "unknown closed enum literal", filter: typedPredicate("User.status", schema.FilterEqual, "Status", `"DELETED"`)},
		{name: "unknown closed enum member", filter: typedPredicate("User.status", schema.FilterIn, "Status", `["ACTIVE","DELETED"]`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := collectionquery.Prepare(types, query, test.filter, test.variables)
			var diagnostic *collectionquery.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != collectionquery.CodeInvalid {
				t.Fatalf("Prepare error = %v, want %s", err, collectionquery.CodeInvalid)
			}
		})
	}
}

func TestFilterPreflightEnforcesPredicateAndDeclaredCostBudgets(t *testing.T) {
	t.Parallel()
	predicates := []schema.CollectionFilterExpression{
		predicate("User.age", schema.FilterEqual, schema.Int64, `"1"`),
		predicate("User.age", schema.FilterEqual, schema.Int64, `"2"`),
		predicate("User.age", schema.FilterEqual, schema.Int64, `"3"`),
	}
	tests := []struct {
		name  string
		query schema.CollectionQueryDescriptor
		code  string
	}{
		{name: "predicate count", query: func() schema.CollectionQueryDescriptor {
			query := queryContract()
			query.Limits.MaxPredicates = 2
			return query
		}(), code: collectionquery.CodeResourceExhausted},
		{name: "unindexed declared cost", query: func() schema.CollectionQueryDescriptor {
			query := queryContract()
			query.Fields[0].Indexed = false
			query.Fields[0].Cost = 5
			return query
		}(), code: collectionquery.CodeResourceExhausted},
	}
	filter := schema.CollectionFilterExpression{Kind: schema.CollectionFilterAnd, Children: predicates}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := collectionquery.Prepare(queryTypes(t), test.query, filter, nil)
			var diagnostic *collectionquery.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != test.code {
				t.Fatalf("Prepare error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestAdvertisedOptionalOperatorsAndCaseFoldRemainProviderGated(t *testing.T) {
	t.Parallel()
	query := queryContract()
	query.Fields[1].Operators = append(query.Fields[1].Operators, schema.FilterRegex)
	query.Fields[1].Capabilities = append(query.Fields[1].Capabilities, "collection.regex-1")
	_, err := collectionquery.Prepare(queryTypes(t), query, predicate("User.name", schema.FilterRegex, schema.String, `"^A"`), nil)
	var diagnostic *collectionquery.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != collectionquery.CodeUnsupported {
		t.Fatalf("regex Prepare error = %v", err)
	}

	query = queryContract()
	query.Fields[1].Sort.CaseSensitivity = "insensitive"
	query.Fields[1].Capabilities = append(query.Fields[1].Capabilities, schema.CollectionCaseFoldCapability)
	_, err = collectionquery.Sort(queryTypes(t), query, []schema.CollectionSortTerm{{Field: "User.name", Direction: schema.SortAscending, Nulls: schema.NullsLast}}, []collectionquery.Row{row("1", "name", "Ada")})
	if !errors.As(err, &diagnostic) || diagnostic.Code != collectionquery.CodeUnsupported {
		t.Fatalf("case-fold Sort error = %v", err)
	}
}

func TestReferenceSortUsesExplicitNullPlacementUnicodeAndStableTieBreaker(t *testing.T) {
	t.Parallel()
	query := queryContract()
	rows := []collectionquery.Row{
		row("3", "name", nil, "age", int64(7)),
		row("2", "name", "\ue000", "age", int64(7)),
		row("4", "age", int64(7)),
		row("1", "name", "😀", "age", int64(7)),
		row("0", "name", "😀", "age", int64(7)),
	}
	sorted, err := collectionquery.Sort(queryTypes(t), query, []schema.CollectionSortTerm{{Field: "User.name", Direction: schema.SortAscending, Nulls: schema.NullsLast}}, rows)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(sorted))
	for index := range sorted {
		got[index] = sorted[index].TieBreaker.(string)
	}
	if want := []string{"2", "0", "1", "3", "4"}; !slices.Equal(got, want) {
		t.Fatalf("sort order = %v, want %v", got, want)
	}
}

func TestReferenceSortRejectsInvalidAndDuplicatePositions(t *testing.T) {
	t.Parallel()
	types := queryTypes(t)
	query := queryContract()
	terms := []schema.CollectionSortTerm{{Field: "User.name", Direction: schema.SortAscending, Nulls: schema.NullsLast}}
	tests := []struct {
		name string
		rows []collectionquery.Row
	}{
		{name: "duplicate tie breaker", rows: []collectionquery.Row{row("same", "name", "Ada"), row("same", "name", "Bea")}},
		{name: "wrong tie breaker type", rows: []collectionquery.Row{{Value: map[string]any{"name": "Ada"}, TieBreaker: int64(1)}}},
		{name: "incomparable tie breaker", rows: []collectionquery.Row{{Value: map[string]any{"name": "Ada"}, TieBreaker: []string{"bad"}}}},
		{name: "wrong sort value type", rows: []collectionquery.Row{row("one", "name", int64(1))}},
		{name: "incomparable sort value", rows: []collectionquery.Row{{Value: map[string]any{"name": []string{"bad"}}, TieBreaker: "one"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := collectionquery.Sort(types, query, terms, test.rows); err == nil {
				t.Fatal("Sort succeeded, want invalid position error")
			}
		})
	}
	strictTerms := []schema.CollectionSortTerm{{Field: "User.age", Direction: schema.SortAscending, Nulls: schema.NullsLast}}
	for _, test := range []struct {
		name string
		row  collectionquery.Row
	}{
		{name: "undeclared missing", row: row("one")},
		{name: "undeclared null", row: row("one", "age", nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := collectionquery.Sort(types, query, strictTerms, []collectionquery.Row{test.row}); err == nil {
				t.Fatal("Sort accepted a row outside the declared missing/null contract")
			}
		})
	}
}

func queryContract() schema.CollectionQueryDescriptor {
	return schema.CollectionQueryDescriptor{
		Capability: schema.CollectionQueryCapability,
		Fields: []schema.CollectionQueryFieldDescriptor{
			{ID: "User.age", Path: []string{"age"}, Type: schema.TypeID(schema.Int64), Operators: []schema.FilterOperator{schema.FilterEqual, schema.FilterNotEqual, schema.FilterLess, schema.FilterLessEqual, schema.FilterGreater, schema.FilterGreaterEqual, schema.FilterIn, schema.FilterNotIn}, Cost: 1, Indexed: true, Sort: &schema.CollectionSortDescriptor{Directions: []schema.SortDirection{schema.SortAscending, schema.SortDescending}, Nulls: []schema.NullPlacement{schema.NullsFirst, schema.NullsLast}, Collation: "numeric", CaseSensitivity: "sensitive"}},
			{ID: "User.name", Path: []string{"name"}, Type: schema.TypeID(schema.String), Nullable: true, Missing: true, Operators: []schema.FilterOperator{schema.FilterEqual, schema.FilterNotEqual, schema.FilterIsNull, schema.FilterIsNotNull, schema.FilterExists, schema.FilterNotExists, schema.FilterIn, schema.FilterNotIn}, Cost: 1, Indexed: true, Sort: &schema.CollectionSortDescriptor{Directions: []schema.SortDirection{schema.SortAscending, schema.SortDescending}, Nulls: []schema.NullPlacement{schema.NullsFirst, schema.NullsLast}, Collation: "unicode-code-point", CaseSensitivity: "sensitive"}},
			{ID: "User.tags", Path: []string{"tags"}, Type: "Tags", ElementType: schema.TypeID(schema.String), Nullable: true, Missing: true, Operators: []schema.FilterOperator{schema.FilterAny, schema.FilterAll}, Cost: 2, Indexed: true},
		},
		TieBreaker: schema.CollectionTieBreakerDescriptor{Type: schema.TypeID(schema.ID), Collation: "unicode-code-point", CaseSensitivity: "sensitive"},
		Limits:     schema.CollectionQueryLimits{MaxDepth: 4, MaxPredicates: 8, MaxMembership: 4, MaxSortKeys: 2, MaxCost: 8},
	}
}

func predicate(field string, operator schema.FilterOperator, kind schema.ScalarKind, literal string) schema.CollectionFilterExpression {
	return typedPredicate(field, operator, schema.TypeID(kind), literal)
}

func typedPredicate(field string, operator schema.FilterOperator, kind schema.TypeID, literal string) schema.CollectionFilterExpression {
	return schema.CollectionFilterExpression{Kind: schema.CollectionFilterPredicate, Field: field, Operator: operator, Value: &schema.CollectionFilterValue{Type: kind, Literal: json.RawMessage(literal)}}
}

func variablePredicate(field string, operator schema.FilterOperator, kind schema.TypeID, variable string) schema.CollectionFilterExpression {
	return schema.CollectionFilterExpression{Kind: schema.CollectionFilterPredicate, Field: field, Operator: operator, Value: &schema.CollectionFilterValue{Type: kind, Variable: variable}}
}

func queryTypes(t testing.TB) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	for _, descriptor := range []schema.TypeDescriptor{
		{ID: "Status", Kind: schema.EnumType, Input: true, Output: true, EnumValues: []string{"ACTIVE", "DISABLED"}},
		{ID: "Tags", Kind: schema.ListType, Output: true, Element: schema.TypeID(schema.String)},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	types, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	return types
}

func unaryPredicate(field string, operator schema.FilterOperator) schema.CollectionFilterExpression {
	return schema.CollectionFilterExpression{Kind: schema.CollectionFilterPredicate, Field: field, Operator: operator}
}

func ptrFilter(value schema.CollectionFilterExpression) *schema.CollectionFilterExpression {
	return &value
}

func nestedNot(value schema.CollectionFilterExpression, count int) schema.CollectionFilterExpression {
	for range count {
		value = schema.CollectionFilterExpression{Kind: schema.CollectionFilterNot, Child: ptrFilter(value)}
	}
	return value
}

func row(tieBreaker string, values ...any) collectionquery.Row {
	fields := make(map[string]any, len(values)/2)
	for index := 0; index < len(values); index += 2 {
		fields[values[index].(string)] = values[index+1]
	}
	return collectionquery.Row{Value: fields, TieBreaker: tieBreaker}
}
