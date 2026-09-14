package collectionquery

import (
	"encoding/json"
	"slices"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

type Variables map[string]json.RawMessage

type PreparedFilter struct {
	root preparedExpression
}

type preparedExpression struct {
	kind      schema.CollectionFilterKind
	children  []preparedExpression
	field     *schema.CollectionQueryFieldDescriptor
	operator  schema.FilterOperator
	value     any
	predicate *preparedExpression
	current   bool
}

type numericValue string

type prepareState struct {
	types      schema.Snapshot
	query      schema.CollectionQueryDescriptor
	fields     map[string]*schema.CollectionQueryFieldDescriptor
	variables  Variables
	predicates uint64
	cost       uint64
}

func Prepare(types schema.Snapshot, query schema.CollectionQueryDescriptor, expression schema.CollectionFilterExpression, variables Variables) (*PreparedFilter, error) {
	if err := schema.ValidateCollectionQueryDescriptor(&query); err != nil {
		return nil, invalid("collection query contract is invalid")
	}
	if err := schema.ValidateCollectionQueryTypes(&query, types); err != nil {
		return nil, invalid("collection query contract has incompatible types")
	}
	state := prepareState{types: types, query: query, fields: make(map[string]*schema.CollectionQueryFieldDescriptor, len(query.Fields)), variables: variables}
	for index := range query.Fields {
		state.fields[query.Fields[index].ID] = &query.Fields[index]
	}
	root, err := state.expression(expression, 1, "")
	if err != nil {
		return nil, err
	}
	return &PreparedFilter{root: root}, nil
}

func (s *prepareState) expression(input schema.CollectionFilterExpression, depth uint64, currentType schema.TypeID) (preparedExpression, error) {
	if depth > s.query.Limits.MaxDepth {
		return preparedExpression{}, exhausted("filter depth limit exceeded")
	}
	s.predicates++
	if s.predicates > s.query.Limits.MaxPredicates {
		return preparedExpression{}, exhausted("filter predicate limit exceeded")
	}
	switch input.Kind {
	case schema.CollectionFilterAnd, schema.CollectionFilterOr:
		return s.boolean(input, depth, currentType)
	case schema.CollectionFilterNot:
		return s.negation(input, depth, currentType)
	case schema.CollectionFilterPredicate:
		return s.predicate(input, currentType)
	case schema.CollectionFilterAny, schema.CollectionFilterAll:
		return s.list(input, depth)
	default:
		return preparedExpression{}, invalid("filter expression kind is invalid")
	}
}

func (s *prepareState) boolean(input schema.CollectionFilterExpression, depth uint64, currentType schema.TypeID) (preparedExpression, error) {
	if len(input.Children) == 0 || input.Child != nil || input.Field != "" || input.Operator != "" || input.Value != nil || input.Predicate != nil {
		return preparedExpression{}, invalid("boolean filter shape is invalid")
	}
	result := preparedExpression{kind: input.Kind, children: make([]preparedExpression, len(input.Children))}
	for index := range input.Children {
		child, err := s.expression(input.Children[index], depth+1, currentType)
		if err != nil {
			return preparedExpression{}, err
		}
		result.children[index] = child
	}
	return result, nil
}

func (s *prepareState) negation(input schema.CollectionFilterExpression, depth uint64, currentType schema.TypeID) (preparedExpression, error) {
	if input.Child == nil || len(input.Children) != 0 || input.Field != "" || input.Operator != "" || input.Value != nil || input.Predicate != nil {
		return preparedExpression{}, invalid("negated filter shape is invalid")
	}
	child, err := s.expression(*input.Child, depth+1, currentType)
	if err != nil {
		return preparedExpression{}, err
	}
	return preparedExpression{kind: input.Kind, children: []preparedExpression{child}}, nil
}

func (s *prepareState) predicate(input schema.CollectionFilterExpression, currentType schema.TypeID) (preparedExpression, error) {
	field, current, err := s.resolveField(input.Field, currentType)
	if err != nil {
		return preparedExpression{}, err
	}
	if !current && !slices.Contains(field.Operators, input.Operator) {
		return preparedExpression{}, unsupported("filter operator is not available")
	}
	if current && !nestedOperatorCompatible(s.types, currentType, input.Operator) {
		return preparedExpression{}, unsupported("nested filter operator is not available")
	}
	if optionalOperator(input.Operator) {
		return preparedExpression{}, unsupported("filter operator is not available")
	}
	expectsValue := operatorExpectsValue(input.Operator)
	if expectsValue != (input.Value != nil) || len(input.Children) != 0 || input.Child != nil || input.Predicate != nil {
		return preparedExpression{}, invalid("filter predicate shape is invalid")
	}
	result := preparedExpression{kind: input.Kind, field: field, operator: input.Operator, current: current}
	if !expectsValue {
		return s.charge(result, field.Cost)
	}
	expectedType := field.Type
	if current {
		expectedType = currentType
	}
	membership := input.Operator == schema.FilterIn || input.Operator == schema.FilterNotIn
	value, err := s.resolveValue(*input.Value, expectedType, membership, s.query.Limits.MaxMembership)
	if err != nil {
		return preparedExpression{}, err
	}
	if membership {
		values, ok := value.([]any)
		if !ok {
			return preparedExpression{}, invalid("membership value must be a list")
		}
		if uint64(len(values)) > s.query.Limits.MaxMembership {
			return preparedExpression{}, exhausted("filter membership limit exceeded")
		}
	}
	result.value = value
	return s.charge(result, field.Cost)
}

func (s *prepareState) list(input schema.CollectionFilterExpression, depth uint64) (preparedExpression, error) {
	field := s.fields[input.Field]
	operator := schema.FilterAny
	if input.Kind == schema.CollectionFilterAll {
		operator = schema.FilterAll
	}
	if field == nil || !slices.Contains(field.Operators, operator) {
		return preparedExpression{}, unsupported("list filter field is not available")
	}
	if input.Predicate == nil || len(input.Children) != 0 || input.Child != nil || input.Operator != "" || input.Value != nil {
		return preparedExpression{}, invalid("list filter shape is invalid")
	}
	predicate, err := s.expression(*input.Predicate, depth+1, field.ElementType)
	if err != nil {
		return preparedExpression{}, err
	}
	result, err := s.charge(preparedExpression{kind: input.Kind, field: field, predicate: &predicate}, field.Cost)
	return result, err
}

func (s *prepareState) resolveField(id string, currentType schema.TypeID) (*schema.CollectionQueryFieldDescriptor, bool, error) {
	if currentType != "" {
		if id == "@" {
			return &schema.CollectionQueryFieldDescriptor{Type: currentType, Cost: 1}, true, nil
		}
		return nil, false, unsupported("nested filter field is not available")
	}
	field := s.fields[id]
	if field == nil {
		return nil, false, unsupported("filter field is not available")
	}
	return field, false, nil
}

func (s *prepareState) resolveValue(input schema.CollectionFilterValue, expected schema.TypeID, membership bool, maxMembership uint64) (any, error) {
	if input.Type != expected || (len(input.Literal) == 0) == (input.Variable == "") {
		return nil, invalid("filter value type or source is invalid")
	}
	raw := input.Literal
	if input.Variable != "" {
		var exists bool
		raw, exists = s.variables[input.Variable]
		if !exists {
			return nil, invalid("filter variable is missing")
		}
	}
	return decodeTypedFilterValue(s.types, raw, expected, membership, maxMembership)
}

func (s *prepareState) charge(result preparedExpression, cost uint64) (preparedExpression, error) {
	if !result.current {
		if cost > s.query.Limits.MaxCost || s.cost > s.query.Limits.MaxCost-cost {
			return preparedExpression{}, exhausted("filter cost limit exceeded")
		}
		s.cost += cost
	}
	return result, nil
}

func decodeTypedFilterValue(types schema.Snapshot, raw []byte, expected schema.TypeID, membership bool, maxMembership uint64) (any, error) {
	value, err := decodeJSONValue(raw)
	if err != nil {
		return nil, err
	}
	if !membership {
		return decodeTypedScalar(types, raw, expected)
	}
	if _, ok := value.([]any); !ok {
		return nil, invalid("membership value must be a list")
	}
	if uint64(len(value.([]any))) > maxMembership {
		return nil, exhausted("filter membership limit exceeded")
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, invalid("membership value must be a list")
	}
	result := make([]any, len(elements))
	for index, element := range elements {
		resolved, scalarErr := decodeTypedScalar(types, element, expected)
		if scalarErr != nil {
			return nil, scalarErr
		}
		result[index] = resolved
	}
	return result, nil
}

func decodeTypedScalar(types schema.Snapshot, raw []byte, expected schema.TypeID) (any, error) {
	value, err := schema.CoerceRuntimeInput(types, expected, raw, true)
	if err != nil {
		return nil, invalid("filter value does not match its declared type")
	}
	canonical, err := value.MarshalJSON()
	if err != nil {
		return nil, invalid("filter value does not match its declared type")
	}
	decoded, err := decodeJSONValue(canonical)
	if err != nil || decoded == nil {
		return decoded, err
	}
	if slices.Contains([]schema.ScalarKind{schema.Int64, schema.UInt64, schema.BigInt, schema.Decimal, schema.Duration}, schema.ScalarKind(expected)) {
		text, ok := decoded.(string)
		if !ok {
			return nil, invalid("filter value does not match its declared type")
		}
		return numericValue(text), nil
	}
	return decoded, nil
}

func decodeJSONValue(raw []byte) (any, error) {
	if value, err := protocol.DecodeJSONValue(raw); err == nil {
		return value, nil
	}
	return nil, invalid("filter value is not one valid JSON value")
}

func operatorExpectsValue(operator schema.FilterOperator) bool {
	if slices.Contains([]schema.FilterOperator{schema.FilterIsNull, schema.FilterIsNotNull, schema.FilterExists, schema.FilterNotExists}, operator) {
		return false
	}
	return scalarOperator(operator)
}

func scalarOperator(operator schema.FilterOperator) bool {
	return slices.Contains([]schema.FilterOperator{
		schema.FilterEqual, schema.FilterNotEqual, schema.FilterLess, schema.FilterLessEqual, schema.FilterGreater,
		schema.FilterGreaterEqual, schema.FilterIn, schema.FilterNotIn,
	}, operator)
}

func nestedOperatorCompatible(types schema.Snapshot, typeID schema.TypeID, operator schema.FilterOperator) bool {
	descriptor, exists := types.Lookup(typeID)
	if !exists || (descriptor.Kind != schema.ScalarType && descriptor.Kind != schema.EnumType) {
		return false
	}
	if slices.Contains([]schema.FilterOperator{schema.FilterEqual, schema.FilterNotEqual, schema.FilterIn, schema.FilterNotIn}, operator) {
		return true
	}
	if descriptor.Kind == schema.EnumType {
		return slices.Contains([]schema.FilterOperator{schema.FilterLess, schema.FilterLessEqual, schema.FilterGreater, schema.FilterGreaterEqual}, operator)
	}
	return slices.Contains([]schema.ScalarKind{
		schema.String, schema.ID, schema.Int32, schema.Float64, schema.Int64, schema.UInt64,
		schema.BigInt, schema.Decimal, schema.Timestamp, schema.Duration, schema.UUID,
	}, schema.ScalarKind(typeID)) && slices.Contains([]schema.FilterOperator{
		schema.FilterLess, schema.FilterLessEqual, schema.FilterGreater, schema.FilterGreaterEqual,
	}, operator)
}

func optionalOperator(operator schema.FilterOperator) bool {
	return operator == schema.FilterRegex || operator == schema.FilterFullText || operator == schema.FilterGeoWithin
}
