package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/valksor/naatre/internal/slicesx"
	"github.com/valksor/naatre/protocol"
)

const maxPortableIndex = uint64(9007199254740991)

var typeIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// Expression is one immutable, typed operation-language expression.
type Expression struct{ encoded json.RawMessage }

// Literal creates an inert JSON literal expression.
func Literal(value json.RawMessage) (Expression, error) {
	canonical, err := protocol.CanonicalizeJSON(value, protocol.Limits{})
	if err != nil {
		return Expression{}, builderError(err)
	}
	return encodeExpression("$literal", canonical)
}

// VariableReference creates an operation or fragment variable reference.
func VariableReference(name string) (Expression, error) { return namedExpression("$var", name) }

// ResultReference creates a reference to a preceding result binding.
func ResultReference(name string) (Expression, error) { return namedExpression("$result", name) }

// ParentReference selects the immediately enclosing data context.
func ParentReference() Expression { return mustBooleanExpression("$parent") }

// CurrentReference selects the current pipeline or selection input.
func CurrentReference() Expression { return mustBooleanExpression("$current") }

func namedExpression(tag, name string) (Expression, error) {
	if !requestIdentifier.MatchString(name) {
		return Expression{}, builderError(errors.New("invalid expression identifier"))
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return Expression{}, builderError(err)
	}
	return encodeExpression(tag, encoded)
}

func mustBooleanExpression(tag string) Expression {
	expression, _ := encodeExpression(tag, json.RawMessage("true"))
	return expression
}

func encodeExpression(tag string, value json.RawMessage) (Expression, error) {
	encoded, err := taggedJSON(tag, value)
	if err != nil {
		return Expression{}, builderError(err)
	}
	return Expression{encoded: encoded}, nil
}

// Argument binds one validated name to one typed expression.
type Argument struct {
	name       string
	expression Expression
}

func NewArgument(name string, expression Expression) (Argument, error) {
	if !requestIdentifier.MatchString(name) || len(expression.encoded) == 0 {
		return Argument{}, builderError(errors.New("invalid argument"))
	}
	return Argument{name: name, expression: Expression{encoded: bytes.Clone(expression.encoded)}}, nil
}

// Directive is one immutable directive invocation.
type Directive struct{ encoded json.RawMessage }

func NewDirective(name string, arguments ...Argument) (Directive, error) {
	if !requestIdentifier.MatchString(name) {
		return Directive{}, builderError(errors.New("invalid directive name"))
	}
	args, err := encodeArguments(arguments)
	if err != nil {
		return Directive{}, err
	}
	wire := directiveWire{Name: name, Arguments: args}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return Directive{}, builderError(err)
	}
	return Directive{encoded: encoded}, nil
}

// Selection and Step are immutable, closed operation-language values.
type Selection struct{ encoded json.RawMessage }
type Step struct{ encoded json.RawMessage }

// SelectionOptions applies the members shared by fields and calls.
type SelectionOptions struct {
	Alias      string
	Bind       string
	Arguments  []Argument
	Directives []Directive
	Select     []Selection
}

func Field(name string, options SelectionOptions) (Selection, error) {
	if len(options.Arguments) != 0 {
		return Selection{}, builderError(errors.New("field cannot carry arguments"))
	}
	payload, err := memberPayload(name, options, false)
	return makeSelection("$field", payload, err)
}

func Call(name string, options SelectionOptions) (Selection, error) {
	payload, err := memberPayload(name, options, true)
	return makeSelection("$call", payload, err)
}

func FieldStep(name string, options SelectionOptions) (Step, error) {
	if options.Alias != "" || len(options.Arguments) != 0 {
		return Step{}, builderError(errors.New("field step cannot carry alias or arguments"))
	}
	payload, err := memberPayload(name, options, false)
	return makeStep("$field", payload, err)
}

func CallStep(name string, options SelectionOptions) (Step, error) {
	if options.Alias != "" {
		return Step{}, builderError(errors.New("call step cannot carry alias"))
	}
	payload, err := memberPayload(name, options, true)
	return makeStep("$call", payload, err)
}

func Pipeline(alias string, stages []Step, options SelectionOptions) (Selection, error) {
	if options.Alias != "" || len(options.Arguments) != 0 || len(options.Select) != 0 || len(stages) == 0 {
		return Selection{}, builderError(errors.New("invalid pipeline options"))
	}
	if err := validOptionalIdentifier(alias, true); err != nil {
		return Selection{}, err
	}
	directives, err := encodeDirectives(options.Directives)
	if err != nil {
		return Selection{}, err
	}
	payload := pipelineWire{Alias: alias, Bind: options.Bind, Directives: directives, Stages: stepBytes(stages)}
	return makeSelection("$pipeline", payload, validateBind(options.Bind))
}

func Map(alias string, selectValues []Selection, options SelectionOptions) (Selection, error) {
	payload, err := collectionPayload(alias, selectValues, options, true)
	return makeSelection("$map", payload, err)
}

func MapStep(selectValues []Selection, options SelectionOptions) (Step, error) {
	payload, err := collectionPayload("", selectValues, options, false)
	return makeStep("$map", payload, err)
}

func Index(alias string, at uint64, selectValues []Selection, options SelectionOptions) (Selection, error) {
	payload, err := indexedPayload(alias, at, selectValues, options, true)
	return makeSelection("$index", payload, err)
}

func IndexStep(at uint64, selectValues []Selection, options SelectionOptions) (Step, error) {
	payload, err := indexedPayload("", at, selectValues, options, false)
	return makeStep("$index", payload, err)
}

func Slice(alias string, start, end *uint64, selectValues []Selection, options SelectionOptions) (Selection, error) {
	payload, err := slicePayload(alias, start, end, selectValues, options, true)
	return makeSelection("$slice", payload, err)
}

func SliceStep(start, end *uint64, selectValues []Selection, options SelectionOptions) (Step, error) {
	payload, err := slicePayload("", start, end, selectValues, options, false)
	return makeStep("$slice", payload, err)
}

func ForwardPage(alias string, first uint64, after *Expression, selectValues []Selection, options SelectionOptions) (Selection, error) {
	payload, err := pagePayload(alias, first, after, true, selectValues, options, true)
	return makeSelection("$page", payload, err)
}

func ForwardPageStep(first uint64, after *Expression, selectValues []Selection, options SelectionOptions) (Step, error) {
	payload, err := pagePayload("", first, after, true, selectValues, options, false)
	return makeStep("$page", payload, err)
}

func BackwardPage(alias string, last uint64, before *Expression, selectValues []Selection, options SelectionOptions) (Selection, error) {
	payload, err := pagePayload(alias, last, before, false, selectValues, options, true)
	return makeSelection("$page", payload, err)
}

func BackwardPageStep(last uint64, before *Expression, selectValues []Selection, options SelectionOptions) (Step, error) {
	payload, err := pagePayload("", last, before, false, selectValues, options, false)
	return makeStep("$page", payload, err)
}

func Meta(name, alias string, options SelectionOptions) (Selection, error) {
	payload, err := metadataPayload(name, alias, options)
	if err != nil {
		return Selection{}, err
	}
	result, err := makeSelection("$meta", payload, nil)
	if err != nil {
		return Selection{}, err
	}
	return result, nil
}

func MetaStep(name string, options SelectionOptions) (Step, error) {
	payload, err := metadataPayload(name, "", options)
	return makeStep("$meta", payload, err)
}

func Current(alias string, options SelectionOptions) (Selection, error) {
	payload, err := currentPayload(alias, options, true)
	return makeSelection("$current", payload, err)
}

func CurrentStep(options SelectionOptions) (Step, error) {
	payload, err := currentPayload("", options, false)
	return makeStep("$current", payload, err)
}

func Nest(alias string, selectValues []Selection, options SelectionOptions) (Selection, error) {
	payload, err := collectionPayload(alias, selectValues, options, true)
	return makeSelection("$nest", payload, err)
}

func Unnest(selectValues []Selection, directives ...Directive) (Selection, error) {
	encodedDirectives, err := encodeDirectives(directives)
	if err != nil {
		return Selection{}, err
	}
	payload := collectionWire{Directives: encodedDirectives, Select: selectionBytes(selectValues)}
	return makeSelection("$unnest", payload, nil)
}

func Parallel(policy string, selectValues []Selection, directives ...Directive) (Selection, error) {
	if policy != "" && policy != "collect" && policy != "fail-fast" {
		return Selection{}, builderError(errors.New("invalid parallel policy"))
	}
	encodedDirectives, err := encodeDirectives(directives)
	if err != nil {
		return Selection{}, err
	}
	payload := parallelWire{Policy: policy, Directives: encodedDirectives, Select: selectionBytes(selectValues)}
	return makeSelection("$parallel", payload, nil)
}

func NamedFragment(name string, arguments []Argument, directives ...Directive) (Selection, error) {
	if !requestIdentifier.MatchString(name) {
		return Selection{}, builderError(errors.New("invalid fragment name"))
	}
	args, err := encodeArguments(arguments)
	if err != nil {
		return Selection{}, err
	}
	encodedDirectives, err := encodeDirectives(directives)
	if err != nil {
		return Selection{}, err
	}
	return makeSelection("$fragment", fragmentSelectionWire{Name: name, Arguments: args, Directives: encodedDirectives}, nil)
}

func InlineFragment(on string, selectValues []Selection, directives ...Directive) (Selection, error) {
	if !typeIdentifier.MatchString(on) {
		return Selection{}, builderError(errors.New("invalid fragment type"))
	}
	encodedDirectives, err := encodeDirectives(directives)
	if err != nil {
		return Selection{}, err
	}
	return makeSelection("$fragment", fragmentSelectionWire{On: on, Directives: encodedDirectives, Select: selectionBytes(selectValues)}, nil)
}

func Atomic(name string, selectValues []Selection, directives ...Directive) (Selection, error) {
	if !requestIdentifier.MatchString(name) {
		return Selection{}, builderError(errors.New("invalid atomic group name"))
	}
	encodedDirectives, err := encodeDirectives(directives)
	if err != nil {
		return Selection{}, err
	}
	return makeSelection("$atomic", atomicWire{Name: name, Directives: encodedDirectives, Select: selectionBytes(selectValues)}, nil)
}

// VariableDefinition is an immutable operation or fragment input declaration.
type VariableDefinition struct {
	name       string
	typeName   string
	required   bool
	nullable   bool
	hasDefault bool
	defaultRaw json.RawMessage
}

func NewVariable(name, typeName string, required, nullable bool) (VariableDefinition, error) {
	if !requestIdentifier.MatchString(name) || !typeIdentifier.MatchString(typeName) {
		return VariableDefinition{}, builderError(errors.New("invalid variable declaration"))
	}
	return VariableDefinition{name: name, typeName: typeName, required: required, nullable: nullable}, nil
}

func (v VariableDefinition) WithDefault(value json.RawMessage) (VariableDefinition, error) {
	canonical, err := protocol.CanonicalizeJSON(value, protocol.Limits{})
	if err != nil {
		return VariableDefinition{}, builderError(err)
	}
	v.hasDefault = true
	v.defaultRaw = canonical
	return v, nil
}

type OperationSpec struct {
	Name      string
	Kind      protocol.OperationKind
	Atomicity protocol.AtomicityMode
	Variables []VariableDefinition
	Select    []Selection
}

type FragmentSpec struct {
	Name       string
	On         string
	Parameters []VariableDefinition
	Select     []Selection
}

// Builder immutably assembles complete documents and delegates final grammar
// validation and canonicalization to protocol.DecodeDocument.
type Builder struct {
	operations []json.RawMessage
	fragments  []json.RawMessage
	requires   []string
}

func NewBuilder() Builder { return Builder{} }

func (b Builder) WithOperation(spec OperationSpec) (Builder, error) {
	if !requestIdentifier.MatchString(spec.Name) || !slices.Contains([]protocol.OperationKind{protocol.Query, protocol.Mutation, protocol.Subscription}, spec.Kind) {
		return Builder{}, builderError(errors.New("invalid operation"))
	}
	if spec.Atomicity != "" && !slices.Contains([]protocol.AtomicityMode{protocol.NoAtomicity, protocol.OperationAtomicity, protocol.GroupAtomicity}, spec.Atomicity) {
		return Builder{}, builderError(errors.New("invalid atomicity"))
	}
	wire, err := operationWireFrom(spec)
	if err != nil {
		return Builder{}, err
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return Builder{}, builderError(err)
	}
	result := b.clone()
	result.operations = append(result.operations, encoded)
	return result, nil
}

func (b Builder) WithFragment(spec FragmentSpec) (Builder, error) {
	if !requestIdentifier.MatchString(spec.Name) || spec.On != "" && !typeIdentifier.MatchString(spec.On) {
		return Builder{}, builderError(errors.New("invalid fragment"))
	}
	parameters, err := variableWires(spec.Parameters)
	if err != nil {
		return Builder{}, err
	}
	wire := fragmentWire{Name: spec.Name, On: spec.On, Parameters: parameters, Select: selectionBytes(spec.Select)}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return Builder{}, builderError(err)
	}
	result := b.clone()
	result.fragments = append(result.fragments, encoded)
	return result, nil
}

func (b Builder) WithCapabilities(capabilities ...string) (Builder, error) {
	values, err := normalizeCapabilities(capabilities)
	if err == nil {
		result := b.clone()
		result.requires = values
		return result, nil
	}
	return Builder{}, builderError(err)
}

func (b Builder) Document() (protocol.Document, error) {
	operations := cloneRawSlice(b.operations)
	wire := documentWire{Operations: operations, Fragments: cloneRawSlice(b.fragments), Requires: slices.Clone(b.requires)}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return protocol.Document{}, builderError(err)
	}
	document, err := protocol.DecodeDocument(encoded, protocol.Limits{})
	if err != nil {
		return protocol.Document{}, builderError(err)
	}
	if err := validateBuilderResponseNames(document); err != nil {
		return protocol.Document{}, err
	}
	return document, nil
}

func (b Builder) Request(operation string) (Request, error) {
	document, err := b.Document()
	if err != nil {
		return Request{}, err
	}
	return NewRequest(document, operation)
}

func (b Builder) clone() Builder {
	return Builder{operations: cloneRawSlice(b.operations), fragments: cloneRawSlice(b.fragments), requires: slices.Clone(b.requires)}
}

type directiveWire struct {
	Name      string                     `json:"name"`
	Arguments map[string]json.RawMessage `json:"arguments,omitempty"`
}

type memberWire struct {
	Name       string                     `json:"name"`
	Alias      string                     `json:"as,omitempty"`
	Bind       string                     `json:"bind,omitempty"`
	Arguments  map[string]json.RawMessage `json:"args,omitempty"`
	Directives []json.RawMessage          `json:"directives,omitempty"`
	Select     []json.RawMessage          `json:"select,omitempty"`
}

type pipelineWire struct {
	Alias      string            `json:"as"`
	Bind       string            `json:"bind,omitempty"`
	Directives []json.RawMessage `json:"directives,omitempty"`
	Stages     []json.RawMessage `json:"stages"`
}

type collectionWire struct {
	Alias      string            `json:"as,omitempty"`
	Bind       string            `json:"bind,omitempty"`
	Directives []json.RawMessage `json:"directives,omitempty"`
	Select     []json.RawMessage `json:"select"`
}

type indexWire struct {
	collectionWire
	At uint64 `json:"at"`
}

type sliceWire struct {
	collectionWire
	Start *uint64 `json:"start,omitempty"`
	End   *uint64 `json:"end,omitempty"`
}

type pageWire struct {
	collectionWire
	First  *uint64         `json:"first,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
	Last   *uint64         `json:"last,omitempty"`
	Before json.RawMessage `json:"before,omitempty"`
}

type metadataWire struct {
	Name       string            `json:"name"`
	Alias      string            `json:"as,omitempty"`
	Bind       string            `json:"bind,omitempty"`
	Directives []json.RawMessage `json:"directives,omitempty"`
}

type currentWire struct {
	Alias      string            `json:"as,omitempty"`
	Bind       string            `json:"bind,omitempty"`
	Directives []json.RawMessage `json:"directives,omitempty"`
}

type parallelWire struct {
	Policy     string            `json:"policy,omitempty"`
	Directives []json.RawMessage `json:"directives,omitempty"`
	Select     []json.RawMessage `json:"select"`
}

type fragmentSelectionWire struct {
	Name       string                     `json:"name,omitempty"`
	On         string                     `json:"on,omitempty"`
	Arguments  map[string]json.RawMessage `json:"args,omitempty"`
	Directives []json.RawMessage          `json:"directives,omitempty"`
	Select     []json.RawMessage          `json:"select,omitempty"`
}

type atomicWire struct {
	Name       string            `json:"name"`
	Directives []json.RawMessage `json:"directives,omitempty"`
	Select     []json.RawMessage `json:"select"`
}

type variableWire struct {
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	Required bool             `json:"required,omitempty"`
	Nullable bool             `json:"nullable,omitempty"`
	Default  *json.RawMessage `json:"default,omitempty"`
}

type operationWire struct {
	Name      string                 `json:"name"`
	Kind      protocol.OperationKind `json:"kind"`
	Atomicity protocol.AtomicityMode `json:"atomicity,omitempty"`
	Variables []variableWire         `json:"variables,omitempty"`
	Select    []json.RawMessage      `json:"select"`
}

type fragmentWire struct {
	Name       string            `json:"name"`
	On         string            `json:"on,omitempty"`
	Parameters []variableWire    `json:"parameters,omitempty"`
	Select     []json.RawMessage `json:"select"`
}

type documentWire struct {
	Operations []json.RawMessage `json:"operations"`
	Fragments  []json.RawMessage `json:"fragments,omitempty"`
	Requires   []string          `json:"requires,omitempty"`
}

func memberPayload(name string, options SelectionOptions, allowArguments bool) (memberWire, error) {
	if !requestIdentifier.MatchString(name) {
		return memberWire{}, builderError(errors.New("invalid member name"))
	}
	if err := validateSelectionOptions(options); err != nil {
		return memberWire{}, err
	}
	args := map[string]json.RawMessage(nil)
	var err error
	if allowArguments {
		args, err = encodeArguments(options.Arguments)
		if err != nil {
			return memberWire{}, err
		}
	}
	directives, err := encodeDirectives(options.Directives)
	if err != nil {
		return memberWire{}, err
	}
	return memberWire{Name: name, Alias: options.Alias, Bind: options.Bind, Arguments: args, Directives: directives, Select: selectionBytes(options.Select)}, nil
}

func collectionPayload(alias string, selectValues []Selection, options SelectionOptions, requireAlias bool) (collectionWire, error) {
	if options.Alias != "" || len(options.Arguments) != 0 || len(options.Select) != 0 {
		return collectionWire{}, builderError(errors.New("invalid collection options"))
	}
	if err := validOptionalIdentifier(alias, requireAlias); err != nil {
		return collectionWire{}, err
	}
	if err := validateBind(options.Bind); err != nil {
		return collectionWire{}, err
	}
	directives, err := encodeDirectives(options.Directives)
	if err != nil {
		return collectionWire{}, err
	}
	return collectionWire{Alias: alias, Bind: options.Bind, Directives: directives, Select: selectionBytes(selectValues)}, nil
}

func indexedPayload(alias string, at uint64, selectValues []Selection, options SelectionOptions, requireAlias bool) (indexWire, error) {
	payload, err := collectionPayload(alias, selectValues, options, requireAlias)
	if err != nil || at > maxPortableIndex {
		if err != nil {
			return indexWire{}, err
		}
		return indexWire{}, builderError(errors.New("index exceeds portable range"))
	}
	return indexWire{collectionWire: payload, At: at}, nil
}

func slicePayload(alias string, start, end *uint64, selectValues []Selection, options SelectionOptions, requireAlias bool) (sliceWire, error) {
	payload, err := collectionPayload(alias, selectValues, options, requireAlias)
	if err != nil {
		return sliceWire{}, err
	}
	if start != nil && *start > maxPortableIndex || end != nil && *end > maxPortableIndex || start != nil && end != nil && *start > *end {
		return sliceWire{}, builderError(errors.New("invalid slice range"))
	}
	return sliceWire{collectionWire: payload, Start: snapshotUint(start), End: snapshotUint(end)}, nil
}

func pagePayload(alias string, count uint64, cursor *Expression, forward bool, selectValues []Selection, options SelectionOptions, requireAlias bool) (pageWire, error) {
	payload, err := collectionPayload(alias, selectValues, options, requireAlias)
	if err != nil {
		return pageWire{}, err
	}
	if count == 0 || count > maxPortableIndex || cursor != nil && len(cursor.encoded) == 0 {
		return pageWire{}, builderError(errors.New("invalid page boundary"))
	}
	result := pageWire{collectionWire: payload}
	if forward {
		result.First = snapshotUint(&count)
		if cursor != nil {
			result.After = bytes.Clone(cursor.encoded)
		}
	} else {
		result.Last = snapshotUint(&count)
		if cursor != nil {
			result.Before = bytes.Clone(cursor.encoded)
		}
	}
	return result, nil
}

func metadataPayload(name, alias string, options SelectionOptions) (metadataWire, error) {
	if !requestIdentifier.MatchString(name) || options.Alias != "" || len(options.Arguments) != 0 || len(options.Select) != 0 {
		return metadataWire{}, builderError(errors.New("invalid metadata selection"))
	}
	if err := validOptionalIdentifier(alias, false); err != nil {
		return metadataWire{}, err
	}
	if err := validateBind(options.Bind); err != nil {
		return metadataWire{}, err
	}
	directives, err := encodeDirectives(options.Directives)
	return metadataWire{Name: name, Alias: alias, Bind: options.Bind, Directives: directives}, err
}

func currentPayload(alias string, options SelectionOptions, requireAlias bool) (currentWire, error) {
	if options.Alias != "" || len(options.Arguments) != 0 || len(options.Select) != 0 {
		return currentWire{}, builderError(errors.New("invalid current selection"))
	}
	if err := validOptionalIdentifier(alias, requireAlias); err != nil {
		return currentWire{}, err
	}
	if err := validateBind(options.Bind); err != nil {
		return currentWire{}, err
	}
	directives, err := encodeDirectives(options.Directives)
	return currentWire{Alias: alias, Bind: options.Bind, Directives: directives}, err
}

func validateSelectionOptions(options SelectionOptions) error {
	if err := validOptionalIdentifier(options.Alias, false); err != nil {
		return err
	}
	return validateBind(options.Bind)
}

func validateBind(bind string) error { return validOptionalIdentifier(bind, false) }

func validOptionalIdentifier(value string, required bool) error {
	if required && value == "" || value != "" && !requestIdentifier.MatchString(value) {
		return builderError(errors.New("invalid response or binding name"))
	}
	return nil
}

func makeSelection(tag string, payload any, prior error) (Selection, error) {
	if prior != nil {
		return Selection{}, prior
	}
	encoded, err := marshalTagged(tag, payload)
	if err != nil {
		return Selection{}, builderError(err)
	}
	return Selection{encoded: encoded}, nil
}

func makeStep(tag string, payload any, prior error) (Step, error) {
	if prior != nil {
		return Step{}, prior
	}
	encoded, err := marshalTagged(tag, payload)
	if err != nil {
		return Step{}, builderError(err)
	}
	return Step{encoded: encoded}, nil
}

func marshalTagged(tag string, payload any) (json.RawMessage, error) {
	encoded, err := json.Marshal(map[string]any{tag: payload})
	if err != nil {
		return nil, err
	}
	return protocol.CanonicalizeJSON(encoded, protocol.Limits{})
}

func taggedJSON(tag string, payload json.RawMessage) (json.RawMessage, error) {
	var encoded bytes.Buffer
	encoded.WriteByte('{')
	tagBytes, _ := json.Marshal(tag)
	encoded.Write(tagBytes)
	encoded.WriteByte(':')
	encoded.Write(payload)
	encoded.WriteByte('}')
	return protocol.CanonicalizeJSON(encoded.Bytes(), protocol.Limits{})
}

func encodeArguments(arguments []Argument) (map[string]json.RawMessage, error) {
	if len(arguments) == 0 {
		return nil, nil
	}
	result := make(map[string]json.RawMessage, len(arguments))
	for _, argument := range arguments {
		if !requestIdentifier.MatchString(argument.name) || len(argument.expression.encoded) == 0 || result[argument.name] != nil {
			return nil, builderError(errors.New("invalid or duplicate argument"))
		}
		result[argument.name] = bytes.Clone(argument.expression.encoded)
	}
	return result, nil
}

func encodeDirectives(directives []Directive) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, len(directives))
	for index, directive := range directives {
		if len(directive.encoded) == 0 {
			return nil, builderError(errors.New("invalid directive"))
		}
		result[index] = bytes.Clone(directive.encoded)
	}
	return result, nil
}

func variableWires(variables []VariableDefinition) ([]variableWire, error) {
	result := make([]variableWire, len(variables))
	seen := make(map[string]bool, len(variables))
	for index, variable := range variables {
		if !requestIdentifier.MatchString(variable.name) || !typeIdentifier.MatchString(variable.typeName) || seen[variable.name] {
			return nil, builderError(errors.New("invalid or duplicate variable"))
		}
		seen[variable.name] = true
		result[index] = variableWire{Name: variable.name, Type: variable.typeName, Required: variable.required, Nullable: variable.nullable}
		if variable.hasDefault {
			value := json.RawMessage(bytes.Clone(variable.defaultRaw))
			result[index].Default = &value
		}
	}
	return result, nil
}

func operationWireFrom(spec OperationSpec) (operationWire, error) {
	variables, err := variableWires(spec.Variables)
	if err != nil {
		return operationWire{}, err
	}
	return operationWire{Name: spec.Name, Kind: spec.Kind, Atomicity: spec.Atomicity, Variables: variables, Select: selectionBytes(spec.Select)}, nil
}

func selectionBytes(values []Selection) []json.RawMessage {
	return append([]json.RawMessage{}, slicesx.Map(values, selectionRaw)...)
}

func selectionRaw(selection Selection) json.RawMessage { return bytes.Clone(selection.encoded) }

func stepBytes(values []Step) []json.RawMessage {
	return append([]json.RawMessage{}, slicesx.Map(values, stepRaw)...)
}

func stepRaw(step Step) json.RawMessage { return bytes.Clone(step.encoded) }

func cloneRawSlice(values []json.RawMessage) []json.RawMessage {
	return slicesx.Map(values, cloneRawMessage)
}

func cloneRawMessage(value json.RawMessage) json.RawMessage { return bytes.Clone(value) }

func snapshotUint(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	result := new(uint64)
	*result = *value
	return result
}

func builderError(cause error) *Error {
	return clientError("INVALID_BUILDER", 0, cause)
}

func validateBuilderResponseNames(document protocol.Document) error {
	fragments := make(map[string][]protocol.Selection)
	for _, fragment := range document.Fragments() {
		fragments[fragment.Name()] = fragment.Selections()
	}
	for _, operation := range document.Operations() {
		if _, err := collectResponseNames(operation.Selections(), fragments, make(map[string]bool)); err != nil {
			return err
		}
	}
	return nil
}

func collectResponseNames(selections []protocol.Selection, fragments map[string][]protocol.Selection, active map[string]bool) (map[string]bool, error) {
	names := make(map[string]bool)
	for _, selection := range selections {
		selectionNames, err := responseNamesForSelection(selection, fragments, active)
		if err != nil {
			return nil, err
		}
		if err := mergeResponseNames(names, selectionNames); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func responseNamesForSelection(selection protocol.Selection, fragments map[string][]protocol.Selection, active map[string]bool) (map[string]bool, error) {
	children := selection.Selections()
	mergeChildren := selection.Kind() == protocol.ParallelSelection || selection.Kind() == protocol.UnnestSelection || selection.Kind() == protocol.AtomicSelection || selection.Kind() == protocol.FragmentSelection
	if len(children) != 0 && !mergeChildren {
		if _, err := collectResponseNames(children, fragments, active); err != nil {
			return nil, err
		}
	}
	for _, stage := range selection.Stages() {
		if _, err := collectResponseNames(stage.Selections(), fragments, active); err != nil {
			return nil, err
		}
	}
	switch selection.Kind() {
	case protocol.FieldSelection, protocol.CallSelection, protocol.MetaSelection:
		name := selection.Alias()
		if name == "" {
			name = selection.Name()
		}
		return map[string]bool{name: true}, nil
	case protocol.PipelineSelection, protocol.MapSelection, protocol.IndexSelection, protocol.SliceSelection, protocol.PageSelection, protocol.CurrentSelection, protocol.NestSelection:
		return map[string]bool{selection.Alias(): true}, nil
	case protocol.ParallelSelection, protocol.UnnestSelection, protocol.AtomicSelection:
		return collectResponseNames(children, fragments, active)
	case protocol.FragmentSelection:
		return fragmentResponseNames(selection, children, fragments, active)
	default:
		return nil, builderError(errors.New("unsupported selection"))
	}
}

func fragmentResponseNames(selection protocol.Selection, inline []protocol.Selection, fragments map[string][]protocol.Selection, active map[string]bool) (map[string]bool, error) {
	name := selection.Name()
	if name == "" {
		return collectResponseNames(inline, fragments, active)
	}
	if active[name] {
		return nil, builderError(errors.New("fragment cycle"))
	}
	selected, found := fragments[name]
	if !found {
		return nil, builderError(errors.New("unknown fragment"))
	}
	active[name] = true
	result, err := collectResponseNames(selected, fragments, active)
	delete(active, name)
	return result, err
}

func mergeResponseNames(target, source map[string]bool) error {
	for name := range source {
		if name == "" || target[name] {
			return builderError(errors.New("duplicate or empty response name"))
		}
		target[name] = true
	}
	return nil
}
