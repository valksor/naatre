package collectionquery

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"

	"github.com/valksor/naatre/schema"
)

type Row struct {
	Value      map[string]any
	TieBreaker any
}

func Evaluate(types schema.Snapshot, query schema.CollectionQueryDescriptor, expression schema.CollectionFilterExpression, row Row, variables Variables) (bool, error) {
	prepared, err := Prepare(types, query, expression, variables)
	if err == nil {
		return prepared.Match(row)
	}
	return false, err
}

func (p *PreparedFilter) Match(row Row) (bool, error) {
	if p == nil {
		return false, invalid("prepared filter is missing")
	}
	return evaluateExpression(p.root, row.Value, nil), nil
}

func evaluateExpression(expression preparedExpression, root map[string]any, current any) bool {
	switch expression.kind {
	case schema.CollectionFilterAnd:
		for _, child := range expression.children {
			if !evaluateExpression(child, root, current) {
				return false
			}
		}
		return true
	case schema.CollectionFilterOr:
		for _, child := range expression.children {
			if evaluateExpression(child, root, current) {
				return true
			}
		}
		return false
	case schema.CollectionFilterNot:
		return !evaluateExpression(expression.children[0], root, current)
	case schema.CollectionFilterPredicate:
		return evaluatePredicate(expression, root, current)
	case schema.CollectionFilterAny, schema.CollectionFilterAll:
		return evaluateList(expression, root)
	default:
		return false
	}
}

func evaluatePredicate(expression preparedExpression, root map[string]any, current any) bool {
	value, present := current, true
	if !expression.current {
		value, present = lookupPath(root, expression.field.Path)
	}
	if expression.operator == schema.FilterExists {
		return present
	}
	if expression.operator == schema.FilterNotExists {
		return !present
	}
	if expression.operator == schema.FilterIsNull {
		return present && value == nil
	}
	if expression.operator == schema.FilterIsNotNull {
		return present && value != nil
	}
	if !present || value == nil || expression.value == nil {
		return false
	}
	if expression.operator == schema.FilterIn || expression.operator == schema.FilterNotIn {
		matched := false
		for _, candidate := range expression.value.([]any) {
			if compared, ok := compareValues(value, candidate); ok && compared == 0 {
				matched = true
				break
			}
		}
		if expression.operator == schema.FilterNotIn {
			return !matched
		}
		return matched
	}
	compared, ok := compareValues(value, expression.value)
	if !ok {
		return false
	}
	return comparisonResult(expression.operator, compared)
}

func evaluateList(expression preparedExpression, root map[string]any) bool {
	value, present := lookupPath(root, expression.field.Path)
	if !present || value == nil {
		return false
	}
	items, ok := asSlice(value)
	if !ok {
		return false
	}
	if expression.kind == schema.CollectionFilterAny {
		for _, item := range items {
			if evaluateExpression(*expression.predicate, root, item) {
				return true
			}
		}
		return false
	}
	for _, item := range items {
		if !evaluateExpression(*expression.predicate, root, item) {
			return false
		}
	}
	return true
}

func lookupPath(root map[string]any, path []string) (any, bool) {
	var current any = root
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func asSlice(value any) ([]any, bool) {
	if direct, ok := value.([]any); ok {
		return direct, true
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Slice {
		return nil, false
	}
	result := make([]any, reflected.Len())
	for index := range result {
		result[index] = reflected.Index(index).Interface()
	}
	return result, true
}

func compareValues(left, right any) (int, bool) {
	if leftNumber, ok := numberValue(left); ok {
		rightNumber, rightOK := numberValue(right)
		if !rightOK {
			return 0, false
		}
		return leftNumber.Cmp(rightNumber), true
	}
	switch typed := left.(type) {
	case string:
		other, ok := right.(string)
		return stringsCompare(typed, other), ok
	case bool:
		other, ok := right.(bool)
		if !ok || typed == other {
			return 0, ok
		}
		if !typed {
			return -1, true
		}
		return 1, true
	default:
		return 0, false
	}
}

func numberValue(value any) (*big.Rat, bool) {
	var text string
	switch typed := value.(type) {
	case numericValue:
		text = string(typed)
	case json.Number:
		text = typed.String()
	case int:
		text = strconv.FormatInt(int64(typed), 10)
	case int32:
		text = strconv.FormatInt(int64(typed), 10)
	case int64:
		text = strconv.FormatInt(typed, 10)
	case uint:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint32:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint64:
		text = strconv.FormatUint(typed, 10)
	case float32:
		text = strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		text = strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return nil, false
	}
	result, ok := new(big.Rat).SetString(text)
	return result, ok
}

func comparisonResult(operator schema.FilterOperator, compared int) bool {
	if operator == schema.FilterEqual {
		return compared == 0
	}
	if operator == schema.FilterNotEqual {
		return compared != 0
	}
	if operator == schema.FilterLess {
		return compared < 0
	}
	if operator == schema.FilterLessEqual {
		return compared <= 0
	}
	if operator == schema.FilterGreater {
		return compared > 0
	}
	if operator == schema.FilterGreaterEqual {
		return compared >= 0
	}
	return false
}

func stringsCompare(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
