package graphqladapter

import (
	"errors"
	"sort"
	"strings"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func validateExecutable(executable *executableModel, imported ImportedSchema, limits Limits) error {
	for _, fragment := range executable.fragments {
		value := imported.model.types[fragment.typeCondition]
		if value == nil || value.kind != schema.ObjectType && value.kind != schema.UnionType {
			return adapterError(CodeSchemaInvalid, errors.New("GraphQL fragment type condition is unknown or not selectable"))
		}
	}
	for _, operation := range executable.operations {
		rootName := imported.model.roots[operation.kind]
		root := imported.model.types[rootName]
		if root == nil {
			return adapterError(CodeOperationUnknown, errors.New("GraphQL operation kind has no declared root"))
		}
		if operation.kind == protocol.Subscription && countResponseSelections(operation.selections, executable.fragments, nil) != 1 {
			return adapterError(CodeDocumentInvalid, errors.New("GraphQL subscription requires exactly one root selection"))
		}
		variables := make(map[string]graphVariable, len(operation.variables))
		for _, variable := range operation.variables {
			variables[variable.name] = variable
		}
		if err := validateGraphSelections(rootName, operation.selections, variables, executable.fragments, imported.model, limits, 1, make(map[string]bool)); err != nil {
			return err
		}
	}
	return nil
}

func validateGraphSelections(typeName string, selections []graphSelection, variables map[string]graphVariable, fragments map[string]graphFragment, model *schemaModel, limits Limits, depth int, stack map[string]bool) error {
	if depth > limits.MaxDepth {
		return adapterError(CodeResourceExhausted, errors.New("GraphQL selection validation depth exceeded"))
	}
	parent := model.types[typeName]
	if parent == nil {
		return adapterError(CodeSchemaInvalid, errors.New("GraphQL selection parent type is unknown"))
	}
	for _, selection := range selections {
		if selection.fragmentName != "" {
			fragment, ok := fragments[selection.fragmentName]
			if !ok {
				return adapterError(CodeDocumentInvalid, errors.New("GraphQL fragment spread is unknown"))
			}
			if !selectionTypesOverlap(typeName, fragment.typeCondition, model) {
				return adapterError(CodeDocumentInvalid, errors.New("GraphQL fragment cannot apply at this selection"))
			}
			if stack[selection.fragmentName] {
				return adapterError(CodeDocumentInvalid, errors.New("GraphQL fragment cycle is not executable"))
			}
			stack[selection.fragmentName] = true
			err := validateGraphSelections(fragment.typeCondition, fragment.selections, variables, fragments, model, limits, depth+1, stack)
			delete(stack, selection.fragmentName)
			if err != nil {
				return err
			}
			continue
		}
		if selection.typeCondition != "" {
			if model.types[selection.typeCondition] == nil {
				return adapterError(CodeSchemaInvalid, errors.New("GraphQL inline fragment type is unknown"))
			}
			if !selectionTypesOverlap(typeName, selection.typeCondition, model) {
				return adapterError(CodeDocumentInvalid, errors.New("GraphQL inline fragment cannot apply at this selection"))
			}
			if err := validateGraphSelections(selection.typeCondition, selection.selections, variables, fragments, model, limits, depth+1, stack); err != nil {
				return err
			}
			continue
		}
		if selection.name == "__typename" || strings.HasPrefix(selection.name, "__") {
			return adapterError(CodeUnsupported, errors.New("GraphQL introspection execution is unsupported"))
		}
		field, ok := findField(parent, selection.name)
		if !ok {
			return adapterError(CodeDocumentInvalid, errors.New("GraphQL selection field is not declared"))
		}
		declaredArguments := make(map[string]graphInput, len(field.arguments))
		for _, argument := range field.arguments {
			declaredArguments[argument.name] = argument
			if argument.typeRef.nonNull {
				if _, present := selection.arguments[argument.name]; !present {
					return adapterError(CodeDocumentInvalid, errors.New("GraphQL required argument is absent"))
				}
			}
		}
		for name, value := range selection.arguments {
			argument, declared := declaredArguments[name]
			if !declared {
				return adapterError(CodeDocumentInvalid, errors.New("GraphQL selection argument is not declared"))
			}
			if value.variable != "" {
				variable, exists := variables[value.variable]
				if !exists {
					return adapterError(CodeDocumentInvalid, errors.New("GraphQL selection references an unknown variable"))
				}
				if !variableTypeAllowed(variable, argument.typeRef) {
					return adapterError(CodeDocumentInvalid, errors.New("GraphQL variable type is incompatible with the argument"))
				}
			}
		}
		for _, directive := range selection.directives {
			condition, ok := directive.arguments["if"]
			if !ok || len(directive.arguments) != 1 {
				return adapterError(CodeDocumentInvalid, errors.New("GraphQL include or skip directive requires if"))
			}
			if condition.variable != "" {
				variable, exists := variables[condition.variable]
				if !exists {
					return adapterError(CodeDocumentInvalid, errors.New("GraphQL directive references an unknown variable"))
				}
				if !variableTypeAllowed(variable, graphTypeRef{name: "Boolean", nonNull: true}) {
					return adapterError(CodeDocumentInvalid, errors.New("GraphQL directive variable must be Boolean"))
				}
			}
			if condition.variable == "" {
				if _, boolean := condition.literal.(bool); !boolean {
					return adapterError(CodeDocumentInvalid, errors.New("GraphQL include or skip condition must be Boolean"))
				}
			}
		}
		child := model.types[field.typeRef.named()]
		selectable := child != nil && (child.kind == schema.ObjectType || child.kind == schema.UnionType)
		if selectable && len(selection.selections) == 0 || !selectable && len(selection.selections) != 0 {
			return adapterError(CodeDocumentInvalid, errors.New("GraphQL selection set does not match the field type"))
		}
		if len(selection.selections) != 0 {
			if err := validateGraphSelections(field.typeRef.named(), selection.selections, variables, fragments, model, limits, depth+1, stack); err != nil {
				return err
			}
		}
	}
	return nil
}

func variableTypeAllowed(variable graphVariable, location graphTypeRef) bool {
	if !sameGraphType(variable.typeRef, location) {
		return false
	}
	if location.nonNull && !variable.typeRef.nonNull {
		return variable.hasDefault && variable.defaultValue != nil
	}
	return true
}

func sameGraphType(variable, location graphTypeRef) bool {
	if variable.element == nil || location.element == nil {
		return variable.element == nil && location.element == nil && variable.name == location.name
	}
	if location.element.nonNull && !variable.element.nonNull {
		return false
	}
	return sameGraphType(*variable.element, *location.element)
}

func selectionTypesOverlap(leftName, rightName string, model *schemaModel) bool {
	left := model.types[leftName]
	right := model.types[rightName]
	if left == nil || right == nil {
		return false
	}
	leftTypes := possibleObjectTypes(left, model)
	rightTypes := possibleObjectTypes(right, model)
	for name := range leftTypes {
		if rightTypes[name] {
			return true
		}
	}
	return false
}

func possibleObjectTypes(value *graphType, model *schemaModel) map[string]bool {
	result := make(map[string]bool)
	switch value.kind {
	case schema.ObjectType:
		result[value.name] = true
	case schema.UnionType:
		for _, variant := range value.variants {
			if member := model.types[variant]; member != nil && member.kind == schema.ObjectType {
				result[variant] = true
			}
		}
	case schema.ScalarType, schema.InputObjectType, schema.ListType, schema.MapType,
		schema.EnumType, schema.InterfaceType, schema.OneOfType:
		return result
	}
	return result
}

func countResponseSelections(selections []graphSelection, fragments map[string]graphFragment, stack map[string]bool) int {
	if stack == nil {
		stack = make(map[string]bool)
	}
	count := 0
	for _, selection := range selections {
		if selection.fragmentName != "" {
			if stack[selection.fragmentName] {
				continue
			}
			stack[selection.fragmentName] = true
			count += countResponseSelections(fragments[selection.fragmentName].selections, fragments, stack)
			delete(stack, selection.fragmentName)
		} else if selection.typeCondition != "" {
			count += countResponseSelections(selection.selections, fragments, stack)
		} else {
			count++
		}
	}
	return count
}

func (p *operationParser) naatreWire(imported ImportedSchema) (map[string]any, error) {
	operations := make([]any, 0, len(p.model.operations))
	for _, operation := range p.model.operations {
		variables := make([]any, 0, len(operation.variables))
		for _, variable := range operation.variables {
			typeID, ok := imported.typeRefs[variable.typeRef.string()]
			if !ok {
				return nil, adapterError(CodeSchemaInvalid, errors.New("GraphQL variable type has no Naatre mapping"))
			}
			mapped := map[string]any{
				"name": variable.name, "type": string(typeID),
				"required": variable.typeRef.nonNull, "nullable": !variable.typeRef.nonNull,
			}
			if variable.hasDefault {
				mapped["default"] = variable.defaultValue
			}
			variables = append(variables, mapped)
		}
		selections, err := selectionsToWire(operation.selections, true, imported)
		if err != nil {
			return nil, err
		}
		mapped := map[string]any{"name": operation.name, "kind": operation.kind, "select": selections}
		if len(variables) != 0 {
			mapped["variables"] = variables
		}
		operations = append(operations, mapped)
	}
	fragmentNames := make([]string, 0, len(p.model.fragments))
	for name := range p.model.fragments {
		fragmentNames = append(fragmentNames, name)
	}
	sort.Strings(fragmentNames)
	fragments := make([]any, 0, len(fragmentNames))
	for _, name := range fragmentNames {
		fragment := p.model.fragments[name]
		selections, err := selectionsToWire(fragment.selections, false, imported)
		if err != nil {
			return nil, err
		}
		mapped := map[string]any{"name": fragment.name, "on": string(imported.typeIDs[fragment.typeCondition]), "select": selections}
		if mapped["on"] == "" {
			return nil, adapterError(CodeSchemaInvalid, errors.New("GraphQL fragment references unknown type"))
		}
		fragments = append(fragments, mapped)
	}
	wire := map[string]any{"requires": []string{Profile}, "operations": operations}
	if len(fragments) != 0 {
		wire["fragments"] = fragments
	}
	return wire, nil
}

func selectionsToWire(input []graphSelection, root bool, imported ImportedSchema) ([]any, error) {
	result := make([]any, 0, len(input))
	for _, selection := range input {
		if selection.fragmentName != "" || selection.typeCondition != "" {
			payload := make(map[string]any)
			if selection.fragmentName != "" {
				payload["name"] = selection.fragmentName
			} else {
				typeID, ok := imported.typeIDs[selection.typeCondition]
				if !ok {
					return nil, adapterError(CodeSchemaInvalid, errors.New("GraphQL inline fragment references unknown type"))
				}
				payload["on"] = string(typeID)
				children, err := selectionsToWire(selection.selections, false, imported)
				if err != nil {
					return nil, err
				}
				payload["select"] = children
			}
			result = append(result, map[string]any{"$fragment": payload})
			continue
		}
		payload := map[string]any{"name": selection.name}
		if selection.alias != "" {
			payload["as"] = selection.alias
		}
		if len(selection.arguments) != 0 {
			arguments := make(map[string]any, len(selection.arguments))
			for name, value := range selection.arguments {
				if value.variable != "" {
					arguments[name] = map[string]any{"$var": value.variable}
				} else {
					arguments[name] = map[string]any{"$literal": value.literal}
				}
			}
			payload["args"] = arguments
		}
		if len(selection.directives) != 0 {
			directives := make([]any, 0, len(selection.directives))
			for _, directive := range selection.directives {
				arguments := make(map[string]any, len(directive.arguments))
				for name, value := range directive.arguments {
					if value.variable != "" {
						arguments[name] = map[string]any{"$var": value.variable}
					} else {
						arguments[name] = map[string]any{"$literal": value.literal}
					}
				}
				directives = append(directives, map[string]any{"name": directive.name, "arguments": arguments})
			}
			payload["directives"] = directives
		}
		if len(selection.selections) != 0 {
			children, err := selectionsToWire(selection.selections, false, imported)
			if err != nil {
				return nil, err
			}
			payload["select"] = children
		}
		tag := "$field"
		if root || len(selection.arguments) != 0 {
			tag = "$call"
		}
		result = append(result, map[string]any{tag: payload})
	}
	return result, nil
}
