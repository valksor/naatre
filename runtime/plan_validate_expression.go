package runtime

import (
	"fmt"
	"slices"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func (v *planValidator) validateArguments(descriptor Descriptor, arguments map[string]protocol.Expression, source protocol.Source, scope *validationScope) {
	input, ok := v.registry.types.Lookup(descriptor.Input)
	if !ok || !input.Input {
		v.add("UNKNOWN_TYPE", "TYPE-001", "call input type is unavailable", source)
		return
	}
	if input.Kind != schema.InputObjectType && input.Kind != schema.OneOfType {
		for _, name := range sortedStringKeys(arguments) {
			v.validateExpression(arguments[name], staticType{id: input.ID, nullable: descriptor.InputNullable, valid: true}, true, scope)
		}
		return
	}
	for _, name := range sortedStringKeys(arguments) {
		field, exists := input.Fields[name]
		if !exists {
			v.add("UNKNOWN_ARGUMENT", "LANG-100", fmt.Sprintf("call input has no argument %q", name), arguments[name].Source())
			continue
		}
		v.validateExpression(arguments[name], staticType{id: field.Type, nullable: field.Nullable, valid: true}, field.Required, scope)
	}
	fieldNames := make([]string, 0, len(input.Fields))
	for name := range input.Fields {
		fieldNames = append(fieldNames, name)
	}
	slices.Sort(fieldNames)
	for _, name := range fieldNames {
		field := input.Fields[name]
		if _, exists := arguments[name]; field.Required && !exists && field.Default == nil {
			v.add("MISSING_ARGUMENT", "LANG-100", fmt.Sprintf("call requires argument %q", name), source)
		}
	}
	if input.Kind == schema.OneOfType && len(arguments) != 1 {
		v.add("INVALID_ARGUMENTS", "TYPE-201", "one-of call input requires one argument", source)
	}
}

func (v *planValidator) validateExpression(expression protocol.Expression, expected staticType, _ bool, scope *validationScope) {
	actual, _ := v.inferExpression(expression, scope)
	if expression.Kind() == protocol.LiteralExpression {
		literal, _ := expression.Literal()
		if _, err := schema.CoerceInput(v.registry.types, expected.id, literal, expected.nullable); err != nil {
			v.add("TYPE_MISMATCH", "LANG-100", err.Error(), expression.Source())
		}
		return
	}
	if !actual.valid {
		return
	}
	if actual.id != expected.id || (actual.nullable && !expected.nullable) {
		v.add("TYPE_MISMATCH", "LANG-100", fmt.Sprintf("expression type %s is incompatible with %s", actual.id, expected.id), expression.Source())
	}
}

func (v *planValidator) inferExpression(expression protocol.Expression, scope *validationScope) (staticType, bool) {
	switch expression.Kind() {
	case protocol.LiteralExpression:
		return staticType{}, true
	case protocol.VariableExpression:
		variable := v.variables[expression.Name()]
		if variable == nil {
			v.add("UNKNOWN_VARIABLE", "LANG-005", "variable use has no declaration", expression.Source())
			return staticType{}, false
		}
		variable.used = true
		return variable.typeInfo, variable.guaranteed
	case protocol.ResultExpression:
		resultSource := expression.PayloadSource()
		if resultSource.Pointer == "" {
			resultSource = expression.Source()
		}
		if binding, ok := scope.bindings[expression.Name()]; ok {
			return binding.typeInfo, true
		}
		if declared, ok := v.allBindings[expression.Name()]; ok {
			selfReference := declared.selection.Start <= expression.Source().Start && declared.selection.End >= expression.Source().End
			if selfReference || declared.selection.Start > expression.Source().Start {
				v.addRelated("RESULT_FORWARD_REFERENCE", "LANG-221", "result binding is self-referential or declared later", resultSource, declared.source)
			} else {
				v.addRelated("RESULT_SCOPE", "LANG-222", "result binding is outside the current sequential scope", resultSource, declared.source)
			}
		} else {
			v.add("UNKNOWN_RESULT", "LANG-221", "result binding is not declared", resultSource)
		}
		return staticType{}, false
	case protocol.CurrentExpression:
		if !scope.current.valid {
			v.add("INVALID_CURRENT", "LANG-103", "current value is unavailable", expression.Source())
			return staticType{}, false
		}
		return scope.current, true
	case protocol.ParentExpression:
		if !scope.parent.valid {
			v.add("INVALID_PARENT", "LANG-103", "parent value is unavailable", expression.Source())
			return staticType{}, false
		}
		return scope.parent, true
	default:
		v.add("INVALID_EXPRESSION", "LANG-004", "expression kind is unsupported", expression.Source())
		return staticType{}, false
	}
}

func (v *planValidator) validateDirectives(directives []protocol.Directive, scope *validationScope) {
	for _, directive := range directives {
		if directive.Name() != "include" && directive.Name() != "skip" {
			v.add("UNKNOWN_DIRECTIVE", "LANG-006", "directive is not registered", directive.Source())
			continue
		}
		arguments := directive.Arguments()
		condition, ok := arguments["if"]
		if !ok {
			v.add("MISSING_ARGUMENT", "LANG-006", "conditional directive requires if", directive.Source())
			continue
		}
		if len(arguments) != 1 {
			v.add("UNKNOWN_ARGUMENT", "LANG-006", "conditional directive accepts only if", directive.Source())
		}
		v.validateExpression(condition, staticType{id: schema.TypeID(schema.Boolean), valid: true}, true, scope)
	}
}
