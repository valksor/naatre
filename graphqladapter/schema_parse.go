package graphqladapter

import (
	"context"
	"errors"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func parseSchema(ctx context.Context, source []byte, limits Limits) (*schemaModel, error) {
	stream, err := newLexer(ctx, source, limits)
	if err != nil {
		return nil, err
	}
	parser := &schemaParser{
		lexer: stream, limits: limits,
		model: schemaModel{
			roots: map[protocol.OperationKind]string{
				protocol.Query: "Query", protocol.Mutation: "Mutation", protocol.Subscription: "Subscription",
			},
			types: make(map[string]*graphType),
		},
	}
	for {
		next, nextErr := stream.peek()
		if nextErr != nil {
			return nil, nextErr
		}
		if next.kind == tokenEOF {
			break
		}
		if next.kind == tokenString {
			_, _ = stream.next()
			continue
		}
		keyword, nameErr := stream.requireName()
		if nameErr != nil {
			return nil, nameErr
		}
		switch keyword {
		case "schema":
			if parser.model.explicitRoots {
				return nil, adapterError(CodeSchemaInvalid, errors.New("duplicate GraphQL schema definition"))
			}
			parser.model.explicitRoots = true
			parser.model.roots = make(map[protocol.OperationKind]string)
			if err := parser.parseRoots(); err != nil {
				return nil, err
			}
		case "type":
			if err := parser.parseObject(schema.ObjectType); err != nil {
				return nil, err
			}
		case "input":
			if err := parser.parseObject(schema.InputObjectType); err != nil {
				return nil, err
			}
		case "enum":
			if err := parser.parseEnum(); err != nil {
				return nil, err
			}
		case "scalar":
			if err := parser.parseScalar(); err != nil {
				return nil, err
			}
		case "union":
			if err := parser.parseUnion(); err != nil {
				return nil, err
			}
		default:
			return nil, adapterError(CodeUnsupported, errors.New("unsupported GraphQL schema definition"))
		}
	}
	if len(parser.model.types) == 0 {
		return nil, adapterError(CodeSchemaInvalid, errors.New("GraphQL schema has no types"))
	}
	return &parser.model, nil
}

func (p *schemaParser) addType(value *graphType) error {
	p.typeCount++
	if p.typeCount > p.limits.MaxTypes {
		return adapterError(CodeResourceExhausted, errors.New("GraphQL type limit exceeded"))
	}
	if _, exists := p.model.types[value.name]; exists || isBuiltinScalar(value.name) || !validUserGraphQLName(value.name) {
		return adapterError(CodeSchemaInvalid, errors.New("duplicate or reserved GraphQL type"))
	}
	p.model.types[value.name] = value
	return nil
}

func (p *schemaParser) parseRoots() error {
	if err := p.lexer.require("{"); err != nil {
		return err
	}
	seen := make(map[protocol.OperationKind]bool)
	seenNames := make(map[string]bool)
	for {
		if accepted, err := p.lexer.accept("}"); err != nil || accepted {
			return err
		}
		kindName, err := p.lexer.requireName()
		if err != nil {
			return err
		}
		var kind protocol.OperationKind
		switch kindName {
		case "query":
			kind = protocol.Query
		case "mutation":
			kind = protocol.Mutation
		case "subscription":
			kind = protocol.Subscription
		default:
			return adapterError(CodeSchemaInvalid, errors.New("unknown GraphQL root kind"))
		}
		if seen[kind] || p.lexer.require(":") != nil {
			return adapterError(CodeSchemaInvalid, errors.New("duplicate or invalid GraphQL root"))
		}
		name, err := p.lexer.requireName()
		if err != nil {
			return err
		}
		if !validUserGraphQLName(name) || seenNames[name] {
			return adapterError(CodeSchemaInvalid, errors.New("GraphQL root type names must be valid and distinct"))
		}
		seen[kind] = true
		seenNames[name] = true
		p.model.roots[kind] = name
	}
}

func (p *schemaParser) parseObject(kind schema.TypeKind) error {
	name, err := p.lexer.requireName()
	if err != nil {
		return err
	}
	next, err := p.lexer.peek()
	if err != nil {
		return err
	}
	if next.value == "implements" || next.value == "@" {
		return adapterError(CodeUnsupported, errors.New("GraphQL interfaces and type directives are unsupported"))
	}
	if err := p.lexer.require("{"); err != nil {
		return err
	}
	value := &graphType{kind: kind, name: name}
	seen := make(map[string]bool)
	for {
		if accepted, acceptErr := p.lexer.accept("}"); acceptErr != nil || accepted {
			if acceptErr != nil {
				return acceptErr
			}
			break
		}
		peeked, peekErr := p.lexer.peek()
		if peekErr != nil {
			return peekErr
		}
		if peeked.kind == tokenString {
			_, _ = p.lexer.next()
			continue
		}
		field, fieldErr := p.parseField(kind)
		if fieldErr != nil {
			return fieldErr
		}
		if seen[field.name] {
			return adapterError(CodeSchemaInvalid, errors.New("duplicate GraphQL field"))
		}
		seen[field.name] = true
		value.fields = append(value.fields, field)
		p.fieldCount++
		if p.fieldCount > p.limits.MaxFields {
			return adapterError(CodeResourceExhausted, errors.New("GraphQL field limit exceeded"))
		}
	}
	if len(value.fields) == 0 {
		return adapterError(CodeSchemaInvalid, errors.New("GraphQL object has no fields"))
	}
	return p.addType(value)
}

func (p *schemaParser) parseField(kind schema.TypeKind) (graphField, error) {
	name, err := p.lexer.requireName()
	if err != nil || !validUserGraphQLName(name) {
		return graphField{}, adapterError(CodeSchemaInvalid, errors.New("invalid or reserved GraphQL field name"))
	}
	field := graphField{name: name}
	if accepted, acceptErr := p.lexer.accept("("); acceptErr != nil {
		return graphField{}, acceptErr
	} else if accepted {
		if kind == schema.InputObjectType {
			return graphField{}, adapterError(CodeSchemaInvalid, errors.New("GraphQL input fields cannot have arguments"))
		}
		seen := make(map[string]bool)
		for {
			if closed, closeErr := p.lexer.accept(")"); closeErr != nil || closed {
				if closeErr != nil {
					return graphField{}, closeErr
				}
				break
			}
			argumentName, nameErr := p.lexer.requireName()
			if nameErr != nil || !validUserGraphQLName(argumentName) {
				return graphField{}, adapterError(CodeSchemaInvalid, errors.New("invalid or reserved GraphQL argument name"))
			}
			if seen[argumentName] || p.lexer.require(":") != nil {
				return graphField{}, adapterError(CodeSchemaInvalid, errors.New("duplicate or invalid GraphQL argument"))
			}
			argumentType, typeErr := p.parseTypeRef(1)
			if typeErr != nil {
				return graphField{}, typeErr
			}
			if defaulted, defaultErr := p.lexer.accept("="); defaultErr != nil {
				return graphField{}, defaultErr
			} else if defaulted {
				return graphField{}, adapterError(CodeUnsupported, errors.New("GraphQL schema defaults are unsupported"))
			}
			seen[argumentName] = true
			field.arguments = append(field.arguments, graphInput{name: argumentName, typeRef: argumentType})
			p.fieldCount++
			if p.fieldCount > p.limits.MaxFields {
				return graphField{}, adapterError(CodeResourceExhausted, errors.New("GraphQL field limit exceeded"))
			}
		}
	}
	if err := p.lexer.require(":"); err != nil {
		return graphField{}, err
	}
	field.typeRef, err = p.parseTypeRef(1)
	if err != nil {
		return graphField{}, err
	}
	if directed, directErr := p.lexer.accept("@"); directErr != nil {
		return graphField{}, directErr
	} else if directed {
		return graphField{}, adapterError(CodeUnsupported, errors.New("GraphQL schema directives are unsupported"))
	}
	return field, nil
}

func (p *schemaParser) parseTypeRef(depth int) (graphTypeRef, error) {
	if depth > p.limits.MaxDepth {
		return graphTypeRef{}, adapterError(CodeResourceExhausted, errors.New("GraphQL type nesting limit exceeded"))
	}
	var result graphTypeRef
	if list, err := p.lexer.accept("["); err != nil {
		return graphTypeRef{}, err
	} else if list {
		element, elementErr := p.parseTypeRef(depth + 1)
		if elementErr != nil {
			return graphTypeRef{}, elementErr
		}
		if err := p.lexer.require("]"); err != nil {
			return graphTypeRef{}, err
		}
		result.element = &element
	} else {
		name, nameErr := p.lexer.requireName()
		if nameErr != nil {
			return graphTypeRef{}, nameErr
		}
		result.name = name
	}
	if nonNull, err := p.lexer.accept("!"); err != nil {
		return graphTypeRef{}, err
	} else {
		result.nonNull = nonNull
	}
	return result, nil
}

func (p *schemaParser) parseEnum() error {
	name, err := p.lexer.requireName()
	if err != nil || p.lexer.require("{") != nil {
		return adapterError(CodeSchemaInvalid, errors.New("invalid GraphQL enum"))
	}
	value := &graphType{kind: schema.EnumType, name: name}
	seen := make(map[string]bool)
	for {
		if closed, closeErr := p.lexer.accept("}"); closeErr != nil || closed {
			if closeErr != nil {
				return closeErr
			}
			break
		}
		member, memberErr := p.lexer.requireName()
		if memberErr != nil || seen[member] || !validUserGraphQLName(member) || member == "true" || member == "false" || member == "null" {
			return adapterError(CodeSchemaInvalid, errors.New("invalid or duplicate GraphQL enum member"))
		}
		if directed, directErr := p.lexer.accept("@"); directErr != nil {
			return directErr
		} else if directed {
			return adapterError(CodeUnsupported, errors.New("GraphQL enum directives are unsupported"))
		}
		seen[member] = true
		value.values = append(value.values, member)
	}
	if len(value.values) == 0 {
		return adapterError(CodeSchemaInvalid, errors.New("GraphQL enum has no members"))
	}
	return p.addType(value)
}

func (p *schemaParser) parseScalar() error {
	name, err := p.lexer.requireName()
	if err != nil {
		return err
	}
	if directed, directErr := p.lexer.accept("@"); directErr != nil {
		return directErr
	} else if directed {
		return adapterError(CodeUnsupported, errors.New("GraphQL scalar directives are unsupported"))
	}
	return p.addType(&graphType{kind: schema.ScalarType, name: name})
}

func (p *schemaParser) parseUnion() error {
	name, err := p.lexer.requireName()
	if err != nil || p.lexer.require("=") != nil {
		return adapterError(CodeSchemaInvalid, errors.New("invalid GraphQL union"))
	}
	value := &graphType{kind: schema.UnionType, name: name}
	seen := make(map[string]bool)
	for {
		_, _ = p.lexer.accept("|")
		member, memberErr := p.lexer.requireName()
		if memberErr != nil || seen[member] {
			return adapterError(CodeSchemaInvalid, errors.New("invalid or duplicate GraphQL union member"))
		}
		seen[member] = true
		value.variants = append(value.variants, member)
		next, nextErr := p.lexer.peek()
		if nextErr != nil {
			return nextErr
		}
		if next.value != "|" {
			break
		}
	}
	return p.addType(value)
}
