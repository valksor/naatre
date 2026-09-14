package schema

import "encoding/json"

type CollectionFilterKind string

const (
	CollectionFilterAnd       CollectionFilterKind = "and"
	CollectionFilterOr        CollectionFilterKind = "or"
	CollectionFilterNot       CollectionFilterKind = "not"
	CollectionFilterPredicate CollectionFilterKind = "predicate"
	CollectionFilterAny       CollectionFilterKind = "any"
	CollectionFilterAll       CollectionFilterKind = "all"
)

// CollectionFilterValue is exactly one typed literal or variable reference.
// A literal containing JSON null is present; a nil Literal is absent.
type CollectionFilterValue struct {
	Type     TypeID          `json:"type"`
	Literal  json.RawMessage `json:"literal,omitempty"`
	Variable string          `json:"variable,omitempty"`
}

// CollectionFilterExpression is the language-neutral collection.query-1 AST.
// Validation enforces the fields allowed for each Kind before evaluation or
// provider translation.
type CollectionFilterExpression struct {
	Kind      CollectionFilterKind         `json:"kind"`
	Children  []CollectionFilterExpression `json:"children,omitempty"`
	Child     *CollectionFilterExpression  `json:"child,omitempty"`
	Field     string                       `json:"field,omitempty"`
	Operator  FilterOperator               `json:"operator,omitempty"`
	Value     *CollectionFilterValue       `json:"value,omitempty"`
	Predicate *CollectionFilterExpression  `json:"predicate,omitempty"`
}

type CollectionSortTerm struct {
	Field     string        `json:"field"`
	Direction SortDirection `json:"direction"`
	Nulls     NullPlacement `json:"nulls"`
}
