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
	if actual.id != expected.id {
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

func (v *planValidator) validateDirectives(directives []protocol.Directive, scope *validationScope, node *planNode) []plannedDirective {
	v.applyDirectiveEffects(directives, node)
	planned := make([]plannedDirective, 0, len(directives))
	counts := make(map[string]int, len(directives))
	for _, invocation := range directives {
		definition, registered := v.registry.directives[invocation.Name()]
		if !registered {
			v.add("UNKNOWN_DIRECTIVE", "LANG-006", "directive is not registered", invocation.Source())
			continue
		}
		counts[invocation.Name()]++
		valid := v.validateDirectiveUse(definition, invocation, counts[invocation.Name()], node)
		valid = v.validateDirectiveInvocationArguments(definition.Descriptor, invocation, scope) && valid
		if !v.addDirectiveCost(definition.Descriptor.Cost, 0, invocation.Source()) {
			valid = false
		}
		if valid {
			planned = append(planned, plannedDirective{definition: definition, invocation: invocation})
		}
	}
	return planned
}

func (v *planValidator) planDirectives(nodes []planNode) {
	for nodeIndex := range nodes {
		node := &nodes[nodeIndex]
		for directiveIndex := range node.directives {
			directive := &node.directives[directiveIndex]
			decision, ok := v.planDirective(directive.definition, directive.invocation, node)
			if !ok || !v.addDirectiveCost(0, decision.AdditionalCost, directive.invocation.Source()) ||
				!v.addPlannedResourceCost(decision.AdditionalCost, directive.invocation.Source()) {
				continue
			}
			directive.decision = decision
			if decision.Skip {
				node.optional = true
			}
		}
		v.planDirectives(node.children)
	}
}

func (v *planValidator) applyDirectiveEffects(directives []protocol.Directive, node *planNode) {
	if !node.hasDefinition {
		return
	}
	for _, invocation := range directives {
		definition, registered := v.registry.directives[invocation.Name()]
		if registered && definition.Descriptor.Effect == string(WriteEffect) {
			node.definition.descriptor.Metadata.Effect = WriteEffect
			return
		}
	}
}

func (v *planValidator) validateDirectiveUse(definition registeredDirective, invocation protocol.Directive, count int, node *planNode) bool {
	valid := true
	if count > 1 && !definition.Descriptor.Repeatable {
		v.add("DUPLICATE_DIRECTIVE", "LANG-006", "directive is not repeatable", invocation.Source())
		valid = false
	}
	if !slices.Contains(definition.Descriptor.Locations, node.kind) {
		v.add("INVALID_DIRECTIVE_LOCATION", "LANG-006", "directive is not allowed at this selection", invocation.Source())
		valid = false
	}
	if !definition.standard && !v.directiveCapabilitiesPinned(definition.Descriptor, invocation.Source()) {
		valid = false
	}
	if v.operation.Kind() == protocol.Query && definition.Descriptor.Effect != string(ReadEffect) {
		v.add("EFFECT_NOT_ALLOWED", "LANG-243", "query transitively reaches a write directive", invocation.Source())
		valid = false
	}
	return valid
}

func (v *planValidator) validateDirectiveInvocationArguments(descriptor schema.DirectiveDescriptor, invocation protocol.Directive, scope *validationScope) bool {
	valid := true
	arguments := invocation.Arguments()
	descriptors := make(map[string]schema.DirectiveArgumentDescriptor, len(descriptor.Arguments))
	for _, argument := range descriptor.Arguments {
		descriptors[argument.Name] = argument
	}
	for _, name := range sortedStringKeys(arguments) {
		if _, exists := descriptors[name]; !exists {
			v.add("UNKNOWN_ARGUMENT", "LANG-006", fmt.Sprintf("directive has no argument %q", name), arguments[name].Source())
			valid = false
		}
	}
	for _, argument := range descriptor.Arguments {
		expression, present := arguments[argument.Name]
		if !present {
			if argument.Required && len(argument.Default) == 0 {
				v.add("MISSING_ARGUMENT", "LANG-006", fmt.Sprintf("directive requires argument %q", argument.Name), invocation.Source())
				valid = false
			}
			continue
		}
		before := len(v.issues)
		v.validateExpression(expression, staticType{id: argument.Type, nullable: argument.Nullable, valid: true}, argument.Required, scope)
		valid = len(v.issues) == before && valid
	}
	return valid
}

func (v *planValidator) planDirective(definition registeredDirective, invocation protocol.Directive, node *planNode) (DirectivePlanDecision, bool) {
	if definition.Planner == nil {
		return DirectivePlanDecision{}, true
	}
	coerced, stable, err := v.coerceDirectivePlanArguments(definition.Descriptor, invocation)
	if err != nil {
		v.add("TYPE_MISMATCH", "LANG-006", err.Error(), invocation.Source())
		return DirectivePlanDecision{}, false
	}
	if !stable {
		v.add("DYNAMIC_DIRECTIVE_ARGUMENT", "LANG-243", "planning directives require request-stable arguments", invocation.Source())
		return DirectivePlanDecision{}, false
	}
	decision, err := callDirectivePlanner(definition.Planner, DirectivePlanContext{
		Operation: v.operation.Kind(), Descriptor: cloneDirectiveDescriptor(definition.Descriptor),
		Selection: directiveSelection(*node), Arguments: coerced,
	})
	if err != nil {
		v.add("DIRECTIVE_PLANNING", "LANG-243", "directive planner failed", invocation.Source())
		return DirectivePlanDecision{}, false
	}
	return decision, true
}

func (v *planValidator) directiveCapabilitiesPinned(descriptor schema.DirectiveDescriptor, source protocol.Source) bool {
	required := append([]string{descriptor.Capability}, descriptor.Capabilities...)
	documentRequirements := v.request.Document().Requires()
	valid := true
	for _, capability := range required {
		if !slices.Contains(documentRequirements, capability) {
			v.add("MISSING_DIRECTIVE_CAPABILITY", "LANG-006", fmt.Sprintf("directive requires exact capability %q", capability), source)
			valid = false
		}
	}
	return valid
}

func (v *planValidator) coerceDirectivePlanArguments(descriptor schema.DirectiveDescriptor, invocation protocol.Directive) (DirectiveArguments, bool, error) {
	values := make(map[string]any, len(descriptor.Arguments))
	arguments := invocation.Arguments()
	for _, argument := range descriptor.Arguments {
		expression, present := arguments[argument.Name]
		var raw []byte
		switch {
		case present && expression.Kind() == protocol.LiteralExpression:
			raw, _ = expression.Literal()
		case present && expression.Kind() == protocol.VariableExpression:
			value, exists := v.request.Variable(expression.Name())
			if !exists {
				variable := v.variables[expression.Name()]
				if variable != nil {
					value, exists = variable.definition.Default()
				}
			}
			if !exists {
				if argument.Required {
					return DirectiveArguments{}, true, fmt.Errorf("required directive argument %q is missing", argument.Name)
				}
				continue
			}
			raw = value
		case present:
			return DirectiveArguments{}, false, nil
		case len(argument.Default) != 0:
			raw = argument.Default
		case argument.Required:
			return DirectiveArguments{}, true, fmt.Errorf("required directive argument %q is missing", argument.Name)
		default:
			continue
		}
		value, err := coerceDirectiveValue(v.registry.types, argument, raw, false)
		if err != nil {
			return DirectiveArguments{}, true, fmt.Errorf("directive argument %q: %w", argument.Name, err)
		}
		values[argument.Name] = value
	}
	return DirectiveArguments{values: values}, true, nil
}

func (v *planValidator) addDirectiveCost(declared, additional uint64, source protocol.Source) bool {
	if additional > MaxDirectivePlanCost || declared > MaxDirectivePlanCost-additional {
		v.add("DIRECTIVE_COST_EXCEEDED", "LANG-243", "directive cost exceeds the portable limit", source)
		return false
	}
	cost := declared + additional
	if v.directiveCost > MaxDirectivePlanCost-cost {
		v.add("DIRECTIVE_COST_EXCEEDED", "LANG-243", "operation directive cost exceeds the portable limit", source)
		return false
	}
	v.directiveCost += cost
	return true
}

func coerceDirectiveValue(types schema.Snapshot, argument schema.DirectiveArgumentDescriptor, raw []byte, runtimeValue bool) (any, error) {
	coerce := schema.CoerceInput
	if runtimeValue {
		coerce = schema.CoerceRuntimeInput
	}
	coerced, err := coerce(types, argument.Type, raw, argument.Nullable)
	if err != nil {
		return nil, err
	}
	descriptor, ok := types.Lookup(argument.Type)
	if !ok {
		return nil, fmt.Errorf("input type %q is unavailable", argument.Type)
	}
	return inputForHandler(coerced, descriptor, argument.Nullable)
}
