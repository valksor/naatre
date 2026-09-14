package collectionquery

import (
	"encoding/json"
	"slices"
	"strconv"

	"github.com/valksor/naatre/schema"
)

type preparedSortTerm struct {
	field     schema.CollectionQueryFieldDescriptor
	direction schema.SortDirection
	nulls     schema.NullPlacement
}

func Sort(types schema.Snapshot, query schema.CollectionQueryDescriptor, terms []schema.CollectionSortTerm, rows []Row) ([]Row, error) {
	prepared, err := prepareSort(types, query, terms)
	if err != nil {
		return nil, err
	}
	if err := validateSortRows(types, query, prepared, rows); err != nil {
		return nil, err
	}
	result := slices.Clone(rows)
	valid := true
	slices.SortStableFunc(result, func(left, right Row) int {
		for _, term := range prepared {
			compared, ok := compareSortTerm(term, left, right)
			if !ok {
				valid = false
				return 0
			}
			if compared != 0 {
				return compared
			}
		}
		compared, ok := compareValues(left.TieBreaker, right.TieBreaker)
		if !ok {
			valid = false
			return 0
		}
		return compared
	})
	if !valid {
		return nil, invalid("collection sort values are incompatible")
	}
	return result, nil
}

func prepareSort(types schema.Snapshot, query schema.CollectionQueryDescriptor, terms []schema.CollectionSortTerm) ([]preparedSortTerm, error) {
	if err := schema.ValidateCollectionQueryDescriptor(&query); err != nil {
		return nil, invalid("collection query contract is invalid")
	}
	if err := schema.ValidateCollectionQueryTypes(&query, types); err != nil {
		return nil, invalid("collection query contract has incompatible types")
	}
	if query.TieBreaker.CaseSensitivity != "sensitive" {
		return nil, unsupported("stable sort case policy is not available")
	}
	if len(terms) == 0 || uint64(len(terms)) > query.Limits.MaxSortKeys {
		return nil, exhausted("sort key limit exceeded")
	}
	fields := make(map[string]schema.CollectionQueryFieldDescriptor, len(query.Fields))
	for _, field := range query.Fields {
		fields[field.ID] = field
	}
	seen := make(map[string]bool, len(terms))
	result := make([]preparedSortTerm, len(terms))
	for index, term := range terms {
		field, exists := fields[term.Field]
		if !exists || field.Sort == nil || seen[term.Field] {
			return nil, unsupported("sort field is not available")
		}
		if !slices.Contains(field.Sort.Directions, term.Direction) || !slices.Contains(field.Sort.Nulls, term.Nulls) {
			return nil, unsupported("sort option is not available")
		}
		if field.Sort.CaseSensitivity != "sensitive" {
			return nil, unsupported("sort case policy is not available")
		}
		seen[term.Field] = true
		result[index] = preparedSortTerm{field: field, direction: term.Direction, nulls: term.Nulls}
	}
	return result, nil
}

func compareSortTerm(term preparedSortTerm, left, right Row) (int, bool) {
	leftValue, leftPresent := lookupPath(left.Value, term.field.Path)
	rightValue, rightPresent := lookupPath(right.Value, term.field.Path)
	leftNullish := !leftPresent || leftValue == nil
	rightNullish := !rightPresent || rightValue == nil
	if leftNullish || rightNullish {
		return compareNullish(term.nulls, leftNullish, rightNullish), true
	}
	compared, ok := compareValues(leftValue, rightValue)
	if !ok {
		return 0, false
	}
	if term.direction == schema.SortDescending {
		return -compared, true
	}
	return compared, true
}

func validateSortRows(types schema.Snapshot, query schema.CollectionQueryDescriptor, terms []preparedSortTerm, rows []Row) error {
	tieBreakers := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !valueMatchesSchemaType(types, query.TieBreaker.Type, row.TieBreaker) {
			return invalid("collection tie breaker value does not match its declared type")
		}
		key, ok := sortableValueKey(row.TieBreaker)
		if !ok {
			return invalid("collection tie breaker value is invalid")
		}
		if tieBreakers[key] {
			return invalid("collection tie breaker values are not unique")
		}
		tieBreakers[key] = true
		for _, term := range terms {
			value, present := lookupPath(row.Value, term.field.Path)
			if !present {
				if !term.field.Missing {
					return invalid("collection sort row is missing a required field")
				}
				continue
			}
			if value == nil {
				if !term.field.Nullable {
					return invalid("collection sort row has null for a non-null field")
				}
				continue
			}
			if !valueMatchesSchemaType(types, term.field.Type, value) {
				return invalid("collection sort value does not match its declared type")
			}
			if _, ok := sortableValueKey(value); !ok {
				return invalid("collection sort value is invalid")
			}
		}
	}
	return nil
}

func valueMatchesSchemaType(types schema.Snapshot, typeID schema.TypeID, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if slices.Contains([]schema.ScalarKind{schema.Int64, schema.UInt64, schema.BigInt, schema.Decimal, schema.Duration}, schema.ScalarKind(typeID)) {
		text, ok := extendedNumericText(value)
		if !ok {
			return false
		}
		raw, err = json.Marshal(text)
		if err != nil {
			return false
		}
	}
	_, err = schema.CoerceRuntimeInput(types, typeID, raw, false)
	return err == nil
}

func extendedNumericText(value any) (string, bool) {
	switch typed := value.(type) {
	case numericValue:
		return string(typed), true
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case int:
		return strconv.FormatInt(int64(typed), 10), true
	case int32:
		return strconv.FormatInt(int64(typed), 10), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case uint:
		return strconv.FormatUint(uint64(typed), 10), true
	case uint32:
		return strconv.FormatUint(uint64(typed), 10), true
	case uint64:
		return strconv.FormatUint(typed, 10), true
	default:
		return "", false
	}
}

func sortableValueKey(value any) (string, bool) {
	if number, ok := numberValue(value); ok {
		return "n:" + number.RatString(), true
	}
	switch typed := value.(type) {
	case string:
		return "s:" + typed, true
	case bool:
		if typed {
			return "b:1", true
		}
		return "b:0", true
	default:
		return "", false
	}
}

func compareNullish(placement schema.NullPlacement, left, right bool) int {
	if left == right {
		return 0
	}
	if placement == schema.NullsFirst {
		if left {
			return -1
		}
		return 1
	}
	if left {
		return 1
	}
	return -1
}
