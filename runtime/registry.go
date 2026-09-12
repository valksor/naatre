// Package runtime provides explicit registration, planning, and execution for
// Naatre operations. It never exposes Go values through implicit reflection.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

var (
	ErrNotRegistered         = errors.New("operation is not registered")
	ErrDuplicateRegistration = errors.New("duplicate public registration")
	ErrInputType             = errors.New("handler input has wrong Go type")
	ErrSourceType            = errors.New("handler source has wrong Go type")
	namePattern              = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	schemaIdentityPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
)

// Scope identifies root operations and members of registered object types.
type Scope string

const (
	RootScope   Scope = "root"
	ObjectScope Scope = "object"
)

// MemberKind distinguishes projections from typed calls.
type MemberKind string

const (
	FieldMember MemberKind = "field"
	CallMember  MemberKind = "call"
)

// Effect records trusted application read/write metadata.
type Effect string

const (
	ReadEffect  Effect = "read"
	WriteEffect Effect = "write"
)

// ThreadSafety records whether concurrent invocation is permitted.
type ThreadSafety string

const (
	ThreadSafe ThreadSafety = "thread-safe"
	SerialOnly ThreadSafety = "serial-only"
)

// Batching records request-loader eligibility independently of cacheability.
type Batching string

const (
	BatchEligible   Batching = "eligible"
	BatchIneligible Batching = "ineligible"
)

// TransactionParticipation records whether a handler can join a transaction.
type TransactionParticipation string

const (
	TransactionNone     TransactionParticipation = "none"
	TransactionOptional TransactionParticipation = "optional"
	TransactionRequired TransactionParticipation = "required"
)

// Metadata contains independent security, effect, and optimization contracts.
type Metadata struct {
	Effect              Effect                   `json:"effect"`
	Deterministic       bool                     `json:"deterministic"`
	Cacheable           bool                     `json:"cacheable"`
	RetrySafe           bool                     `json:"retrySafe"`
	ThreadSafety        ThreadSafety             `json:"threadSafety"`
	Batching            Batching                 `json:"batching"`
	Transaction         TransactionParticipation `json:"transaction"`
	AuthorizationPolicy string                   `json:"authorizationPolicy"`
	Deprecation         string                   `json:"deprecation,omitempty"`
	Idempotency         string                   `json:"idempotency,omitempty"`
	Cost                uint64                   `json:"cost"`
	ParallelMutation    bool                     `json:"parallelMutation"`
}

// Descriptor is the portable public contract for one registered handler.
type Descriptor struct {
	ID             string                   `json:"id,omitempty"`
	Name           string                   `json:"name"`
	Scope          Scope                    `json:"scope"`
	Owner          schema.TypeID            `json:"owner,omitempty"`
	Kind           protocol.OperationKind   `json:"kind,omitempty"`
	Member         MemberKind               `json:"member"`
	Input          schema.TypeID            `json:"input,omitempty"`
	InputNullable  bool                     `json:"inputNullable"`
	Output         schema.TypeID            `json:"output"`
	OutputNullable bool                     `json:"outputNullable"`
	Description    string                   `json:"description,omitempty"`
	Deprecation    *schema.Deprecation      `json:"deprecation,omitempty"`
	Capabilities   []string                 `json:"capabilities,omitempty"`
	Traits         []schema.TraitDescriptor `json:"traits,omitempty"`
	Source         *schema.SourceMetadata   `json:"source,omitempty"`
	Metadata       Metadata                 `json:"metadata"`
}

// Handler is the supported typed root-operation signature.
type Handler[Input, Output any] func(context.Context, Input) (Output, error)

// FieldHandler projects one explicitly typed source value.
type FieldHandler[Source, Output any] func(context.Context, Source) (Output, error)

// CallHandler invokes a named method with an explicitly typed source and input.
type CallHandler[Source, Input, Output any] func(context.Context, Source, Input) (Output, error)

type bindingKind uint8

const (
	rootBinding bindingKind = iota + 1
	fieldBinding
	callBinding
)

// Definition binds a descriptor to one typed handler. Its invocation adapter
// is deliberately private so arbitrary functions cannot enter a registry.
type Definition struct {
	descriptor Descriptor
	invoke     func(context.Context, any, any) (any, error)
	nilHandler bool
	serial     chan struct{}
	sourceType reflect.Type
	inputType  reflect.Type
	outputType reflect.Type
	adapter    bool
	binding    bindingKind
}

// Bind adapts a typed handler to an explicit registration definition.
func Bind[Input, Output any](descriptor Descriptor, handler Handler[Input, Output]) Definition {
	return bindRoot(descriptor, handler)
}

func bindRoot[Input, Output any](descriptor Descriptor, handler Handler[Input, Output]) Definition {
	definition := newDefinition(descriptor, rootBinding, nil, reflect.TypeFor[Input](), reflect.TypeFor[Output](), handler == nil)
	if handler != nil {
		definition.invoke = rootInvoker(descriptor, handler)
	}
	return definition
}

// BindField adapts an explicitly typed object projection.
func BindField[Source, Output any](descriptor Descriptor, handler FieldHandler[Source, Output]) Definition {
	definition := newDefinition(descriptor, fieldBinding, reflect.TypeFor[Source](), nil, reflect.TypeFor[Output](), handler == nil)
	if handler != nil {
		definition.invoke = fieldInvoker(descriptor, handler)
	}
	return definition
}

// BindCall adapts an explicitly typed object method.
func BindCall[Source, Input, Output any](descriptor Descriptor, handler CallHandler[Source, Input, Output]) Definition {
	definition := newDefinition(descriptor, callBinding, reflect.TypeFor[Source](), reflect.TypeFor[Input](), reflect.TypeFor[Output](), handler == nil)
	if handler != nil {
		definition.invoke = callInvoker(descriptor, handler)
	}
	return definition
}

func rootInvoker[Input, Output any](descriptor Descriptor, handler Handler[Input, Output]) func(context.Context, any, any) (any, error) {
	return func(ctx context.Context, _ any, input any) (any, error) {
		typed, err := requireInput[Input](descriptor, input)
		if err != nil {
			return nil, err
		}
		return handler(ctx, typed)
	}
}

func fieldInvoker[Source, Output any](descriptor Descriptor, handler FieldHandler[Source, Output]) func(context.Context, any, any) (any, error) {
	return func(ctx context.Context, source, _ any) (any, error) {
		typed, err := requireSource[Source](descriptor, source)
		if err != nil {
			return nil, err
		}
		return handler(ctx, typed)
	}
}

func callInvoker[Source, Input, Output any](descriptor Descriptor, handler CallHandler[Source, Input, Output]) func(context.Context, any, any) (any, error) {
	return func(ctx context.Context, source, input any) (any, error) {
		typedSource, err := requireSource[Source](descriptor, source)
		if err != nil {
			return nil, err
		}
		typedInput, err := requireInput[Input](descriptor, input)
		if err != nil {
			return nil, err
		}
		return handler(ctx, typedSource, typedInput)
	}
}

func newDefinition(descriptor Descriptor, binding bindingKind, sourceType, inputType, outputType reflect.Type, nilHandler bool) Definition {
	definition := Definition{
		descriptor: cloneRuntimeDescriptor(descriptor), binding: binding, nilHandler: nilHandler,
		sourceType: sourceType, inputType: inputType, outputType: outputType,
	}
	if descriptor.Metadata.ThreadSafety == SerialOnly {
		definition.serial = make(chan struct{}, 1)
		definition.serial <- struct{}{}
	}
	return definition
}

func requireInput[Input any](descriptor Descriptor, input any) (Input, error) {
	if input == nil && descriptor.InputNullable {
		return *new(Input), nil
	}
	typed, ok := input.(Input)
	if !ok {
		return *new(Input), fmt.Errorf("%w for %q", ErrInputType, descriptor.Name)
	}
	return typed, nil
}

func requireSource[Source any](descriptor Descriptor, source any) (Source, error) {
	typed, ok := source.(Source)
	if !ok {
		return *new(Source), fmt.Errorf("%w for %q", ErrSourceType, descriptor.Name)
	}
	return typed, nil
}

type handlerPanic struct{}

func (e *handlerPanic) Error() string { return "handler panic" }

func (d Definition) call(ctx context.Context, source, input any) (output any, err error) {
	defer func() {
		if recover() != nil {
			output = nil
			err = &handlerPanic{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.serial != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-d.serial:
		}
		defer func() { d.serial <- struct{}{} }()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return d.invoke(ctx, source, input)
}

// Registry collects definitions during application startup.
type Registry struct {
	mu            sync.Mutex
	types         schema.Snapshot
	definitions   map[string]Definition
	directives    map[string]registeredDirective
	authorization AuthorizationConfig
	interceptors  []registeredInterceptor
	frozen        bool
}

// Snapshot is an immutable, concurrent-readable registry.
type Snapshot struct {
	types         schema.Snapshot
	definitions   map[string]Definition
	directives    map[string]registeredDirective
	authorization AuthorizationConfig
	interceptors  []registeredInterceptor
}

func NewRegistry(types schema.Snapshot) *Registry {
	return &Registry{types: types, definitions: make(map[string]Definition), directives: builtInDirectives()}
}

// Register validates and records one explicit public definition.
func (r *Registry) Register(definition Definition) error {
	if definition.nilHandler || definition.invoke == nil {
		return errors.New("register nil handler")
	}
	if err := validateRegistration(r.types, definition); err != nil {
		return err
	}
	key := registrationKey(definition.descriptor)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("runtime registry is frozen")
	}
	if _, exists := r.definitions[key]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateRegistration, definition.descriptor.Name)
	}
	r.definitions[key] = definition
	return nil
}

// RegisterDirective validates and records one custom, version-pinned language
// extension. Standard directive names and namespaces are reserved.
func (r *Registry) RegisterDirective(definition DirectiveDefinition) error {
	prepared, err := prepareDirectiveDefinition(r.types, definition)
	if err != nil {
		return err
	}
	definition = prepared
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("runtime registry is frozen")
	}
	if _, exists := r.directives[definition.Descriptor.Name]; exists {
		return fmt.Errorf("%w: directive %q", ErrDuplicateRegistration, definition.Descriptor.Name)
	}
	for _, existing := range r.directives {
		if existing.Descriptor.ID == definition.Descriptor.ID {
			return fmt.Errorf("%w: directive identity %q", ErrDuplicateRegistration, definition.Descriptor.ID)
		}
	}
	r.directives[definition.Descriptor.Name] = registeredDirective{DirectiveDefinition: definition}
	return nil
}

// Freeze returns an immutable snapshot safe for concurrent planning and calls.
func (r *Registry) Freeze() (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, registration := range r.interceptors {
		if !interceptorHasRegisteredTarget(registration, r.definitions) {
			return Snapshot{}, fmt.Errorf("%s interceptor has no registered target", registration.Level)
		}
	}
	definitions := make(map[string]Definition, len(r.definitions))
	for key, definition := range r.definitions {
		definition.descriptor = cloneRuntimeDescriptor(definition.descriptor)
		definitions[key] = definition
	}
	r.frozen = true
	return Snapshot{
		types: r.types, definitions: definitions, directives: cloneRegisteredDirectives(r.directives), authorization: r.authorization,
		interceptors: slices.Clone(r.interceptors),
	}, nil
}

// DirectiveDescriptors returns every supported directive contract in stable
// identity order.
func (s Snapshot) DirectiveDescriptors() []schema.DirectiveDescriptor {
	descriptors := make([]schema.DirectiveDescriptor, 0, len(s.directives))
	for _, definition := range s.directives {
		descriptors = append(descriptors, cloneDirectiveDescriptor(definition.Descriptor))
	}
	sort.Slice(descriptors, func(left, right int) bool { return descriptors[left].ID < descriptors[right].ID })
	return descriptors
}

// Descriptors returns every portable registration descriptor in stable order.
func (s Snapshot) Descriptors() []Descriptor {
	keys := make([]string, 0, len(s.definitions))
	for key := range s.definitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	descriptors := make([]Descriptor, 0, len(keys))
	for _, key := range keys {
		descriptors = append(descriptors, cloneRuntimeDescriptor(s.definitions[key].descriptor))
	}
	return descriptors
}

// Root returns the descriptor for one root operation.
func (s Snapshot) Root(kind protocol.OperationKind, name string) (Descriptor, bool) {
	definition, ok := s.definitions["root:"+name]
	if !ok || definition.descriptor.Kind != kind {
		return Descriptor{}, false
	}
	return cloneRuntimeDescriptor(definition.descriptor), true
}

// Member returns a field or call explicitly registered on an object.
func (s Snapshot) Member(owner schema.TypeID, member MemberKind, name string) (Descriptor, bool) {
	definition, ok := s.definitions["object:"+string(owner)+":"+string(member)+":"+name]
	if !ok {
		return Descriptor{}, false
	}
	return cloneRuntimeDescriptor(definition.descriptor), true
}

// InvokeRoot invokes a registered root operation through its typed adapter.
func (s Snapshot) InvokeRoot(ctx context.Context, kind protocol.OperationKind, name string, input any) (any, error) {
	definition, ok := s.definitions["root:"+name]
	switch {
	case !ok, definition.descriptor.Kind != kind:
		return nil, fmt.Errorf("%w: %s %q", ErrNotRegistered, kind, name)
	default:
		return definition.call(ctx, nil, input)
	}
}

// InvokeField projects one explicitly registered object field.
func (s Snapshot) InvokeField(ctx context.Context, owner schema.TypeID, name string, source any) (any, error) {
	return s.invokeMember(ctx, owner, FieldMember, name, source, nil)
}

// InvokeCall invokes one explicitly registered object method.
func (s Snapshot) InvokeCall(ctx context.Context, owner schema.TypeID, name string, source, input any) (any, error) {
	return s.invokeMember(ctx, owner, CallMember, name, source, input)
}

func (s Snapshot) invokeMember(ctx context.Context, owner schema.TypeID, member MemberKind, name string, source, input any) (any, error) {
	key := "object:" + string(owner) + ":" + string(member) + ":" + name
	definition, ok := s.definitions[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s.%s", ErrNotRegistered, owner, name)
	}
	return definition.call(ctx, source, input)
}

func validateRegistration(types schema.Snapshot, definition Definition) error {
	descriptor := definition.descriptor
	if !namePattern.MatchString(descriptor.Name) {
		return fmt.Errorf("invalid public name %q", descriptor.Name)
	}
	if descriptor.ID != "" && !schemaIdentityPattern.MatchString(descriptor.ID) {
		return fmt.Errorf("invalid portable identity %q", descriptor.ID)
	}
	output, outputOK := types.Lookup(descriptor.Output)
	if !outputOK || !output.Output {
		return fmt.Errorf("registration %q has unknown or non-output type %q", descriptor.Name, descriptor.Output)
	}
	if err := validateBinding(types, definition); err != nil {
		return err
	}
	if err := validateHandlerTypes(types, definition, output); err != nil {
		return fmt.Errorf("registration %q: %w", descriptor.Name, err)
	}
	return validateMetadata(descriptor)
}

func validateBinding(types schema.Snapshot, definition Definition) error {
	descriptor := definition.descriptor
	if descriptor.Member != FieldMember && descriptor.Member != CallMember {
		return fmt.Errorf("registration %q has invalid member kind %q", descriptor.Name, descriptor.Member)
	}
	switch descriptor.Scope {
	case RootScope:
		return validateRootBinding(definition)
	case ObjectScope:
		return validateObjectBinding(types, definition)
	default:
		return fmt.Errorf("registration %q has invalid scope %q", descriptor.Name, descriptor.Scope)
	}
}

func validateRootBinding(definition Definition) error {
	descriptor := definition.descriptor
	if definition.binding != rootBinding || descriptor.Member != CallMember {
		return fmt.Errorf("root registration %q requires Bind and call member kind", descriptor.Name)
	}
	if descriptor.Owner != "" {
		return fmt.Errorf("root registration %q cannot have an owner", descriptor.Name)
	}
	if descriptor.Kind != protocol.Query && descriptor.Kind != protocol.Mutation && descriptor.Kind != protocol.Subscription {
		return fmt.Errorf("root registration %q requires an operation kind", descriptor.Name)
	}
	if descriptor.Input == "" {
		return fmt.Errorf("root registration %q requires an input type", descriptor.Name)
	}
	return nil
}

func validateObjectBinding(types schema.Snapshot, definition Definition) error {
	descriptor := definition.descriptor
	owner, ok := types.Lookup(descriptor.Owner)
	if !ok || (owner.Kind != schema.ObjectType && owner.Kind != schema.InterfaceType) {
		return fmt.Errorf("object registration %q requires a known object owner", descriptor.Name)
	}
	if descriptor.Kind != "" {
		return fmt.Errorf("object registration %q cannot declare a root operation kind", descriptor.Name)
	}
	if descriptor.Member == FieldMember {
		return validateFieldBinding(definition, owner)
	}
	if definition.binding != callBinding || descriptor.Input == "" {
		return fmt.Errorf("object call %q requires BindCall and an input type", descriptor.Name)
	}
	return nil
}

func validateFieldBinding(definition Definition, owner schema.TypeDescriptor) error {
	descriptor := definition.descriptor
	if definition.binding != fieldBinding || descriptor.Input != "" || descriptor.InputNullable {
		return fmt.Errorf("object field %q requires BindField and cannot declare input", descriptor.Name)
	}
	field, ok := owner.Fields[descriptor.Name]
	if !ok {
		return fmt.Errorf("object field %q is not declared by schema %q", descriptor.Name, descriptor.Owner)
	}
	if descriptor.Output != field.Type || descriptor.OutputNullable != field.Nullable {
		return fmt.Errorf("object field %q output does not match schema %q", descriptor.Name, descriptor.Owner)
	}
	return nil
}

func validateHandlerTypes(types schema.Snapshot, definition Definition, output schema.TypeDescriptor) error {
	descriptor := definition.descriptor
	validator := outputTypeValidator{types: types, active: make(map[outputTypeVisit]bool)}
	if err := validator.validate(output, descriptor.OutputNullable, definition.outputType, false); err != nil {
		return err
	}
	if definition.binding != rootBinding && definition.sourceType == nil {
		return errors.New("member handler requires a concrete source type")
	}
	if definition.binding == fieldBinding || definition.adapter {
		return nil
	}
	input, ok := types.Lookup(descriptor.Input)
	if !ok || !input.Input {
		return fmt.Errorf("unknown or non-input type %q", descriptor.Input)
	}
	expected := scalarGoType(descriptor.Input)
	if expected == nil {
		expected = reflect.TypeFor[schema.InputValue]()
	}
	if descriptor.InputNullable {
		expected = reflect.PointerTo(expected)
	}
	if definition.inputType != expected {
		return fmt.Errorf("go input %s does not match schema %s", definition.inputType, descriptor.Input)
	}
	return nil
}

func validateMetadata(descriptor Descriptor) error {
	if err := validateEffectMetadata(descriptor); err != nil {
		return err
	}
	if err := validateSchedulingMetadata(descriptor); err != nil {
		return err
	}
	if descriptor.Metadata.AuthorizationPolicy == "" {
		return fmt.Errorf("registration %q requires authorization policy metadata", descriptor.Name)
	}
	return validatePortableDescriptorMetadata(descriptor)
}

func validatePortableDescriptorMetadata(descriptor Descriptor) error {
	if err := validatePortableDeprecation(descriptor); err != nil {
		return err
	}
	if err := validatePortableCapabilities(descriptor); err != nil {
		return err
	}
	if err := validatePortableTraits(descriptor); err != nil {
		return err
	}
	if descriptor.Source != nil && (descriptor.Source.URI == "" || descriptor.Source.Line < 0 || descriptor.Source.Column < 0) {
		return fmt.Errorf("registration %q has invalid source metadata", descriptor.Name)
	}
	return nil
}

func validatePortableDeprecation(descriptor Descriptor) error {
	if descriptor.Deprecation == nil {
		return nil
	}
	if descriptor.Metadata.Deprecation != "" {
		return fmt.Errorf("registration %q declares both legacy and structured deprecation", descriptor.Name)
	}
	if descriptor.Deprecation.Reason == "" {
		return fmt.Errorf("registration %q deprecation requires a reason", descriptor.Name)
	}
	if descriptor.Deprecation.Sunset != "" {
		if _, err := time.Parse(time.RFC3339, descriptor.Deprecation.Sunset); err != nil {
			return fmt.Errorf("registration %q has invalid deprecation sunset: %w", descriptor.Name, err)
		}
	}
	return nil
}

func validatePortableCapabilities(descriptor Descriptor) error {
	capabilities := slices.Clone(descriptor.Capabilities)
	slices.Sort(capabilities)
	for index, capability := range capabilities {
		duplicate := index > 0 && capabilities[index-1] == capability
		if !schemaIdentityPattern.MatchString(capability) || duplicate {
			return fmt.Errorf("registration %q has invalid or duplicate capability %q", descriptor.Name, capability)
		}
	}
	return nil
}

func validatePortableTraits(descriptor Descriptor) error {
	seenTraits := make(map[string]bool, len(descriptor.Traits))
	for _, trait := range descriptor.Traits {
		if !schemaIdentityPattern.MatchString(trait.ID) || seenTraits[trait.ID] || !json.Valid(trait.Value) {
			return fmt.Errorf("registration %q has invalid or duplicate trait %q", descriptor.Name, trait.ID)
		}
		switch trait.Semantics {
		case schema.TraitDocumentation, schema.TraitValidation, schema.TraitExecution, schema.TraitAuthorization, schema.TraitIdentity:
		default:
			return fmt.Errorf("registration %q has unknown trait semantics %q", descriptor.Name, trait.Semantics)
		}
		seenTraits[trait.ID] = true
	}
	return nil
}

func validateEffectMetadata(descriptor Descriptor) error {
	if descriptor.Kind == protocol.Query && descriptor.Metadata.Effect != ReadEffect {
		return fmt.Errorf("query %q cannot have %s effect", descriptor.Name, descriptor.Metadata.Effect)
	}
	if descriptor.Kind == protocol.Subscription && descriptor.Metadata.Effect != ReadEffect {
		return fmt.Errorf("subscription %q cannot conceal a write effect", descriptor.Name)
	}
	if descriptor.Metadata.Effect != ReadEffect && descriptor.Metadata.Effect != WriteEffect {
		return fmt.Errorf("registration %q requires explicit effect metadata", descriptor.Name)
	}
	if descriptor.Metadata.Cacheable && (descriptor.Metadata.Effect != ReadEffect || !descriptor.Metadata.Deterministic) {
		return fmt.Errorf("registration %q cannot cache a write or nondeterministic handler", descriptor.Name)
	}
	return nil
}

func validateSchedulingMetadata(descriptor Descriptor) error {
	switch descriptor.Metadata.ThreadSafety {
	case ThreadSafe, SerialOnly:
	default:
		return fmt.Errorf("registration %q requires thread-safety metadata", descriptor.Name)
	}
	switch descriptor.Metadata.Batching {
	case BatchEligible, BatchIneligible:
	default:
		return fmt.Errorf("registration %q requires batching metadata", descriptor.Name)
	}
	switch descriptor.Metadata.Transaction {
	case TransactionNone, TransactionOptional, TransactionRequired:
	default:
		return fmt.Errorf("registration %q requires transaction metadata", descriptor.Name)
	}
	if descriptor.Metadata.ParallelMutation && (descriptor.Scope != RootScope || descriptor.Kind != protocol.Mutation || descriptor.Metadata.ThreadSafety != ThreadSafe) {
		return fmt.Errorf("registration %q has impossible parallel-mutation metadata", descriptor.Name)
	}
	return nil
}

type outputTypeVisit struct {
	schemaID schema.TypeID
	goType   reflect.Type
	nullable bool
}

type outputTypeValidator struct {
	types  schema.Snapshot
	active map[outputTypeVisit]bool
}

func (v outputTypeValidator) validate(output schema.TypeDescriptor, nullable bool, actual reflect.Type, dynamic bool) error {
	if dynamic && actual == reflect.TypeFor[any]() {
		return nil
	}
	if actual.Kind() == reflect.Pointer {
		if !nullable {
			return fmt.Errorf("go output %s cannot be a pointer for non-null schema %s", actual, output.ID)
		}
		actual = actual.Elem()
	}
	visit := outputTypeVisit{schemaID: output.ID, goType: actual, nullable: nullable}
	if v.active[visit] {
		return nil
	}
	v.active[visit] = true
	defer delete(v.active, visit)

	if expected := scalarGoType(output.ID); expected != nil {
		return validateExactOutputType(actual, expected, "", output.ID)
	}
	return v.validateComposite(output, actual)
}

func (v outputTypeValidator) validateComposite(output schema.TypeDescriptor, actual reflect.Type) error {
	switch output.Kind {
	case schema.ObjectType:
		return v.validateObject(output, actual)
	case schema.MapType:
		if actual.Kind() != reflect.Map || actual.Key().Kind() != reflect.String {
			return fmt.Errorf("go output %s must be a string-keyed map for schema %s", actual, output.ID)
		}
		return v.validateElement(output, actual.Elem())
	case schema.ListType:
		if actual.Kind() != reflect.Slice && actual.Kind() != reflect.Array {
			return fmt.Errorf("go output %s must be a slice or array for schema %s", actual, output.ID)
		}
		return v.validateElement(output, actual.Elem())
	case schema.EnumType:
		if actual.Kind() != reflect.String {
			return fmt.Errorf("go output %s must be a string for schema %s", actual, output.ID)
		}
		return nil
	case schema.UnionType, schema.InterfaceType:
		return validateExactOutputType(actual, reflect.TypeFor[schema.TaggedValue](), "schema.TaggedValue", output.ID)
	case schema.ScalarType:
		return validateExactOutputType(actual, reflect.TypeFor[json.RawMessage](), "json.RawMessage", output.ID)
	case schema.InputObjectType, schema.OneOfType:
		return fmt.Errorf("schema output %s is not valid for runtime registration", output.ID)
	}
	return fmt.Errorf("schema output %s has an unknown kind", output.ID)
}

func (v outputTypeValidator) validateObject(output schema.TypeDescriptor, actual reflect.Type) error {
	if actual.Kind() != reflect.Map || actual.Key().Kind() != reflect.String {
		return fmt.Errorf("go output %s must be a string-keyed map for schema %s", actual, output.ID)
	}
	for _, field := range output.Fields {
		fieldOutput, ok := v.types.Lookup(field.Type)
		if !ok {
			return fmt.Errorf("schema output %s references unknown field type %s", output.ID, field.Type)
		}
		if err := v.validate(fieldOutput, field.Nullable, actual.Elem(), true); err != nil {
			return fmt.Errorf("go output %s cannot represent every field of schema %s: %w", actual, output.ID, err)
		}
	}
	return nil
}

func (v outputTypeValidator) validateElement(container schema.TypeDescriptor, actual reflect.Type) error {
	element, ok := v.types.Lookup(container.Element)
	if !ok {
		return fmt.Errorf("schema output %s references unknown element type %s", container.ID, container.Element)
	}
	if err := v.validate(element, container.ElementNullable, actual, true); err != nil {
		return fmt.Errorf("go output %s element does not match schema %s: %w", actual, container.ID, err)
	}
	return nil
}

func validateExactOutputType(actual, expected reflect.Type, expectedName string, output schema.TypeID) error {
	if actual == expected {
		return nil
	}
	if expectedName != "" {
		return fmt.Errorf("go output %s must be %s for schema %s", actual, expectedName, output)
	}
	return fmt.Errorf("go output %s does not match schema %s", actual, output)
}

func scalarGoType(id schema.TypeID) reflect.Type {
	switch schema.ScalarKind(id) {
	case schema.Boolean:
		return reflect.TypeFor[bool]()
	case schema.Int32:
		return reflect.TypeFor[int32]()
	case schema.Float64:
		return reflect.TypeFor[float64]()
	case schema.String, schema.ID, schema.Int64, schema.UInt64, schema.BigInt, schema.Decimal, schema.Timestamp, schema.Duration, schema.UUID, schema.Bytes:
		return reflect.TypeFor[string]()
	default:
		return nil
	}
}

func registrationKey(descriptor Descriptor) string {
	if descriptor.Scope == RootScope {
		return "root:" + descriptor.Name
	}
	return "object:" + string(descriptor.Owner) + ":" + string(descriptor.Member) + ":" + descriptor.Name
}
