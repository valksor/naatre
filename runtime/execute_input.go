package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

func (p *Plan) evaluateDirectives(node planNode, scope executionScope, path []any) (bool, []evaluatedDirective, []ExecutionError) {
	run := true
	evaluated := make([]evaluatedDirective, 0, len(node.directives))
	for _, directive := range node.directives {
		arguments, status, err := p.coerceDirectiveExecutionArguments(directive, scope)
		if err != nil {
			return false, nil, directiveArgumentErrors(node, path, status, err)
		}
		evaluated = append(evaluated, evaluatedDirective{planned: directive, arguments: arguments})
		if directive.decision.Skip {
			run = false
		}
		standardRun, err := standardDirectiveRuns(directive, arguments)
		if err != nil {
			return false, nil, []ExecutionError{nodeExecutionError("INPUT_COERCION", "directive condition is missing", node, path, err)}
		}
		if !standardRun {
			run = false
		}
	}
	return run, evaluated, nil
}

func directiveArgumentErrors(node planNode, path []any, status executionStatus, err error) []ExecutionError {
	code := executionStatusCode(status)
	if status == valueAvailable {
		code = "INPUT_COERCION"
	}
	return []ExecutionError{nodeExecutionError(code, "directive argument is invalid", node, path, err)}
}

func standardDirectiveRuns(directive plannedDirective, arguments DirectiveArguments) (bool, error) {
	if !directive.definition.standard {
		return true, nil
	}
	condition, ok := arguments.Value("if")
	if !ok {
		return false, errors.New("prepared directive argument is missing")
	}
	conditionValue := condition.(bool)
	return (directive.invocation.Name() != "include" || conditionValue) && (directive.invocation.Name() != "skip" || !conditionValue), nil
}

func (p *Plan) coerceDirectiveExecutionArguments(directive plannedDirective, scope executionScope) (DirectiveArguments, executionStatus, error) {
	descriptor := directive.definition.Descriptor
	invocationArguments := directive.invocation.Arguments()
	values := make(map[string]any, len(descriptor.Arguments))
	for _, argument := range descriptor.Arguments {
		expression, present := invocationArguments[argument.Name]
		var raw json.RawMessage
		var status executionStatus
		var err error
		runtimeValue := false
		switch {
		case present:
			raw, status, err = p.evaluateExpression(expression, scope)
			runtimeValue = expression.Kind() == protocol.CurrentExpression || expression.Kind() == protocol.ParentExpression || expression.Kind() == protocol.ResultExpression
		case len(argument.Default) != 0:
			raw = append(json.RawMessage(nil), argument.Default...)
			status = statusFromRaw(raw)
		case argument.Required:
			return DirectiveArguments{}, valueMissing, fmt.Errorf("required directive argument %q is missing", argument.Name)
		default:
			continue
		}
		if err != nil {
			return DirectiveArguments{}, status, err
		}
		if status == valueMissing {
			if argument.Required {
				return DirectiveArguments{}, status, fmt.Errorf("required directive argument %q is missing", argument.Name)
			}
			continue
		}
		value, err := coerceDirectiveValue(p.types, argument, raw, runtimeValue)
		if err != nil {
			return DirectiveArguments{}, status, fmt.Errorf("directive argument %q: %w", argument.Name, err)
		}
		values[argument.Name] = value
	}
	return DirectiveArguments{values: values}, valueAvailable, nil
}

func (p *Plan) evaluateExpression(expression protocol.Expression, scope executionScope) (json.RawMessage, executionStatus, error) {
	switch expression.Kind() {
	case protocol.LiteralExpression:
		literal, _ := expression.Literal()
		return literal, statusFromRaw(literal), nil
	case protocol.VariableExpression:
		if variable, scoped := scope.variables[expression.Name()]; scoped {
			if !variable.present {
				return nil, valueMissing, nil
			}
			return append(json.RawMessage(nil), variable.value...), statusFromRaw(variable.value), nil
		}
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

func (p *Plan) fragmentExecutionScope(node planNode, scope executionScope, path []any) (executionScope, []ExecutionError) {
	// A fragment introduces only a parameter scope. Result bindings deliberately
	// share the spread's sequential map, because LANG-241 makes fragment
	// selections behave as though they were written at the spread site.
	fragmentScope := scope
	fragmentScope.variables = maps.Clone(scope.variables)
	if fragmentScope.variables == nil {
		fragmentScope.variables = make(map[string]scopedVariable, len(node.fragmentParams))
	}
	for _, name := range sortedStringKeys(node.fragmentParams) {
		binding := node.fragmentParams[name]
		var raw json.RawMessage
		var status executionStatus
		var err error
		switch {
		case binding.hasExpression:
			raw, status, err = p.evaluateExpression(binding.expression, scope)
		case binding.hasDefault:
			raw = append(json.RawMessage(nil), binding.defaultValue...)
			status = statusFromRaw(raw)
		default:
			fragmentScope.variables[name] = scopedVariable{}
			continue
		}
		if err != nil || status == valueMissing || status == valueUnavailable || status == valueSkipped {
			if err == nil {
				err = errors.New("fragment parameter value is unavailable")
			}
			code := executionStatusCode(status)
			return fragmentScope, []ExecutionError{nodeExecutionError(code, "fragment parameter binding failed", node, path, err)}
		}
		coerce := schema.CoerceInput
		if binding.hasExpression {
			switch binding.expression.Kind() {
			case protocol.LiteralExpression, protocol.VariableExpression:
				// Client-supplied values use the portable input coercer.
			case protocol.ResultExpression, protocol.CurrentExpression, protocol.ParentExpression:
				coerce = schema.CoerceRuntimeInput
			}
		}
		value, coerceErr := coerce(p.types, binding.typeID, raw, binding.nullable)
		if coerceErr != nil {
			return fragmentScope, []ExecutionError{nodeExecutionError("INPUT_COERCION", "fragment parameter binding failed", node, path, coerceErr)}
		}
		canonical, marshalErr := value.MarshalJSON()
		if marshalErr != nil {
			return fragmentScope, []ExecutionError{nodeExecutionError("INPUT_COERCION", "fragment parameter binding failed", node, path, marshalErr)}
		}
		fragmentScope.variables[name] = scopedVariable{value: canonical, present: true}
	}
	return fragmentScope, nil
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
