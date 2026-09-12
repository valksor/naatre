package runtime

import (
	"maps"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func (v *planValidator) narrowTypeCondition(current staticType, condition schema.TypeID) (staticType, bool) {
	conditionDescriptor, conditionOK := v.registry.types.Lookup(condition)
	if !current.valid || !conditionOK || !conditionDescriptor.Output {
		return staticType{}, false
	}
	currentDescriptor, ok := v.registry.types.Lookup(current.id)
	if !ok {
		return staticType{}, false
	}
	currentVariants, currentOK := concreteTypes(currentDescriptor)
	if current.possible != nil {
		currentVariants = current.possible
		currentOK = true
	}
	conditionVariants, conditionOK := concreteTypes(conditionDescriptor)
	if !currentOK || !conditionOK {
		if current.id == condition {
			return current, true
		}
		return staticType{}, false
	}
	intersection := make(map[schema.TypeID]bool)
	for variant := range currentVariants {
		if conditionVariants[variant] {
			intersection[variant] = true
		}
	}
	if len(intersection) == 0 {
		return staticType{}, false
	}
	return staticType{id: condition, nullable: current.nullable, valid: true, possible: intersection}, true
}

func concreteTypes(descriptor schema.TypeDescriptor) (map[schema.TypeID]bool, bool) {
	switch descriptor.Kind {
	case schema.ObjectType:
		return map[schema.TypeID]bool{descriptor.ID: true}, true
	case schema.InterfaceType, schema.UnionType:
		variants := make(map[schema.TypeID]bool, len(descriptor.Variants))
		for _, variant := range descriptor.Variants {
			variants[variant] = true
		}
		return variants, true
	case schema.ScalarType, schema.InputObjectType, schema.ListType, schema.MapType,
		schema.EnumType, schema.OneOfType:
		return nil, false
	}
	return nil, false
}

func (v *planValidator) claimResponseName(selection protocol.Selection, scope *validationScope) string {
	name := selection.Alias()
	if name == "" && (selection.Kind() == protocol.CallSelection || selection.Kind() == protocol.FieldSelection) {
		name = selection.Name()
	}
	if name == "" {
		return ""
	}
	claim := responseClaim{
		source:   selectionResponseNameSource(selection),
		possible: v.possibleTypes(scope.current),
	}
	for _, previous := range scope.responseNames[name] {
		if !conditionsDisjoint(previous, claim) {
			v.addRelated("DUPLICATE_RESPONSE_NAME", "LANG-201", "response name collides after aliasing", selectionResponseNameSource(selection), previous.source)
			break
		}
	}
	scope.responseNames[name] = append(scope.responseNames[name], claim)
	return name
}

func (v *planValidator) possibleTypes(current staticType) map[schema.TypeID]bool {
	if !current.valid {
		return nil
	}
	if current.possible != nil {
		return maps.Clone(current.possible)
	}
	descriptor, ok := v.registry.types.Lookup(current.id)
	if !ok {
		return nil
	}
	possible, concrete := concreteTypes(descriptor)
	if !concrete {
		return nil
	}
	return possible
}
func selectionPayloadSource(selection protocol.Selection) protocol.Source {
	source := selection.PayloadSource()
	if source.Pointer == "" {
		return selection.Source()
	}
	return source
}

func selectionNameSource(selection protocol.Selection) protocol.Source {
	source := selection.NameSource()
	if source.Pointer == "" {
		return selection.Source()
	}
	return source
}

func selectionResponseNameSource(selection protocol.Selection) protocol.Source {
	if selection.Alias() != "" {
		source := selection.AliasSource()
		if source.Pointer != "" {
			return source
		}
	}
	return selectionNameSource(selection)
}

func selectionBindSource(selection protocol.Selection) protocol.Source {
	source := selection.BindSource()
	if source.Pointer == "" {
		return selection.Source()
	}
	return source
}

func conditionsDisjoint(left, right responseClaim) bool {
	if len(left.possible) == 0 || len(right.possible) == 0 {
		return false
	}
	for variant := range left.possible {
		if right.possible[variant] {
			return false
		}
	}
	return true
}

func (v *planValidator) collectBindings(selections []protocol.Selection, fragments map[string]bool) {
	for _, selection := range selections {
		if name := selection.Bind(); name != "" {
			if _, exists := v.allBindings[name]; !exists {
				v.allBindings[name] = bindingDeclaration{source: selectionBindSource(selection), selection: selection.Source()}
			}
		}
		v.collectBindings(selection.Stages(), fragments)
		v.collectBindings(selection.Selections(), fragments)
		if selection.Kind() == protocol.FragmentSelection && !fragments[selection.Name()] {
			if fragment, ok := v.fragments[selection.Name()]; ok {
				fragments[selection.Name()] = true
				v.collectBindings(fragment.Selections(), fragments)
				delete(fragments, selection.Name())
			}
		}
	}
}

func (v *planValidator) add(code, clause, message string, source protocol.Source) {
	v.addRelated(code, clause, message, source)
}

func (v *planValidator) addRelated(code, clause, message string, source protocol.Source, related ...protocol.Source) {
	allRelated := append([]protocol.Source(nil), related...)
	for _, candidate := range v.context {
		if candidate.Pointer == source.Pointer || containsSource(allRelated, candidate) {
			continue
		}
		allRelated = append(allRelated, candidate)
	}
	v.issues = append(v.issues, newValidationIssue(code, clause, message, source, allRelated...))
}

func containsSource(sources []protocol.Source, candidate protocol.Source) bool {
	for _, source := range sources {
		if source.Pointer == candidate.Pointer && source.Start == candidate.Start && source.End == candidate.End {
			return true
		}
	}
	return false
}

func (s validationScope) child(current staticType, freshResponses bool) validationScope {
	bindings := make(map[string]plannedBinding, len(s.bindings))
	for name, binding := range s.bindings {
		bindings[name] = binding
	}
	stack := make(map[string]protocol.Source, len(s.fragmentStack))
	for name, source := range s.fragmentStack {
		stack[name] = source
	}
	responseNames := s.responseNames
	if freshResponses {
		responseNames = make(map[string][]responseClaim)
	}
	return validationScope{
		current: current, parent: s.current, bindings: bindings, responseNames: responseNames,
		fragmentStack: stack, typeConditions: append([]schema.TypeID(nil), s.typeConditions...),
	}
}

func selectionIsOptional(selection protocol.Selection) bool {
	for _, directive := range selection.Directives() {
		condition, exists := directive.Arguments()["if"]
		if !exists {
			return true
		}
		literal, literalOK := condition.Literal()
		if !literalOK || (directive.Name() == "include" && string(literal) != "true") ||
			(directive.Name() == "skip" && string(literal) != "false") {
			return true
		}
	}
	return false
}

func annotatePlanNodes(nodes []planNode, parentPath []string, scheduling Scheduling) {
	for index := range nodes {
		node := &nodes[index]
		node.scheduling = scheduling
		node.ordinal = index
		node.responsePath = append([]string(nil), parentPath...)
		if node.outputName != "" {
			node.responsePath = append(node.responsePath, node.outputName)
		}
		childScheduling := SequentialScheduling
		if node.kind == protocol.ParallelSelection {
			childScheduling = ParallelScheduling
		}
		annotatePlanNodes(node.children, node.responsePath, childScheduling)
	}
}
