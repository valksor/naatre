package graphqladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// ExportOperation renders the GraphQL-compatible Naatre language subset.
func ExportOperation(ctx context.Context, document protocol.Document, imported ImportedSchema, inputLimits Limits) ([]byte, error) {
	limits, err := normalizeLimits(inputLimits)
	if err != nil {
		return nil, err
	}
	if imported.model == nil {
		return nil, adapterError(CodeSchemaInvalid, errors.New("GraphQL schema mapping is required"))
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	var builder strings.Builder
	operations := document.Operations()
	if len(operations) > limits.MaxOperations {
		return nil, adapterError(CodeResourceExhausted, errors.New("GraphQL operation limit exceeded"))
	}
	for index, operation := range operations {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		if !validUserGraphQLName(operation.Name()) {
			return nil, adapterError(CodeUnsupported, errors.New("naatre operation name has no GraphQL representation"))
		}
		fmt.Fprintf(&builder, "%s %s", operation.Kind(), operation.Name())
		variables := operation.Variables()
		if len(variables) != 0 {
			builder.WriteString("(")
			for variableIndex, variable := range variables {
				if variableIndex > 0 {
					builder.WriteString(", ")
				}
				if !validUserGraphQLName(variable.Name()) {
					return nil, adapterError(CodeUnsupported, errors.New("naatre variable name has no GraphQL representation"))
				}
				typeName, typeErr := imported.operationTypeName(schema.TypeID(variable.Type()), variable.Nullable())
				if typeErr != nil {
					return nil, typeErr
				}
				fmt.Fprintf(&builder, "$%s: %s", variable.Name(), typeName)
				if raw, present := variable.Default(); present {
					literal, literalErr := jsonToGraphQL(raw)
					if literalErr != nil {
						return nil, literalErr
					}
					builder.WriteString(" = " + literal)
				}
			}
			builder.WriteString(")")
		}
		if err := renderSelections(&builder, operation.Selections(), 0, limits, imported); err != nil {
			return nil, err
		}
	}
	fragments := document.Fragments()
	for _, fragment := range fragments {
		if len(fragment.Parameters()) != 0 {
			return nil, adapterError(CodeUnsupported, errors.New("naatre parameterized fragments have no GraphQL representation"))
		}
		typeName := fragment.TypeCondition()
		if !validUserGraphQLName(fragment.Name()) {
			return nil, adapterError(CodeUnsupported, errors.New("naatre fragment name has no GraphQL representation"))
		}
		if mapped, ok := imported.typeNames[schema.TypeID(typeName)]; ok {
			typeName = mapped
		}
		if !validUserGraphQLName(typeName) {
			return nil, adapterError(CodeUnsupported, errors.New("naatre fragment type has no GraphQL representation"))
		}
		fmt.Fprintf(&builder, "\n\nfragment %s on %s", fragment.Name(), typeName)
		if err := renderSelections(&builder, fragment.Selections(), 0, limits, imported); err != nil {
			return nil, err
		}
	}
	if builder.Len() > limits.MaxDocumentBytes {
		return nil, adapterError(CodeResourceExhausted, errors.New("GraphQL document byte limit exceeded"))
	}
	return []byte(builder.String() + "\n"), nil
}

func renderSelections(builder *strings.Builder, selections []protocol.Selection, depth int, limits Limits, imported ImportedSchema) error {
	if depth >= limits.MaxDepth {
		return adapterError(CodeResourceExhausted, errors.New("GraphQL selection depth exceeded"))
	}
	builder.WriteString(" {")
	for _, selection := range selections {
		builder.WriteString(" ")
		switch selection.Kind() {
		case protocol.FieldSelection, protocol.CallSelection:
			if !validUserGraphQLName(selection.Name()) || selection.Alias() != "" && !validUserGraphQLName(selection.Alias()) {
				return adapterError(CodeUnsupported, errors.New("naatre selection name has no GraphQL representation"))
			}
			if selection.Alias() != "" {
				builder.WriteString(selection.Alias() + ": ")
			}
			builder.WriteString(selection.Name())
			if err := renderArguments(builder, selection.Arguments()); err != nil {
				return err
			}
			if err := renderDirectives(builder, selection.Directives()); err != nil {
				return err
			}
			if len(selection.Selections()) != 0 {
				if err := renderSelections(builder, selection.Selections(), depth+1, limits, imported); err != nil {
					return err
				}
			}
		case protocol.FragmentSelection:
			builder.WriteString("...")
			if selection.Name() != "" {
				if !validUserGraphQLName(selection.Name()) {
					return adapterError(CodeUnsupported, errors.New("naatre fragment spread name has no GraphQL representation"))
				}
				builder.WriteString(selection.Name())
			} else if selection.TypeCondition() != "" {
				typeName := selection.TypeCondition()
				if mapped, ok := imported.typeNames[schema.TypeID(typeName)]; ok {
					typeName = mapped
				}
				if !validUserGraphQLName(typeName) {
					return adapterError(CodeUnsupported, errors.New("naatre fragment type has no GraphQL representation"))
				}
				builder.WriteString(" on " + typeName)
				if err := renderSelections(builder, selection.Selections(), depth+1, limits, imported); err != nil {
					return err
				}
			} else {
				return adapterError(CodeDocumentInvalid, errors.New("naatre fragment selection is incomplete"))
			}
		case protocol.PipelineSelection, protocol.MapSelection, protocol.IndexSelection,
			protocol.SliceSelection, protocol.PageSelection, protocol.MetaSelection,
			protocol.ParallelSelection, protocol.CurrentSelection, protocol.NestSelection,
			protocol.UnnestSelection, protocol.AtomicSelection:
			return adapterError(CodeUnsupported, errors.New("naatre composition has no GraphQL representation"))
		}
	}
	builder.WriteString(" }")
	return nil
}

func (s ImportedSchema) operationTypeName(identifier schema.TypeID, nullable bool) (string, error) {
	types := make(map[schema.TypeID]schema.TypeDeclaration)
	for _, value := range s.document.Types() {
		types[value.ID] = value
	}
	return renderType(identifier, nullable, types, s.typeNames)
}

func renderArguments(builder *strings.Builder, arguments map[string]protocol.Expression) error {
	if len(arguments) == 0 {
		return nil
	}
	names := make([]string, 0, len(arguments))
	for name := range arguments {
		names = append(names, name)
	}
	sort.Strings(names)
	builder.WriteString("(")
	for index, name := range names {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(name + ": ")
		expression := arguments[name]
		switch expression.Kind() {
		case protocol.VariableExpression:
			builder.WriteString("$" + expression.Name())
		case protocol.LiteralExpression:
			raw, _ := expression.Literal()
			literal, err := jsonToGraphQL(raw)
			if err != nil {
				return err
			}
			builder.WriteString(literal)
		case protocol.ParentExpression, protocol.CurrentExpression, protocol.ResultExpression:
			return adapterError(CodeUnsupported, errors.New("naatre contextual expression has no GraphQL representation"))
		}
	}
	builder.WriteString(")")
	return nil
}

func renderDirectives(builder *strings.Builder, directives []protocol.Directive) error {
	for _, directive := range directives {
		if directive.Name() != "include" && directive.Name() != "skip" {
			return adapterError(CodeUnsupported, errors.New("naatre directive has no GraphQL representation"))
		}
		builder.WriteString(" @" + directive.Name())
		if err := renderArguments(builder, directive.Arguments()); err != nil {
			return err
		}
	}
	return nil
}

func jsonToGraphQL(raw []byte) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", adapterError(CodeDocumentInvalid, errors.New("invalid Naatre literal"))
	}
	return renderGraphQLValue(value)
}

func renderGraphQLValue(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(typed), nil
	case string:
		return strconv.Quote(typed), nil
	case json.Number:
		return typed.String(), nil
	case []any:
		parts := make([]string, len(typed))
		for index, item := range typed {
			rendered, err := renderGraphQLValue(item)
			if err != nil {
				return "", err
			}
			parts[index] = rendered
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			if !validUserGraphQLName(name) {
				return "", adapterError(CodeUnsupported, errors.New("naatre input object key has no GraphQL representation"))
			}
			rendered, err := renderGraphQLValue(typed[name])
			if err != nil {
				return "", err
			}
			parts = append(parts, name+": "+rendered)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		return "", adapterError(CodeUnsupported, errors.New("naatre literal has no GraphQL representation"))
	}
}
