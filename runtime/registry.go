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
	"sync"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

var (
	ErrNotRegistered         = errors.New("operation is not registered")
	ErrDuplicateRegistration = errors.New("duplicate public registration")
	ErrInputType             = errors.New("handler input has wrong Go type")
	namePattern              = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
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
	Effect              Effect
	Deterministic       bool
	Cacheable           bool
	RetrySafe           bool
	ThreadSafety        ThreadSafety
	Batching            Batching
	Transaction         TransactionParticipation
	AuthorizationPolicy string
	Deprecation         string
	Idempotency         string
	Cost                uint64
	ParallelMutation    bool
}

// Descriptor is the portable public contract for one registered handler.
type Descriptor struct {
	Name           string
	Scope          Scope
	Owner          schema.TypeID
	Kind           protocol.OperationKind
	Member         MemberKind
	Input          schema.TypeID
	Output         schema.TypeID
	OutputNullable bool
	Metadata       Metadata
}

// Handler is the only supported in-process application signature.
type Handler[Input, Output any] func(context.Context, Input) (Output, error)

// Definition binds a descriptor to one typed handler. Its invocation adapter
// is deliberately private so arbitrary functions cannot enter a registry.
type Definition struct {
	descriptor Descriptor
	invoke     func(context.Context, any) (any, error)
	nilHandler bool
	serial     chan struct{}
	inputType  reflect.Type
	outputType reflect.Type
	adapter    bool
}

// Bind adapts a typed handler to an explicit registration definition.
func Bind[Input, Output any](descriptor Descriptor, handler Handler[Input, Output]) Definition {
	definition := Definition{descriptor: descriptor, nilHandler: handler == nil, inputType: reflect.TypeFor[Input](), outputType: reflect.TypeFor[Output]()}
	if descriptor.Metadata.ThreadSafety == SerialOnly {
		definition.serial = make(chan struct{}, 1)
		definition.serial <- struct{}{}
	}
	if handler == nil {
		return definition
	}
	definition.invoke = func(ctx context.Context, input any) (any, error) {
		typed, ok := input.(Input)
		if !ok {
			return nil, fmt.Errorf("%w for %q", ErrInputType, descriptor.Name)
		}
		output, err := handler(ctx, typed)
		if err != nil {
			return nil, err
		}
		return output, nil
	}
	return definition
}

type handlerPanic struct{}

func (e *handlerPanic) Error() string { return "handler panic" }

func (d Definition) call(ctx context.Context, input any) (output any, err error) {
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
	return d.invoke(ctx, input)
}

// Registry collects definitions during application startup.
type Registry struct {
	mu          sync.Mutex
	types       schema.Snapshot
	definitions map[string]Definition
	frozen      bool
}

// Snapshot is an immutable, concurrent-readable registry.
type Snapshot struct {
	types       schema.Snapshot
	definitions map[string]Definition
}

func NewRegistry(types schema.Snapshot) *Registry {
	return &Registry{types: types, definitions: make(map[string]Definition)}
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

// Freeze returns an immutable snapshot safe for concurrent planning and calls.
func (r *Registry) Freeze() (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	definitions := make(map[string]Definition, len(r.definitions))
	for key, definition := range r.definitions {
		definitions[key] = definition
	}
	r.frozen = true
	return Snapshot{types: r.types, definitions: definitions}, nil
}

// Root returns the descriptor for one root operation.
func (s Snapshot) Root(kind protocol.OperationKind, name string) (Descriptor, bool) {
	definition, ok := s.definitions["root:"+name]
	if !ok || definition.descriptor.Kind != kind {
		return Descriptor{}, false
	}
	return definition.descriptor, true
}

// Member returns a field or call explicitly registered on an object.
func (s Snapshot) Member(owner schema.TypeID, member MemberKind, name string) (Descriptor, bool) {
	definition, ok := s.definitions["object:"+string(owner)+":"+string(member)+":"+name]
	if !ok {
		return Descriptor{}, false
	}
	return definition.descriptor, true
}

// InvokeRoot invokes a registered root operation through its typed adapter.
func (s Snapshot) InvokeRoot(ctx context.Context, kind protocol.OperationKind, name string, input any) (any, error) {
	definition, ok := s.definitions["root:"+name]
	if !ok || definition.descriptor.Kind != kind {
		return nil, fmt.Errorf("%w: %s %q", ErrNotRegistered, kind, name)
	}
	return definition.call(ctx, input)
}

// InvokeMember invokes an explicitly registered object field or call.
func (s Snapshot) InvokeMember(ctx context.Context, owner schema.TypeID, member MemberKind, name string, input any) (any, error) {
	definition, ok := s.definitions["object:"+string(owner)+":"+string(member)+":"+name]
	if !ok {
		return nil, fmt.Errorf("%w: %s.%s", ErrNotRegistered, owner, name)
	}
	return definition.call(ctx, input)
}

func validateRegistration(types schema.Snapshot, definition Definition) error {
	descriptor := definition.descriptor
	if !namePattern.MatchString(descriptor.Name) {
		return fmt.Errorf("invalid public name %q", descriptor.Name)
	}
	input, inputOK := types.Lookup(descriptor.Input)
	if !inputOK || !input.Input {
		return fmt.Errorf("registration %q has unknown or non-input type %q", descriptor.Name, descriptor.Input)
	}
	output, outputOK := types.Lookup(descriptor.Output)
	if !outputOK || !output.Output {
		return fmt.Errorf("registration %q has unknown or non-output type %q", descriptor.Name, descriptor.Output)
	}
	if !definition.adapter {
		if expected := scalarGoType(descriptor.Input); expected != nil && definition.inputType != expected {
			return fmt.Errorf("registration %q Go input %s does not match schema %s", descriptor.Name, definition.inputType, descriptor.Input)
		}
	}
	if err := validateOutputGoType(descriptor, output, definition.outputType); err != nil {
		return fmt.Errorf("registration %q: %w", descriptor.Name, err)
	}
	if descriptor.Member != FieldMember && descriptor.Member != CallMember {
		return fmt.Errorf("registration %q has invalid member kind %q", descriptor.Name, descriptor.Member)
	}
	switch descriptor.Scope {
	case RootScope:
		if descriptor.Owner != "" {
			return fmt.Errorf("root registration %q cannot have an owner", descriptor.Name)
		}
		if descriptor.Kind != protocol.Query && descriptor.Kind != protocol.Mutation && descriptor.Kind != protocol.Subscription {
			return fmt.Errorf("root registration %q requires an operation kind", descriptor.Name)
		}
	case ObjectScope:
		owner, ok := types.Lookup(descriptor.Owner)
		if !ok || (owner.Kind != schema.ObjectType && owner.Kind != schema.InterfaceType) {
			return fmt.Errorf("object registration %q requires a known object owner", descriptor.Name)
		}
		if descriptor.Kind != "" {
			return fmt.Errorf("object registration %q cannot declare a root operation kind", descriptor.Name)
		}
	default:
		return fmt.Errorf("registration %q has invalid scope %q", descriptor.Name, descriptor.Scope)
	}
	if descriptor.Kind == protocol.Query && descriptor.Metadata.Effect != ReadEffect {
		return fmt.Errorf("query %q cannot have %s effect", descriptor.Name, descriptor.Metadata.Effect)
	}
	if descriptor.Kind == protocol.Subscription && descriptor.Metadata.Effect != ReadEffect {
		return fmt.Errorf("subscription %q cannot conceal a write effect", descriptor.Name)
	}
	if descriptor.Metadata.Effect != ReadEffect && descriptor.Metadata.Effect != WriteEffect {
		return fmt.Errorf("registration %q requires explicit effect metadata", descriptor.Name)
	}
	if descriptor.Metadata.ThreadSafety != ThreadSafe && descriptor.Metadata.ThreadSafety != SerialOnly {
		return fmt.Errorf("registration %q requires thread-safety metadata", descriptor.Name)
	}
	if descriptor.Metadata.Batching != BatchEligible && descriptor.Metadata.Batching != BatchIneligible {
		return fmt.Errorf("registration %q requires batching metadata", descriptor.Name)
	}
	if descriptor.Metadata.Transaction != TransactionNone && descriptor.Metadata.Transaction != TransactionOptional && descriptor.Metadata.Transaction != TransactionRequired {
		return fmt.Errorf("registration %q requires transaction metadata", descriptor.Name)
	}
	if descriptor.Metadata.AuthorizationPolicy == "" {
		return fmt.Errorf("registration %q requires authorization policy metadata", descriptor.Name)
	}
	return nil
}

func validateOutputGoType(descriptor Descriptor, output schema.TypeDescriptor, actual reflect.Type) error {
	if expected := scalarGoType(descriptor.Output); expected != nil {
		return validateExactOutputType(descriptor, actual, expected, "")
	}
	switch output.Kind {
	case schema.ObjectType, schema.MapType:
		if actual.Kind() != reflect.Map || actual.Key().Kind() != reflect.String {
			return fmt.Errorf("go output %s must be a string-keyed map for schema %s", actual, descriptor.Output)
		}
		return nil
	case schema.ListType:
		if actual.Kind() != reflect.Slice && actual.Kind() != reflect.Array {
			return fmt.Errorf("go output %s must be a slice or array for schema %s", actual, descriptor.Output)
		}
		return nil
	case schema.EnumType:
		if actual.Kind() != reflect.String {
			return fmt.Errorf("go output %s must be a string for schema %s", actual, descriptor.Output)
		}
		return nil
	case schema.UnionType, schema.InterfaceType:
		return validateExactOutputType(descriptor, actual, reflect.TypeFor[schema.TaggedValue](), "schema.TaggedValue")
	case schema.ScalarType:
		return validateExactOutputType(descriptor, actual, reflect.TypeFor[json.RawMessage](), "json.RawMessage")
	case schema.InputObjectType, schema.OneOfType:
		return fmt.Errorf("schema output %s is not valid for runtime registration", descriptor.Output)
	}
	return fmt.Errorf("schema output %s has an unknown kind", descriptor.Output)
}

func validateExactOutputType(descriptor Descriptor, actual, expected reflect.Type, expectedName string) error {
	if actual == expected || (descriptor.OutputNullable && actual.Kind() == reflect.Pointer && actual.Elem() == expected) {
		return nil
	}
	if expectedName != "" {
		return fmt.Errorf("go output %s must be %s for schema %s", actual, expectedName, descriptor.Output)
	}
	return fmt.Errorf("go output %s does not match schema %s", actual, descriptor.Output)
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
