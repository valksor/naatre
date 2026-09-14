package runtime

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// NewCollectionQueryCursorScope binds a cursor directly to the canonical
// collection.query-1 filter and ordered sort ASTs. Variable references are
// replaced with their canonical resolved values so cursor reuse cannot cross
// variable-result boundaries.
func NewCollectionQueryCursorScope(collection string, filter schema.CollectionFilterExpression, variables map[string]json.RawMessage, sortTerms []schema.CollectionSortTerm, options CursorScopeOptions) (CursorScope, error) {
	if filterJSON, sortJSON, err := marshalCollectionQueryScope(filter, variables, sortTerms); err == nil {
		return NewCursorScope(collection, filterJSON, sortJSON, options)
	} else {
		return CursorScope{}, err
	}
}

func marshalCollectionQueryScope(filter schema.CollectionFilterExpression, variables map[string]json.RawMessage, sortTerms []schema.CollectionSortTerm) ([]byte, []byte, error) {
	resolved, err := resolveCollectionCursorVariables(filter, variables)
	if err != nil {
		return nil, nil, err
	}
	filterJSON, err := json.Marshal(resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal cursor filter: %w", err)
	}
	sortJSON, err := json.Marshal(sortTerms)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal cursor sort: %w", err)
	}
	return filterJSON, sortJSON, nil
}

func resolveCollectionCursorVariables(input schema.CollectionFilterExpression, variables map[string]json.RawMessage) (schema.CollectionFilterExpression, error) {
	result := input
	result.Children = make([]schema.CollectionFilterExpression, len(input.Children))
	for index, child := range input.Children {
		resolved, err := resolveCollectionCursorVariables(child, variables)
		if err != nil {
			return schema.CollectionFilterExpression{}, err
		}
		result.Children[index] = resolved
	}
	if input.Child != nil {
		resolved, err := resolveCollectionCursorVariables(*input.Child, variables)
		if err != nil {
			return schema.CollectionFilterExpression{}, err
		}
		result.Child = &resolved
	}
	if input.Predicate != nil {
		resolved, err := resolveCollectionCursorVariables(*input.Predicate, variables)
		if err != nil {
			return schema.CollectionFilterExpression{}, err
		}
		result.Predicate = &resolved
	}
	if input.Value == nil {
		return result, nil
	}
	value := *input.Value
	hasLiteral := len(value.Literal) != 0
	hasVariable := value.Variable != ""
	if hasLiteral == hasVariable {
		return schema.CollectionFilterExpression{}, errors.New("cursor filter value requires exactly one literal or variable")
	}
	raw := value.Literal
	if hasVariable {
		var exists bool
		raw, exists = variables[value.Variable]
		if !exists || len(raw) == 0 {
			return schema.CollectionFilterExpression{}, errors.New("cursor filter variable is missing")
		}
	}
	canonical, err := protocol.CanonicalizeJSON(raw, protocol.DefaultLimits())
	if err != nil {
		return schema.CollectionFilterExpression{}, fmt.Errorf("canonicalize cursor filter variable: %w", err)
	}
	value.Variable = ""
	value.Literal = canonical
	result.Value = &value
	return result, nil
}
