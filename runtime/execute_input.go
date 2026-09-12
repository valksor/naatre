package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func (p *Plan) handlerInput(node planNode, scope executionScope) (any, executionStatus, error) {
	if node.kind == protocol.FieldSelection {
		return nil, valueAvailable, nil
	}
	arguments := node.selection.Arguments()
	if node.definition.adapter && len(arguments) == 0 {
		return Invocation{Operation: p.operationName, Selection: node.source}, valueAvailable, nil
	}
	descriptor, ok := p.types.Lookup(node.definition.descriptor.Input)
	if !ok {
		return nil, valueUnavailable, errors.New("prepared input type is unavailable")
	}
	var raw json.RawMessage
	if descriptor.Kind == schema.InputObjectType || descriptor.Kind == schema.OneOfType {
		members := make(map[string]json.RawMessage, len(arguments))
		for _, name := range sortedStringKeys(arguments) {
			value, status, err := p.evaluateExpression(arguments[name], scope)
			if err != nil {
				return nil, status, err
			}
			if status == valueMissing {
				if field, exists := descriptor.Fields[name]; exists && field.Required && field.Default == nil {
					return nil, valueMissing, errors.New("required call argument is missing")
				}
				continue
			}
			if status == valueNull {
				if field, exists := descriptor.Fields[name]; exists && !field.Nullable {
					return nil, valueNull, errors.New("non-null call argument is null")
				}
			}
			members[name] = value
		}
		encoded, err := json.Marshal(members)
		if err != nil {
			return nil, valueUnavailable, fmt.Errorf("encode call arguments: %w", err)
		}
		raw = encoded
	} else {
		if len(arguments) == 0 {
			return nil, valueMissing, errors.New("call input is missing")
		}
		if len(arguments) != 1 {
			return nil, valueUnavailable, errors.New("scalar call input requires exactly one argument")
		}
		for _, expression := range arguments {
			var status executionStatus
			var err error
			raw, status, err = p.evaluateExpression(expression, scope)
			if err != nil {
				return nil, status, err
			}
			if status == valueNull && !node.definition.descriptor.InputNullable {
				return nil, valueNull, errors.New("non-null call input is null")
			}
		}
	}
	coerce := schema.CoerceInput
	if containsRuntimeExpression(arguments) {
		coerce = schema.CoerceRuntimeInput
	}
	coerced, err := coerce(p.types, descriptor.ID, raw, node.definition.descriptor.InputNullable)
	if err != nil {
		return nil, valueUnavailable, fmt.Errorf("coerce handler input: %w", err)
	}
	if node.definition.adapter {
		return Invocation{Operation: p.operationName, Selection: node.source}, valueAvailable, nil
	}
	input, err := inputForHandler(coerced, descriptor, node.definition.descriptor.InputNullable)
	if err != nil {
		return nil, valueUnavailable, err
	}
	return input, valueAvailable, nil
}

func containsRuntimeExpression(arguments map[string]protocol.Expression) bool {
	for _, expression := range arguments {
		switch expression.Kind() {
		case protocol.CurrentExpression, protocol.ParentExpression, protocol.ResultExpression:
			return true
		case protocol.LiteralExpression, protocol.VariableExpression:
		}
	}
	return false
}

func inputForHandler(value schema.InputValue, descriptor schema.TypeDescriptor, nullable bool) (any, error) {
	if value.IsMissing() {
		return nil, errors.New("input is missing")
	}
	if value.IsNull() {
		if nullable {
			return nil, nil
		}
		return nil, errors.New("input is null")
	}
	var result any
	if descriptor.Kind != schema.ScalarType || scalarGoType(descriptor.ID) == nil {
		result = value
	} else {
		raw, err := value.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("encode coerced input: %w", err)
		}
		switch schema.ScalarKind(descriptor.ID) {
		case schema.Boolean:
			var decoded bool
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return nil, err
			}
			result = decoded
		case schema.Int32:
			var decoded int32
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return nil, err
			}
			result = decoded
		case schema.Float64:
			var decoded float64
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return nil, err
			}
			result = decoded
		case schema.String, schema.ID, schema.Int64, schema.UInt64, schema.BigInt,
			schema.Decimal, schema.Timestamp, schema.Duration, schema.UUID, schema.Bytes:
			var decoded string
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return nil, err
			}
			result = decoded
		default:
			return nil, fmt.Errorf("unsupported built-in scalar input %q", descriptor.ID)
		}
	}
	if !nullable {
		return result, nil
	}
	pointer := reflect.New(reflect.TypeOf(result))
	pointer.Elem().Set(reflect.ValueOf(result))
	return pointer.Interface(), nil
}

func (p *Plan) evaluateDirectives(node planNode, scope executionScope, path []any) (bool, []ExecutionError) {
	run := true
	for _, directive := range node.selection.Directives() {
		condition := directive.Arguments()["if"]
		raw, status, err := p.evaluateExpression(condition, scope)
		if err != nil {
			code := executionStatusCode(status)
			return false, []ExecutionError{nodeExecutionError(code, "directive condition is unavailable", node, path, err)}
		}
		coerced, err := schema.CoerceInput(p.types, schema.TypeID(schema.Boolean), raw, false)
		if err != nil {
			return false, []ExecutionError{nodeExecutionError("INPUT_COERCION", "directive condition is invalid", node, path, err)}
		}
		value, err := inputForHandler(coerced, schema.TypeDescriptor{ID: schema.TypeID(schema.Boolean), Kind: schema.ScalarType}, false)
		if err != nil {
			return false, []ExecutionError{nodeExecutionError("INPUT_COERCION", "directive condition is invalid", node, path, err)}
		}
		conditionValue := value.(bool)
		if (directive.Name() == "include" && !conditionValue) || (directive.Name() == "skip" && conditionValue) {
			run = false
		}
	}
	return run, nil
}

func (p *Plan) evaluateExpression(expression protocol.Expression, scope executionScope) (json.RawMessage, executionStatus, error) {
	switch expression.Kind() {
	case protocol.LiteralExpression:
		literal, _ := expression.Literal()
		return literal, statusFromRaw(literal), nil
	case protocol.VariableExpression:
		value, ok := p.variableValues[expression.Name()]
		if !ok {
			return nil, valueMissing, nil
		}
		return append(json.RawMessage(nil), value...), statusFromRaw(value), nil
	case protocol.ResultExpression:
		return encodeExecutionValue(scope.bindings[expression.Name()])
	case protocol.CurrentExpression:
		return encodeExecutionValue(scope.current)
	case protocol.ParentExpression:
		return encodeExecutionValue(scope.parent)
	default:
		return nil, valueUnavailable, errors.New("unsupported prepared expression")
	}
}

func encodeExecutionValue(value executionValue) (json.RawMessage, executionStatus, error) {
	if value.status != valueAvailable && value.status != valueNull {
		return nil, value.status, errors.New("referenced runtime value is unavailable")
	}
	encoded, err := json.Marshal(value.value)
	if err != nil {
		return nil, valueUnavailable, fmt.Errorf("encode referenced runtime value: %w", err)
	}
	return encoded, value.status, nil
}
