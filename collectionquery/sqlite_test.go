package collectionquery_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/collectionquery"
	"github.com/valksor/naatre/schema"
	_ "modernc.org/sqlite"
)

func TestSQLiteTranslatorKeepsSQLLikeLiteralsInBoundArguments(t *testing.T) {
	t.Parallel()
	translator := sqliteTranslator()
	literal := `Robert'); DROP TABLE users;--`
	filter := predicate("User.name", schema.FilterEqual, schema.String, `"Robert'); DROP TABLE users;--"`)
	fragment, err := translator.Translate(queryTypes(t), queryContract(), filter, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fragment.Where, literal) || len(fragment.Arguments) != 1 || fragment.Arguments[0] != literal {
		t.Fatalf("translation = %q %#v", fragment.Where, fragment.Arguments)
	}
	if !strings.Contains(fragment.Where, "?") {
		t.Fatalf("translation has no parameter placeholder: %s", fragment.Where)
	}
}

func TestSQLiteProviderRejectsUnavailableFieldsBeforeBackendInvocation(t *testing.T) {
	t.Parallel()
	backend := &countingQueryer{}
	provider := collectionquery.SQLiteProvider{BaseQuery: `SELECT id FROM users`, Translator: sqliteTranslator()}
	_, err := provider.Select(context.Background(), backend, queryTypes(t), queryContract(), predicate("User.salary", schema.FilterEqual, schema.Int64, `"1"`), nil, nil)
	var diagnostic *collectionquery.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != collectionquery.CodeUnsupported || backend.calls != 0 {
		t.Fatalf("Select error=%v calls=%d", err, backend.calls)
	}
	if strings.Contains(strings.ToLower(err.Error()), "salary") {
		t.Fatalf("safe provider error leaked hidden field: %v", err)
	}
}

func TestSQLiteProviderWrapsBackendErrorsWithoutExposingDetails(t *testing.T) {
	t.Parallel()
	cause := errors.New("driver leaked users.secret and SELECT text")
	backend := &countingQueryer{err: cause}
	provider := collectionquery.SQLiteProvider{BaseQuery: `SELECT id FROM users`, Translator: sqliteTranslator()}
	_, err := provider.Select(context.Background(), backend, queryTypes(t), queryContract(), predicate("User.name", schema.FilterEqual, schema.String, `"Ada"`), nil, nil)
	var diagnostic *collectionquery.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != collectionquery.CodeProviderFailed || !errors.Is(err, cause) || backend.calls != 1 {
		t.Fatalf("Select error=%v calls=%d", err, backend.calls)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "SELECT") {
		t.Fatalf("provider error exposed backend details: %v", err)
	}
}

func TestSQLiteTranslatorRejectsInexactExtendedNumericTypes(t *testing.T) {
	t.Parallel()
	query := queryContract()
	query.Fields[0].Type = schema.TypeID(schema.UInt64)
	_, err := sqliteTranslator().Translate(queryTypes(t), query, predicate("User.age", schema.FilterEqual, schema.UInt64, `"18446744073709551615"`), nil, nil)
	var diagnostic *collectionquery.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != collectionquery.CodeUnsupported {
		t.Fatalf("Translate error = %v, want %s", err, collectionquery.CodeUnsupported)
	}
}

func TestSQLiteProviderMatchesReferenceForNullMissingListsUnicodeAndNumericBounds(t *testing.T) {
	t.Parallel()
	db, records := sqliteParityDatabase(t)
	translator := sqliteTranslator()
	tests := []struct {
		name   string
		filter schema.CollectionFilterExpression
	}{
		{name: "missing versus null", filter: unaryPredicate("User.name", schema.FilterIsNull)},
		{name: "unicode equality", filter: predicate("User.name", schema.FilterEqual, schema.String, `"😀"`)},
		{name: "signed boundary", filter: predicate("User.age", schema.FilterGreaterEqual, schema.Int64, `"9223372036854775806"`)},
		{name: "empty membership", filter: predicate("User.age", schema.FilterIn, schema.Int64, `[]`)},
		{name: "list any", filter: schema.CollectionFilterExpression{Kind: schema.CollectionFilterAny, Field: "User.tags", Predicate: ptrFilter(predicate("@", schema.FilterEqual, schema.String, `"vip"`))}},
		{name: "empty list all", filter: schema.CollectionFilterExpression{Kind: schema.CollectionFilterAll, Field: "User.tags", Predicate: ptrFilter(predicate("@", schema.FilterEqual, schema.String, `"vip"`))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := referenceIDs(t, queryTypes(t), queryContract(), test.filter, records)
			fragment, err := translator.Translate(queryTypes(t), queryContract(), test.filter, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := queryIDs(t, db, `SELECT id FROM users WHERE `+fragment.Where+` ORDER BY id`, fragment.Arguments)
			if !slices.Equal(got, want) {
				t.Fatalf("provider=%v reference=%v where=%s args=%#v", got, want, fragment.Where, fragment.Arguments)
			}
		})
	}
}

func TestSQLiteSortMatchesReferenceStableOrdering(t *testing.T) {
	t.Parallel()
	db, records := sqliteParityDatabase(t)
	terms := []schema.CollectionSortTerm{{Field: "User.name", Direction: schema.SortAscending, Nulls: schema.NullsLast}}
	reference, err := collectionquery.Sort(queryTypes(t), queryContract(), terms, records)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, len(reference))
	for index := range reference {
		want[index] = reference[index].TieBreaker.(string)
	}
	fragment, err := sqliteTranslator().TranslateSort(queryTypes(t), queryContract(), terms)
	if err != nil {
		t.Fatal(err)
	}
	got := queryIDs(t, db, `SELECT id FROM users `+fragment, nil)
	if !slices.Equal(got, want) {
		t.Fatalf("provider sort=%v reference=%v order=%s", got, want, fragment)
	}
}

type countingQueryer struct {
	calls int
	err   error
}

func (q *countingQueryer) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.calls++
	if q.err != nil {
		return nil, q.err
	}
	return nil, errors.New("unexpected backend invocation")
}

func sqliteTranslator() collectionquery.SQLiteTranslator {
	return collectionquery.SQLiteTranslator{
		Fields: map[string]collectionquery.SQLiteField{
			"User.age":  {ValueColumn: "age", PresenceColumn: "age_present"},
			"User.name": {ValueColumn: "name", PresenceColumn: "name_present"},
			"User.tags": {ValueColumn: "tags", PresenceColumn: "tags_present", JSONList: true},
		},
		TieBreakerColumn: "id",
	}
}

func sqliteParityDatabase(t testing.TB) (*sql.DB, []collectionquery.Row) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "-"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`, `PRAGMA foreign_keys=ON`,
		`CREATE TABLE users (id TEXT PRIMARY KEY, age INTEGER, age_present INTEGER NOT NULL, name TEXT, name_present INTEGER NOT NULL, tags TEXT, tags_present INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("setup %q: %v", statement, err)
		}
	}
	records := []collectionquery.Row{
		row("a", "age", int64(-9223372036854775808), "name", "\ue000", "tags", []any{}),
		row("b", "age", int64(9223372036854775807), "name", "😀", "tags", []any{"vip"}),
		row("c", "age", int64(9223372036854775806), "name", nil, "tags", []any{"new", "vip"}),
		row("d", "age", int64(7), "tags", []any{}),
	}
	for _, record := range records {
		name, namePresent := record.Value["name"]
		tags, tagsPresent := record.Value["tags"]
		tagsJSON, _ := json.Marshal(tags)
		if _, err := db.Exec(`INSERT INTO users(id,age,age_present,name,name_present,tags,tags_present) VALUES(?,?,?,?,?,?,?)`,
			record.TieBreaker, record.Value["age"], 1, name, boolInt(namePresent), string(tagsJSON), boolInt(tagsPresent)); err != nil {
			t.Fatal(err)
		}
	}
	return db, records
}

func referenceIDs(t testing.TB, types schema.Snapshot, query schema.CollectionQueryDescriptor, filter schema.CollectionFilterExpression, records []collectionquery.Row) []string {
	t.Helper()
	result := make([]string, 0, len(records))
	for _, record := range records {
		matched, err := collectionquery.Evaluate(types, query, filter, record, nil)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			result = append(result, record.TieBreaker.(string))
		}
	}
	slices.Sort(result)
	return result
}

func queryIDs(t testing.TB, db *sql.DB, statement string, arguments []any) []string {
	t.Helper()
	rows, err := db.Query(statement, arguments...)
	if err != nil {
		t.Fatalf("query %q: %v", statement, err)
	}
	defer func() { _ = rows.Close() }()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
