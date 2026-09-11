// Package protocol implements Naatre's transport-independent envelopes and AST.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Limits bounds work performed by the strict decoder. Zero fields use safe
// defaults so callers can override only the limits they need.
type Limits struct {
	MaxBytes       int
	MaxTokens      int
	MaxDepth       int
	MaxStringBytes int
	MaxMembers     int
	MaxArrayItems  int
	MaxNumberBytes int
}

// DefaultLimits returns the core profile's decoder limits.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:       1 << 20,
		MaxTokens:      100_000,
		MaxDepth:       64,
		MaxStringBytes: 1 << 20,
		MaxMembers:     10_000,
		MaxArrayItems:  100_000,
		MaxNumberBytes: 128,
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

// Selection is an immutable typed selection node.
type Selection struct {
	kind     SelectionKind
	name     string
	alias    string
	bind     string
	source   Source
	children []Selection
}

func (s Selection) Kind() SelectionKind { return s.kind }
func (s Selection) Name() string        { return s.name }
func (s Selection) Alias() string       { return s.alias }
func (s Selection) Bind() string        { return s.bind }
func (s Selection) Source() Source      { return s.source }

// Selections returns an isolated copy of child selections.
func (s Selection) Selections() []Selection { return cloneSelections(s.children) }

// Operation is an immutable operation declaration.
type Operation struct {
	name       string
	kind       OperationKind
	selections []Selection
	source     Source
}

func (o Operation) Name() string            { return o.name }
func (o Operation) Kind() OperationKind     { return o.kind }
func (o Operation) Source() Source          { return o.source }
func (o Operation) Selections() []Selection { return cloneSelections(o.selections) }

// Document is an immutable operation document.
type Document struct {
	operations []Operation
}

// Operations returns isolated operation values and selection slices.
func (d Document) Operations() []Operation {
	return cloneSliceWith(d.operations, func(operation Operation) Operation {
		operation.selections = cloneSelections(operation.selections)
		return operation
	})
}

// PersistedReference identifies a canonical persisted document.
type PersistedReference struct {
	Algorithm string
	Digest    string
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
	return &Document{operations: r.document.Operations()}
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
	return cloneSliceWith(input, func(selection Selection) Selection {
		selection.children = cloneSelections(selection.children)
		return selection
	})
}

func cloneRawLookup(input map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := input[key]
	return append(json.RawMessage(nil), raw...), ok
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
