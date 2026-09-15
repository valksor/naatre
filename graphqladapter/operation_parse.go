package graphqladapter

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/valksor/naatre/protocol"
)

// Executable binds the immutable Naatre document to the GraphQL selection
// model needed for response aliases and non-null propagation.
type Executable struct {
	document protocol.Document
	model    *executableModel
}

func (e Executable) Document() protocol.Document { return e.document }

type executableModel struct {
	operations []graphOperation
	fragments  map[string]graphFragment
}

type graphOperation struct {
	name       string
	kind       protocol.OperationKind
	variables  []graphVariable
	selections []graphSelection
}

type graphVariable struct {
	name         string
	typeRef      graphTypeRef
	defaultValue any
	hasDefault   bool
}

type graphSelection struct {
	name          string
	alias         string
	arguments     map[string]graphValue
	directives    []graphDirective
	selections    []graphSelection
	fragmentName  string
	typeCondition string
}

type graphFragment struct {
	name          string
	typeCondition string
	selections    []graphSelection
}

type graphDirective struct {
	name      string
	arguments map[string]graphValue
}

type graphValue struct {
	variable string
	literal  any
}

type operationParser struct {
	lexer      *lexer
	limits     Limits
	model      executableModel
	operations int
	selections int
}

// ImportOperation maps a bounded GraphQL executable document into the
// normative Naatre operation language. Unsupported composition constructs are
// rejected instead of approximated.
func ImportOperation(ctx context.Context, source []byte, imported ImportedSchema, inputLimits Limits) (Executable, error) {
	if imported.model == nil {
		return Executable{}, adapterError(CodeSchemaInvalid, errors.New("GraphQL schema mapping is required"))
	}
	limits, err := normalizeLimits(inputLimits)
	if err != nil {
		return Executable{}, err
	}
	stream, err := newLexer(ctx, source, limits)
	if err != nil {
		return Executable{}, err
	}
	parser := operationParser{lexer: stream, limits: limits, model: executableModel{fragments: make(map[string]graphFragment)}}
	if err := parser.parse(); err != nil {
		return Executable{}, err
	}
	if err := validateExecutable(&parser.model, imported, limits); err != nil {
		return Executable{}, err
	}
	wire, err := parser.naatreWire(imported)
	if err != nil {
		return Executable{}, err
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return Executable{}, adapterError(CodeDocumentInvalid, err)
	}
	document, err := protocol.DecodeDocument(encoded, protocol.Limits{
		MaxBytes: limits.MaxDocumentBytes, MaxTokens: limits.MaxTokens,
		MaxDepth: limits.MaxDepth, MaxMembers: limits.MaxFields,
	})
	if err != nil {
		return Executable{}, adapterError(CodeDocumentInvalid, err)
	}
	return Executable{document: document, model: &parser.model}, nil
}

func (p *operationParser) parse() error {
	for {
		next, err := p.lexer.peek()
		if err != nil {
			return err
		}
		if next.kind == tokenEOF {
			break
		}
		if next.value == "{" {
			if len(p.model.operations) != 0 {
				return adapterError(CodeDocumentInvalid, errors.New("anonymous GraphQL query must be the only operation"))
			}
			selections, selectionErr := p.parseSelectionSet(1)
			if selectionErr != nil {
				return selectionErr
			}
			p.model.operations = append(p.model.operations, graphOperation{name: "Anonymous", kind: protocol.Query, selections: selections})
			continue
		}
		keyword, nameErr := p.lexer.requireName()
		if nameErr != nil {
			return nameErr
		}
		if keyword == "fragment" {
			if err := p.parseFragment(); err != nil {
				return err
			}
			continue
		}
		var kind protocol.OperationKind
		switch keyword {
		case "query":
			kind = protocol.Query
		case "mutation":
			kind = protocol.Mutation
		case "subscription":
			kind = protocol.Subscription
		default:
			return adapterError(CodeDocumentInvalid, errors.New("unknown GraphQL operation kind"))
		}
		if err := p.parseOperation(kind); err != nil {
			return err
		}
	}
	if len(p.model.operations) == 0 {
		return adapterError(CodeDocumentInvalid, errors.New("GraphQL document has no operation"))
	}
	if len(p.model.operations) > 1 {
		for _, operation := range p.model.operations {
			if operation.name == "Anonymous" {
				return adapterError(CodeDocumentInvalid, errors.New("anonymous GraphQL query must be the only operation"))
			}
		}
	}
	seen := make(map[string]bool)
	for _, operation := range p.model.operations {
		if seen[operation.name] {
			return adapterError(CodeDocumentInvalid, errors.New("duplicate GraphQL operation name"))
		}
		seen[operation.name] = true
	}
	return nil
}

func (p *operationParser) parseOperation(kind protocol.OperationKind) error {
	p.operations++
	if p.operations > p.limits.MaxOperations {
		return adapterError(CodeResourceExhausted, errors.New("GraphQL operation limit exceeded"))
	}
	name, err := p.lexer.requireName()
	if err != nil {
		return err
	}
	operation := graphOperation{name: name, kind: kind}
	if variables, variableErr := p.lexer.accept("("); variableErr != nil {
		return variableErr
	} else if variables {
		seen := make(map[string]bool)
		for {
			if closed, closeErr := p.lexer.accept(")"); closeErr != nil || closed {
				if closeErr != nil {
					return closeErr
				}
				break
			}
			if err := p.lexer.require("$"); err != nil {
				return err
			}
			variableName, nameErr := p.lexer.requireName()
			if nameErr != nil || seen[variableName] || p.lexer.require(":") != nil {
				return adapterError(CodeDocumentInvalid, errors.New("invalid or duplicate GraphQL variable"))
			}
			typeRef, typeErr := (&schemaParser{lexer: p.lexer, limits: p.limits}).parseTypeRef(1)
			if typeErr != nil {
				return typeErr
			}
			variable := graphVariable{name: variableName, typeRef: typeRef}
			if defaulted, defaultErr := p.lexer.accept("="); defaultErr != nil {
				return defaultErr
			} else if defaulted {
				value, valueErr := p.parseValue(1, false)
				if valueErr != nil {
					return valueErr
				}
				variable.defaultValue, variable.hasDefault = value.literal, true
			}
			seen[variableName] = true
			operation.variables = append(operation.variables, variable)
		}
	}
	if directed, directErr := p.lexer.accept("@"); directErr != nil {
		return directErr
	} else if directed {
		return adapterError(CodeUnsupported, errors.New("GraphQL operation directives are unsupported"))
	}
	operation.selections, err = p.parseSelectionSet(1)
	if err != nil {
		return err
	}
	p.model.operations = append(p.model.operations, operation)
	return nil
}

func (p *operationParser) parseFragment() error {
	name, err := p.lexer.requireName()
	if err != nil {
		return err
	}
	if _, duplicate := p.model.fragments[name]; duplicate {
		return adapterError(CodeDocumentInvalid, errors.New("duplicate GraphQL fragment"))
	}
	on, err := p.lexer.requireName()
	if err != nil || on != "on" {
		return adapterError(CodeDocumentInvalid, errors.New("GraphQL fragment type condition is required"))
	}
	typeCondition, err := p.lexer.requireName()
	if err != nil {
		return err
	}
	if directed, directErr := p.lexer.accept("@"); directErr != nil {
		return directErr
	} else if directed {
		return adapterError(CodeUnsupported, errors.New("GraphQL fragment directives are unsupported"))
	}
	selections, err := p.parseSelectionSet(1)
	if err != nil {
		return err
	}
	p.model.fragments[name] = graphFragment{name: name, typeCondition: typeCondition, selections: selections}
	return nil
}

func (p *operationParser) parseSelectionSet(depth int) ([]graphSelection, error) {
	if depth > p.limits.MaxDepth {
		return nil, adapterError(CodeResourceExhausted, errors.New("GraphQL selection depth exceeded"))
	}
	if err := p.lexer.require("{"); err != nil {
		return nil, err
	}
	var result []graphSelection
	for {
		if closed, err := p.lexer.accept("}"); err != nil || closed {
			return result, err
		}
		p.selections++
		if p.selections > p.limits.MaxFields {
			return nil, adapterError(CodeResourceExhausted, errors.New("GraphQL selection limit exceeded"))
		}
		if spread, err := p.lexer.accept("..."); err != nil {
			return nil, err
		} else if spread {
			name, nameErr := p.lexer.requireName()
			if nameErr != nil {
				return nil, nameErr
			}
			selection := graphSelection{}
			if name == "on" {
				selection.typeCondition, nameErr = p.lexer.requireName()
				if nameErr != nil {
					return nil, nameErr
				}
				selection.selections, nameErr = p.parseSelectionSet(depth + 1)
			} else {
				selection.fragmentName = name
			}
			if nameErr != nil {
				return nil, nameErr
			}
			result = append(result, selection)
			continue
		}
		first, err := p.lexer.requireName()
		if err != nil {
			return nil, err
		}
		selection := graphSelection{name: first}
		if aliased, aliasErr := p.lexer.accept(":"); aliasErr != nil {
			return nil, aliasErr
		} else if aliased {
			selection.alias = first
			selection.name, err = p.lexer.requireName()
			if err != nil {
				return nil, err
			}
		}
		selection.arguments, err = p.parseArguments(depth)
		if err != nil {
			return nil, err
		}
		selection.directives, err = p.parseDirectives(depth)
		if err != nil {
			return nil, err
		}
		if next, peekErr := p.lexer.peek(); peekErr != nil {
			return nil, peekErr
		} else if next.value == "{" {
			selection.selections, err = p.parseSelectionSet(depth + 1)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, selection)
	}
}

func (p *operationParser) parseArguments(depth int) (map[string]graphValue, error) {
	opened, err := p.lexer.accept("(")
	if err != nil || !opened {
		return nil, err
	}
	result := make(map[string]graphValue)
	for {
		if closed, closeErr := p.lexer.accept(")"); closeErr != nil || closed {
			return result, closeErr
		}
		name, nameErr := p.lexer.requireName()
		_, duplicate := result[name]
		if nameErr != nil || duplicate || p.lexer.require(":") != nil {
			return nil, adapterError(CodeDocumentInvalid, errors.New("invalid or duplicate GraphQL argument"))
		}
		value, valueErr := p.parseValue(depth+1, true)
		if valueErr != nil {
			return nil, valueErr
		}
		result[name] = value
	}
}

func (p *operationParser) parseDirectives(depth int) ([]graphDirective, error) {
	var result []graphDirective
	for {
		present, err := p.lexer.accept("@")
		if err != nil || !present {
			return result, err
		}
		name, nameErr := p.lexer.requireName()
		if nameErr != nil {
			return nil, nameErr
		}
		if name != "include" && name != "skip" {
			return nil, adapterError(CodeUnsupported, errors.New("GraphQL directive is unsupported"))
		}
		arguments, argumentErr := p.parseArguments(depth + 1)
		if argumentErr != nil {
			return nil, argumentErr
		}
		if len(arguments) != 1 {
			return nil, adapterError(CodeDocumentInvalid, errors.New("GraphQL include or skip directive requires one argument"))
		}
		result = append(result, graphDirective{name: name, arguments: arguments})
	}
}

func (p *operationParser) parseValue(depth int, variables bool) (graphValue, error) {
	if depth > p.limits.MaxDepth {
		return graphValue{}, adapterError(CodeResourceExhausted, errors.New("GraphQL value depth exceeded"))
	}
	if variable, err := p.lexer.accept("$"); err != nil {
		return graphValue{}, err
	} else if variable {
		if !variables {
			return graphValue{}, adapterError(CodeUnsupported, errors.New("GraphQL variables nested in composite values are unsupported"))
		}
		name, nameErr := p.lexer.requireName()
		return graphValue{variable: name}, nameErr
	}
	next, err := p.lexer.next()
	if err != nil {
		return graphValue{}, err
	}
	switch next.kind {
	case tokenEOF:
		return graphValue{}, adapterError(CodeDocumentInvalid, errors.New("GraphQL value is absent"))
	case tokenString:
		return graphValue{literal: next.value}, nil
	case tokenNumber:
		return graphValue{literal: json.Number(next.value)}, nil
	case tokenName:
		switch next.value {
		case "true":
			return graphValue{literal: true}, nil
		case "false":
			return graphValue{literal: false}, nil
		case "null":
			return graphValue{literal: nil}, nil
		default:
			return graphValue{}, adapterError(CodeUnsupported, errors.New("GraphQL enum literals require variables to preserve enum semantics"))
		}
	case tokenPunct:
		switch next.value {
		case "[":
			var values []any
			for {
				if closed, closeErr := p.lexer.accept("]"); closeErr != nil || closed {
					return graphValue{literal: values}, closeErr
				}
				value, valueErr := p.parseValue(depth+1, false)
				if valueErr != nil {
					return graphValue{}, valueErr
				}
				values = append(values, value.literal)
			}
		case "{":
			values := make(map[string]any)
			for {
				if closed, closeErr := p.lexer.accept("}"); closeErr != nil || closed {
					return graphValue{literal: values}, closeErr
				}
				name, nameErr := p.lexer.requireName()
				if nameErr != nil || p.lexer.require(":") != nil {
					return graphValue{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL input object"))
				}
				if _, duplicate := values[name]; duplicate {
					return graphValue{}, adapterError(CodeDocumentInvalid, errors.New("duplicate GraphQL input field"))
				}
				value, valueErr := p.parseValue(depth+1, false)
				if valueErr != nil {
					return graphValue{}, valueErr
				}
				values[name] = value.literal
			}
		}
	}
	return graphValue{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL value"))
}
