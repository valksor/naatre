package collectionquery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/valksor/naatre/schema"
)

type SQLiteField struct {
	ValueColumn    string
	PresenceColumn string
	JSONList       bool
}

type SQLiteTranslator struct {
	Fields           map[string]SQLiteField
	TieBreakerColumn string
}

type SQLFragment struct {
	Where     string
	Arguments []any
}

type sqliteBuilder struct {
	translator SQLiteTranslator
	arguments  []any
	aliases    uint64
}

func (t SQLiteTranslator) Translate(types schema.Snapshot, query schema.CollectionQueryDescriptor, filter schema.CollectionFilterExpression, sorts []schema.CollectionSortTerm, variables Variables) (SQLFragment, error) {
	prepared, err := Prepare(types, query, filter, variables)
	if err != nil {
		return SQLFragment{}, err
	}
	if !sqliteFilterTypesSupported(types, prepared.root) {
		return SQLFragment{}, unsupported("filter value type is not available for SQLite translation")
	}
	builder := sqliteBuilder{translator: t}
	where, err := builder.expression(prepared.root, "")
	if err != nil {
		return SQLFragment{}, err
	}
	if len(sorts) != 0 {
		if _, err := t.TranslateSort(types, query, sorts); err != nil {
			return SQLFragment{}, err
		}
	}
	return SQLFragment{Where: where, Arguments: builder.arguments}, nil
}

func (t SQLiteTranslator) TranslateSort(types schema.Snapshot, query schema.CollectionQueryDescriptor, terms []schema.CollectionSortTerm) (string, error) {
	prepared, err := prepareSort(types, query, terms)
	if err != nil {
		return "", err
	}
	if !sqliteScalarTypeSupported(types, query.TieBreaker.Type) {
		return "", unsupported("stable sort type is not available for SQLite translation")
	}
	if t.TieBreakerColumn == "" {
		return "", unsupported("stable sort mapping is not available")
	}
	if query.TieBreaker.CaseSensitivity != "sensitive" {
		return "", unsupported("stable sort case policy is not available")
	}
	parts := make([]string, 0, len(prepared)*2+1)
	for _, term := range prepared {
		if !sqliteScalarTypeSupported(types, term.field.Type) {
			return "", unsupported("sort value type is not available for SQLite translation")
		}
		mapping, exists := t.Fields[term.field.ID]
		if !exists || mapping.ValueColumn == "" || (term.field.Missing && mapping.PresenceColumn == "") {
			return "", unsupported("sort field mapping is not available")
		}
		value := quoteSQLiteIdentifier(mapping.ValueColumn)
		nullish := value + " IS NULL"
		if mapping.PresenceColumn != "" {
			nullish = quoteSQLiteIdentifier(mapping.PresenceColumn) + " = 0 OR " + nullish
		}
		nullRank := "1"
		if term.nulls == schema.NullsFirst {
			nullRank = "0"
		}
		otherRank := "0"
		if nullRank == "0" {
			otherRank = "1"
		}
		parts = append(parts, "CASE WHEN "+nullish+" THEN "+nullRank+" ELSE "+otherRank+" END ASC")
		direction := "ASC"
		if term.direction == schema.SortDescending {
			direction = "DESC"
		}
		collation := ""
		if term.field.Sort.Collation != "numeric" {
			collation = " COLLATE BINARY"
		}
		parts = append(parts, value+collation+" "+direction)
	}
	parts = append(parts, quoteSQLiteIdentifier(t.TieBreakerColumn)+" COLLATE BINARY ASC")
	return "ORDER BY " + strings.Join(parts, ", "), nil
}

func sqliteFilterTypesSupported(types schema.Snapshot, expression preparedExpression) bool {
	if expression.kind == schema.CollectionFilterPredicate && expression.value != nil {
		return sqliteScalarTypeSupported(types, expression.field.Type)
	}
	for _, child := range expression.children {
		if !sqliteFilterTypesSupported(types, child) {
			return false
		}
	}
	return expression.predicate == nil || sqliteFilterTypesSupported(types, *expression.predicate)
}

func sqliteScalarTypeSupported(types schema.Snapshot, typeID schema.TypeID) bool {
	descriptor, exists := types.Lookup(typeID)
	if !exists {
		return false
	}
	if descriptor.Kind == schema.EnumType {
		return true
	}
	if descriptor.Kind != schema.ScalarType {
		return false
	}
	return slices.Contains([]schema.TypeID{
		schema.TypeID(schema.Boolean), schema.TypeID(schema.String), schema.TypeID(schema.ID),
		schema.TypeID(schema.Int32), schema.TypeID(schema.Float64), schema.TypeID(schema.Int64),
		schema.TypeID(schema.Timestamp), schema.TypeID(schema.Duration), schema.TypeID(schema.UUID), schema.TypeID(schema.Bytes),
	}, typeID)
}

func (b *sqliteBuilder) expression(expression preparedExpression, current string) (string, error) {
	switch expression.kind {
	case schema.CollectionFilterAnd, schema.CollectionFilterOr:
		return b.boolean(expression, current)
	case schema.CollectionFilterNot:
		child, err := b.expression(expression.children[0], current)
		if err != nil {
			return "", err
		}
		return "(NOT (" + child + "))", nil
	case schema.CollectionFilterPredicate:
		return b.predicate(expression, current)
	case schema.CollectionFilterAny, schema.CollectionFilterAll:
		return b.list(expression)
	default:
		return "", invalid("filter expression kind is invalid")
	}
}

func (b *sqliteBuilder) boolean(expression preparedExpression, current string) (string, error) {
	parts := make([]string, len(expression.children))
	for index := range expression.children {
		part, err := b.expression(expression.children[index], current)
		if err != nil {
			return "", err
		}
		parts[index] = "(" + part + ")"
	}
	joiner := " AND "
	if expression.kind == schema.CollectionFilterOr {
		joiner = " OR "
	}
	return "(" + strings.Join(parts, joiner) + ")", nil
}

func (b *sqliteBuilder) predicate(expression preparedExpression, current string) (string, error) {
	value, present, err := b.fieldExpressions(expression, current)
	if err != nil {
		return "", err
	}
	if expression.operator == schema.FilterExists {
		return present, nil
	}
	if expression.operator == schema.FilterNotExists {
		return "NOT (" + present + ")", nil
	}
	if expression.operator == schema.FilterIsNull {
		return "(" + present + " AND " + value + " IS NULL)", nil
	}
	if expression.operator == schema.FilterIsNotNull {
		return "(" + present + " AND " + value + " IS NOT NULL)", nil
	}
	if expression.operator == schema.FilterIn || expression.operator == schema.FilterNotIn {
		return b.membership(expression, value, present)
	}
	operator := mapSQLiteOperator(expression.operator)
	if operator == "" {
		return "", unsupported("filter operator is not available")
	}
	b.arguments = append(b.arguments, sqliteArgument(expression.value))
	return "(" + present + " AND " + value + " IS NOT NULL AND " + value + " " + operator + " ?)", nil
}

func (b *sqliteBuilder) fieldExpressions(expression preparedExpression, current string) (string, string, error) {
	if expression.current {
		if current == "" {
			return "", "", invalid("nested filter scope is invalid")
		}
		return current, "1", nil
	}
	mapping, exists := b.translator.Fields[expression.field.ID]
	if !exists || mapping.ValueColumn == "" || (expression.field.Missing && mapping.PresenceColumn == "") {
		return "", "", unsupported("filter field mapping is not available")
	}
	value := quoteSQLiteIdentifier(mapping.ValueColumn)
	present := "1"
	if mapping.PresenceColumn != "" {
		present = quoteSQLiteIdentifier(mapping.PresenceColumn) + " <> 0"
	}
	return value, present, nil
}

func (b *sqliteBuilder) membership(expression preparedExpression, value, present string) (string, error) {
	values := expression.value.([]any)
	matched := "0"
	if len(values) != 0 {
		placeholders := make([]string, len(values))
		for index := range values {
			placeholders[index] = "?"
			b.arguments = append(b.arguments, sqliteArgument(values[index]))
		}
		matched = value + " IN (" + strings.Join(placeholders, ",") + ")"
	}
	if expression.operator == schema.FilterNotIn {
		matched = "NOT (" + matched + ")"
	}
	return "(" + present + " AND " + value + " IS NOT NULL AND " + matched + ")", nil
}

func (b *sqliteBuilder) list(expression preparedExpression) (string, error) {
	mapping, exists := b.translator.Fields[expression.field.ID]
	if !exists || !mapping.JSONList || mapping.ValueColumn == "" || (expression.field.Missing && mapping.PresenceColumn == "") {
		return "", unsupported("list filter mapping is not available")
	}
	b.aliases++
	alias := fmt.Sprintf("cq%d", b.aliases)
	predicate, err := b.expression(*expression.predicate, quoteSQLiteIdentifier(alias)+".value")
	if err != nil {
		return "", err
	}
	value := quoteSQLiteIdentifier(mapping.ValueColumn)
	present := "1"
	if mapping.PresenceColumn != "" {
		present = quoteSQLiteIdentifier(mapping.PresenceColumn) + " <> 0"
	}
	subquery := "SELECT 1 FROM json_each(" + value + ") AS " + quoteSQLiteIdentifier(alias)
	if expression.kind == schema.CollectionFilterAny {
		subquery += " WHERE " + predicate
		return "(" + present + " AND " + value + " IS NOT NULL AND EXISTS (" + subquery + "))", nil
	}
	subquery += " WHERE NOT (" + predicate + ")"
	return "(" + present + " AND " + value + " IS NOT NULL AND NOT EXISTS (" + subquery + "))", nil
}

func mapSQLiteOperator(operator schema.FilterOperator) string {
	switch operator {
	case schema.FilterEqual:
		return "="
	case schema.FilterNotEqual:
		return "<>"
	case schema.FilterLess:
		return "<"
	case schema.FilterLessEqual:
		return "<="
	case schema.FilterGreater:
		return ">"
	case schema.FilterGreaterEqual:
		return ">="
	case schema.FilterIn, schema.FilterNotIn, schema.FilterIsNull, schema.FilterIsNotNull,
		schema.FilterExists, schema.FilterNotExists, schema.FilterAny, schema.FilterAll,
		schema.FilterRegex, schema.FilterFullText, schema.FilterGeoWithin:
		return ""
	default:
		return ""
	}
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func sqliteArgument(value any) any {
	if number, ok := value.(numericValue); ok {
		if integer, err := strconv.ParseInt(string(number), 10, 64); err == nil {
			return integer
		}
		return string(number)
	}
	number, ok := value.(json.Number)
	if !ok {
		return value
	}
	if integer, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
		return integer
	}
	if decimal, err := strconv.ParseFloat(number.String(), 64); err == nil {
		return decimal
	}
	return number.String()
}

type SQLiteQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type SQLiteProvider struct {
	BaseQuery  string
	Translator SQLiteTranslator
}

func (p SQLiteProvider) Select(ctx context.Context, backend SQLiteQueryer, types schema.Snapshot, query schema.CollectionQueryDescriptor, filter schema.CollectionFilterExpression, sorts []schema.CollectionSortTerm, variables Variables) (*sql.Rows, error) {
	if backend == nil || strings.TrimSpace(p.BaseQuery) == "" {
		return nil, invalid("collection provider configuration is invalid")
	}
	fragment, err := p.Translator.Translate(types, query, filter, sorts, variables)
	if err != nil {
		return nil, err
	}
	statement := p.BaseQuery + " WHERE " + fragment.Where
	if len(sorts) != 0 {
		order, sortErr := p.Translator.TranslateSort(types, query, sorts)
		if sortErr != nil {
			return nil, sortErr
		}
		statement += " " + order
	}
	rows, err := backend.QueryContext(ctx, statement, fragment.Arguments...)
	if err != nil {
		return nil, providerFailed(err)
	}
	return rows, nil
}
