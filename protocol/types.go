// Package protocol implements Naatre's transport-independent envelopes and AST.
package protocol

import (
	"encoding/json"
	"fmt"
	"math/big"
)

// Limits bounds work performed by the strict decoder. Zero fields use safe
// defaults so callers can override only the limits they need.
type Limits struct {
	MaxBytes        int
	MaxTokens       int
	MaxDepth        int
	MaxStringBytes  int
	MaxMembers      int
	MaxArrayItems   int
	MaxNumberBytes  int
	MaxLiteralBytes int
}

// DefaultLimits returns the core profile's decoder limits.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:        1 << 20,
		MaxTokens:       100_000,
		MaxDepth:        64,
		MaxStringBytes:  1 << 20,
		MaxMembers:      10_000,
		MaxArrayItems:   100_000,
		MaxNumberBytes:  128,
		MaxLiteralBytes: 1 << 20,
	}
}

func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxBytes == 0 {
		l.MaxBytes = defaults.MaxBytes
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = defaults.MaxTokens
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = defaults.MaxDepth
	}
	if l.MaxStringBytes == 0 {
		l.MaxStringBytes = defaults.MaxStringBytes
	}
	if l.MaxMembers == 0 {
		l.MaxMembers = defaults.MaxMembers
	}
	if l.MaxArrayItems == 0 {
		l.MaxArrayItems = defaults.MaxArrayItems
	}
	if l.MaxNumberBytes == 0 {
		l.MaxNumberBytes = defaults.MaxNumberBytes
	}
	if l.MaxLiteralBytes == 0 {
		l.MaxLiteralBytes = defaults.MaxLiteralBytes
	}
	return l
}

// DecodeOptions configures supported capabilities and extension namespaces.
type DecodeOptions struct {
	Limits              Limits
	Capabilities        map[string]bool
	ExtensionNamespaces map[string]bool
}

// Diagnostic is a stable, source-located protocol error.
type Diagnostic struct {
	Code    string `json:"code"`
	Clause  string `json:"clause"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Pointer string `json:"pointer"`
	Offset  int    `json:"offset"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
}

func (d *Diagnostic) Error() string {
	return fmt.Sprintf("%s at %d:%d (%s): %s", d.Code, d.Line, d.Column, d.Pointer, d.Message)
}

// OperationKind declares query, mutation, or subscription behavior.
type OperationKind string

const (
	Query        OperationKind = "query"
	Mutation     OperationKind = "mutation"
	Subscription OperationKind = "subscription"
)

// SelectionKind identifies one tagged language construct.
type SelectionKind string

const (
	FieldSelection    SelectionKind = "field"
	CallSelection     SelectionKind = "call"
	PipelineSelection SelectionKind = "pipeline"
	MapSelection      SelectionKind = "map"
	IndexSelection    SelectionKind = "index"
	SliceSelection    SelectionKind = "slice"
	PageSelection     SelectionKind = "page"
	MetaSelection     SelectionKind = "meta"
	ParallelSelection SelectionKind = "parallel"
	FragmentSelection SelectionKind = "fragment"
	CurrentSelection  SelectionKind = "current"
	NestSelection     SelectionKind = "nest"
	UnnestSelection   SelectionKind = "unnest"
)

// Source identifies a byte range in the original request.
type Source struct {
	Pointer string
	Start   int
	End     int
	Line    int
	Column  int
}

// ExpressionKind identifies a closed core argument expression.
type ExpressionKind string

const (
	LiteralExpression  ExpressionKind = "literal"
	VariableExpression ExpressionKind = "variable"
	ParentExpression   ExpressionKind = "parent"
	CurrentExpression  ExpressionKind = "current"
	ResultExpression   ExpressionKind = "result"
)

// Expression is an immutable typed argument expression. Literal bytes remain
// lossless until schema coercion.
type Expression struct {
	kind          ExpressionKind
	name          string
	literal       json.RawMessage
	source        Source
	payloadSource Source
}

func (e Expression) Kind() ExpressionKind  { return e.kind }
func (e Expression) Name() string          { return e.name }
func (e Expression) Source() Source        { return e.source }
func (e Expression) PayloadSource() Source { return e.payloadSource }
func (e Expression) Literal() (json.RawMessage, bool) {
	return cloneRaw(e.literal), e.kind == LiteralExpression
}

// Directive is an immutable language directive invocation.
type Directive struct {
	name      string
	arguments map[string]Expression
	source    Source
}

func (d Directive) Name() string                     { return d.name }
func (d Directive) Source() Source                   { return d.source }
func (d Directive) Arguments() map[string]Expression { return cloneExpressions(d.arguments) }

// VariableDefinition declares one operation-local typed input.
type VariableDefinition struct {
	name       string
	typeID     string
	required   bool
	nullable   bool
	defaultRaw json.RawMessage
	hasDefault bool
	source     Source
}

func (v VariableDefinition) Name() string   { return v.name }
func (v VariableDefinition) Type() string   { return v.typeID }
func (v VariableDefinition) Required() bool { return v.required }
func (v VariableDefinition) Nullable() bool { return v.nullable }
func (v VariableDefinition) Source() Source { return v.source }
func (v VariableDefinition) Default() (json.RawMessage, bool) {
	return cloneRaw(v.defaultRaw), v.hasDefault
}

// ParallelPolicy controls admission after a parallel branch failure.
type ParallelPolicy string

const (
	CollectParallel  ParallelPolicy = "collect"
	FailFastParallel ParallelPolicy = "fail-fast"
)

// Selection is an immutable typed selection node.
type Selection struct {
	kind          SelectionKind
	name          string
	typeCondition string
	alias         string
	bind          string
	arguments     map[string]Expression
	directives    []Directive
	children      []Selection
	stages        []Selection
	policy        ParallelPolicy
	at            *big.Int
	start         *big.Int
	end           *big.Int
	first         *big.Int
	last          *big.Int
	after         *Expression
	before        *Expression
	source        Source
	payloadSource Source
	nameSource    Source
	aliasSource   Source
	bindSource    Source
}

func (s Selection) Kind() SelectionKind              { return s.kind }
func (s Selection) Name() string                     { return s.name }
func (s Selection) TypeCondition() string            { return s.typeCondition }
func (s Selection) Alias() string                    { return s.alias }
func (s Selection) Bind() string                     { return s.bind }
func (s Selection) Source() Source                   { return s.source }
func (s Selection) PayloadSource() Source            { return s.payloadSource }
func (s Selection) NameSource() Source               { return s.nameSource }
func (s Selection) AliasSource() Source              { return s.aliasSource }
func (s Selection) BindSource() Source               { return s.bindSource }
func (s Selection) Arguments() map[string]Expression { return cloneExpressions(s.arguments) }
func (s Selection) Directives() []Directive          { return cloneDirectives(s.directives) }
func (s Selection) Stages() []Selection              { return cloneSelections(s.stages) }
func (s Selection) Policy() ParallelPolicy           { return s.policy }
func (s Selection) At() (*big.Int, bool)             { return cloneInteger(s.at), s.at != nil }
func (s Selection) Start() (*big.Int, bool)          { return cloneInteger(s.start), s.start != nil }
func (s Selection) End() (*big.Int, bool)            { return cloneInteger(s.end), s.end != nil }
func (s Selection) First() (*big.Int, bool)          { return cloneInteger(s.first), s.first != nil }
func (s Selection) Last() (*big.Int, bool)           { return cloneInteger(s.last), s.last != nil }
func (s Selection) After() (Expression, bool)        { return pointedValue(s.after) }
func (s Selection) Before() (Expression, bool)       { return pointedValue(s.before) }

// Selections returns an isolated copy of child selections.
func (s Selection) Selections() []Selection { return cloneSelections(s.children) }

// Operation is an immutable operation declaration.
type Operation struct {
	name       string
	kind       OperationKind
	variables  []VariableDefinition
	selections []Selection
	source     Source
}

func (o Operation) Name() string                    { return o.name }
func (o Operation) Kind() OperationKind             { return o.kind }
func (o Operation) Source() Source                  { return o.source }
func (o Operation) Variables() []VariableDefinition { return cloneVariables(o.variables) }
func (o Operation) Selections() []Selection         { return cloneSelections(o.selections) }

// Fragment is an immutable reusable selection declaration.
type Fragment struct {
	name       string
	on         string
	parameters []VariableDefinition
	selections []Selection
	source     Source
}

func (f Fragment) Name() string          { return f.name }
func (f Fragment) TypeCondition() string { return f.on }
func (f Fragment) Source() Source        { return f.source }

// Parameters returns the fragment's typed parameters. A parameter shadows an
// operation variable of the same name for the fragment's selections, and
// requires the fragment-parameter capability.
func (f Fragment) Parameters() []VariableDefinition { return cloneVariables(f.parameters) }
func (f Fragment) Selections() []Selection          { return cloneSelections(f.selections) }

// Document is an immutable operation document.
type Document struct {
	operations []Operation
	fragments  []Fragment
	requires   []string
	canonical  json.RawMessage
	source     Source
}

// Operations returns isolated operation values and selection slices.
func (d Document) Operations() []Operation {
	return cloneOperations(d.operations)
}

func (d Document) Fragments() []Fragment {
	return cloneFragments(d.fragments)
}

func (d Document) Requires() []string { return cloneSlice(d.requires) }
func (d Document) Source() Source     { return d.source }

// CanonicalJSON returns the language-neutral c14n-1 serialization of the AST.
func (d Document) CanonicalJSON() json.RawMessage {
	return cloneRaw(d.canonical)
}

// MarshalJSON emits the same stable AST serialization as CanonicalJSON.
func (d Document) MarshalJSON() ([]byte, error) {
	if len(d.canonical) == 0 {
		return nil, fmt.Errorf("cannot serialize an empty document")
	}
	return d.CanonicalJSON(), nil
}

// PersistedReference identifies a canonical persisted document.
type PersistedReference struct {
	Algorithm        string
	CanonicalVersion string
	Digest           string
}

// Request is an immutable single-operation request envelope.
type Request struct {
	source       Source
	version      string
	id           string
	hasID        bool
	operation    string
	document     *Document
	persisted    *PersistedReference
	variables    map[string]json.RawMessage
	capabilities []string
	extensions   map[string]json.RawMessage
}

func (r *Request) Version() string { return r.version }

func (r *Request) ID() (string, bool) { return r.id, r.hasID }

func (r *Request) OperationName() string { return r.operation }

func (r *Request) Source() Source { return r.source }

func (r *Request) Document() *Document {
	if r.document == nil {
		return nil
	}
	return &Document{
		operations: r.document.Operations(),
		fragments:  r.document.Fragments(),
		requires:   r.document.Requires(),
		canonical:  r.document.CanonicalJSON(),
		source:     r.document.Source(),
	}
}

func (r *Request) Persisted() (PersistedReference, bool) {
	return pointedValue(r.persisted)
}

func (r *Request) Variable(name string) (json.RawMessage, bool) {
	return cloneRawLookup(r.variables, name)
}

// Extension returns an isolated negotiated extension value.
func (r *Request) Extension(namespace string) (json.RawMessage, bool) {
	return cloneRawLookup(r.extensions, namespace)
}

func (r *Request) Capabilities() []string {
	return cloneSlice(r.capabilities)
}

func cloneSelections(input []Selection) []Selection {
	return cloneSliceWith(input, cloneSelection)
}

func cloneSelection(selection Selection) Selection {
	selection.arguments = cloneExpressions(selection.arguments)
	selection.directives = cloneDirectives(selection.directives)
	selection.children = cloneSelections(selection.children)
	selection.stages = cloneSelections(selection.stages)
	selection.at = cloneInteger(selection.at)
	selection.start = cloneInteger(selection.start)
	selection.end = cloneInteger(selection.end)
	selection.first = cloneInteger(selection.first)
	selection.last = cloneInteger(selection.last)
	selection.after = cloneExpressionPointer(selection.after)
	selection.before = cloneExpressionPointer(selection.before)
	return selection
}

func cloneExpressions(input map[string]Expression) map[string]Expression {
	result := make(map[string]Expression, len(input))
	for name, expression := range input {
		expression.literal = cloneRaw(expression.literal)
		result[name] = expression
	}
	return result
}

func cloneDirectives(input []Directive) []Directive {
	return cloneSliceWith(input, cloneDirective)
}

func cloneVariables(input []VariableDefinition) []VariableDefinition {
	return cloneSliceWith(input, cloneVariable)
}

func cloneDirective(directive Directive) Directive {
	directive.arguments = cloneExpressions(directive.arguments)
	return directive
}

func cloneVariable(variable VariableDefinition) VariableDefinition {
	variable.defaultRaw = cloneRaw(variable.defaultRaw)
	return variable
}

func cloneOperations(input []Operation) []Operation {
	return cloneSliceWith(input, cloneOperation)
}

func cloneFragments(input []Fragment) []Fragment {
	return cloneSliceWith(input, cloneFragment)
}

func cloneOperation(operation Operation) Operation {
	operation.variables = cloneVariables(operation.variables)
	operation.selections = cloneSelections(operation.selections)
	return operation
}

func cloneFragment(fragment Fragment) Fragment {
	return Fragment{
		name: fragment.name, on: fragment.on, source: fragment.source,
		parameters: cloneVariables(fragment.parameters), selections: cloneSelections(fragment.selections),
	}
}

func cloneExpressionPointer(input *Expression) *Expression {
	if input == nil {
		return nil
	}
	result := *input
	result.literal = cloneRaw(input.literal)
	return &result
}

func cloneRaw(input json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), input...)
}

func cloneInteger(input *big.Int) *big.Int {
	if input == nil {
		return nil
	}
	return new(big.Int).Set(input)
}

func cloneRawLookup(input map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, exists := input[key]
	if !exists {
		return nil, false
	}
	return cloneRaw(raw), true
}

func cloneSlice[T any](input []T) []T {
	return append([]T(nil), input...)
}

func cloneSliceWith[T any](input []T, clone func(T) T) []T {
	result := make([]T, len(input))
	for index, value := range input {
		result[index] = clone(value)
	}
	return result
}

func pointedValue[T any](input *T) (T, bool) {
	if input == nil {
		var zero T
		return zero, false
	}
	return *input, true
}
