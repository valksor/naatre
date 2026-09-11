package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync"
)

// TypeID is a portable schema type identifier.
type TypeID string

// TypeKind identifies a scalar or composite schema form.
type TypeKind string

const (
	ScalarType      TypeKind = "scalar"
	ObjectType      TypeKind = "object"
	InputObjectType TypeKind = "input-object"
	ListType        TypeKind = "list"
	MapType         TypeKind = "map"
	EnumType        TypeKind = "enum"
	UnionType       TypeKind = "union"
	InterfaceType   TypeKind = "interface"
	OneOfType       TypeKind = "oneof"
)

var typeIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// FieldDescriptor defines one object or one-of member.
type FieldDescriptor struct {
	Type       TypeID          `json:"type"`
	Required   bool            `json:"required,omitempty"`
	Nullable   bool            `json:"nullable,omitempty"`
	Default    json.RawMessage `json:"default,omitempty"`
	Deprecated string          `json:"deprecated,omitempty"`
}

// JSONShape names a language-neutral JSON wire shape accepted by a custom
// scalar contract.
type JSONShape string

const (
	JSONBoolean JSONShape = "boolean"
	JSONString  JSONShape = "string"
	JSONNumber  JSONShape = "number"
	JSONArray   JSONShape = "array"
	JSONObject  JSONShape = "object"
)

// ScalarLimits bounds custom scalar inputs independently of host-language
// implementations. Zero values select the protocol defaults.
type ScalarLimits struct {
	MaxBytes       int `json:"maxBytes,omitempty"`
	MaxDepth       int `json:"maxDepth,omitempty"`
	MaxMembers     int `json:"maxMembers,omitempty"`
	MaxArrayItems  int `json:"maxArrayItems,omitempty"`
	MaxStringBytes int `json:"maxStringBytes,omitempty"`
	MaxNumberBytes int `json:"maxNumberBytes,omitempty"`
	MaxTokens      int `json:"maxTokens,omitempty"`
}

// ScalarConformanceVector fixes an input and canonical output that every
// implementation of a custom scalar must reproduce.
type ScalarConformanceVector struct {
	Input     json.RawMessage `json:"input"`
	Canonical json.RawMessage `json:"canonical"`
}

// ScalarDescriptor is the portable contract for a custom scalar. Identifier
// fields name specifications or algorithms rather than Go implementations.
type ScalarDescriptor struct {
	AcceptedWireShapes []JSONShape               `json:"acceptedWireShapes"`
	Validator          string                    `json:"validator"`
	Serializer         string                    `json:"serializer"`
	Canonicalizer      string                    `json:"canonicalizer"`
	CanonicalProfile   string                    `json:"canonicalProfile"`
	Limits             ScalarLimits              `json:"limits"`
	Conformance        []ScalarConformanceVector `json:"conformance"`
}

// TypeDescriptor is the language-neutral description of one named type.
type TypeDescriptor struct {
	ID      TypeID   `json:"id"`
	Kind    TypeKind `json:"kind"`
	Input   bool     `json:"input,omitempty"`
	Output  bool     `json:"output,omitempty"`
	Open    bool     `json:"open,omitempty"`
	Element TypeID   `json:"element,omitempty"`
	// ElementNullable distinguishes a nullable container element from the
	// default non-null element contract. It is meaningful only for lists and
	// maps.
	ElementNullable bool                       `json:"elementNullable,omitempty"`
	Fields          map[string]FieldDescriptor `json:"fields,omitempty"`
	Variants        []TypeID                   `json:"variants,omitempty"`
	EnumValues      []string                   `json:"enumValues,omitempty"`
	MaxDepth        int                        `json:"maxDepth,omitempty"`
	Scalar          *ScalarDescriptor          `json:"scalar,omitempty"`
}

// Catalog collects type declarations during startup.
type Catalog struct {
	mu      sync.Mutex
	types   map[TypeID]TypeDescriptor
	scalars map[TypeID]scalarRuntime
	frozen  bool
}

// Snapshot is an immutable, concurrent-readable type catalog.
type Snapshot struct {
	types   map[TypeID]TypeDescriptor
	scalars map[TypeID]scalarRuntime
}

// NewCatalog creates a catalog containing every core scalar.
func NewCatalog() *Catalog {
	types := make(map[TypeID]TypeDescriptor)
	for _, scalar := range []ScalarKind{Boolean, String, ID, Int32, Float64, Int64, UInt64, BigInt, Decimal, Timestamp, Duration, UUID, Bytes} {
		identifier := TypeID(scalar)
		types[identifier] = TypeDescriptor{ID: identifier, Kind: ScalarType, Input: true, Output: true}
	}
	return &Catalog{types: types, scalars: make(map[TypeID]scalarRuntime)}
}

// Register adds one explicit named type declaration.
func (c *Catalog) Register(descriptor TypeDescriptor) error {
	return c.register(descriptor, nil)
}

// RegisterScalar adds one custom scalar declaration. The reference runtime
// accepts only deterministic language-neutral profiles it implements itself;
// no host callback participates in canonicalization or hashing.
func (c *Catalog) RegisterScalar(descriptor TypeDescriptor) error {
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	runtime, err := newScalarRuntime(descriptor)
	if err != nil {
		return err
	}
	return c.register(descriptor, &runtime)
}

func (c *Catalog) register(descriptor TypeDescriptor, scalar *scalarRuntime) error {
	if descriptor.Kind == ScalarType && scalar == nil {
		return fmt.Errorf("custom scalar %q requires RegisterScalar", descriptor.ID)
	}
	if descriptor.Kind != ScalarType && scalar != nil {
		return fmt.Errorf("schema type %q cannot register a scalar profile", descriptor.ID)
	}
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return errors.New("schema catalog is frozen")
	}
	if _, exists := c.types[descriptor.ID]; exists {
		return fmt.Errorf("schema type %q is already registered", descriptor.ID)
	}
	c.types[descriptor.ID] = cloneDescriptor(descriptor)
	if scalar != nil {
		c.scalars[descriptor.ID] = *scalar
	}
	return nil
}

// Freeze resolves all references and returns an isolated immutable snapshot.
func (c *Catalog) Freeze() (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateCatalogReferences(c.types); err != nil {
		return Snapshot{}, err
	}
	if err := requireBoundedRecursion(c.types); err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{types: cloneTypeMap(c.types), scalars: maps.Clone(c.scalars)}
	if err := validateInputDefaults(snapshot); err != nil {
		return Snapshot{}, err
	}
	c.frozen = true
	return snapshot, nil
}

// Lookup returns an isolated descriptor copy.
func (s Snapshot) Lookup(id TypeID) (TypeDescriptor, bool) {
	descriptor, ok := s.types[id]
	return cloneDescriptor(descriptor), ok
}

// Descriptors returns all types sorted by identifier.
func (s Snapshot) Descriptors() []TypeDescriptor {
	identifiers := sortedKeys(s.types)
	result := make([]TypeDescriptor, 0, len(identifiers))
	for _, identifier := range identifiers {
		result = append(result, cloneDescriptor(s.types[identifier]))
	}
	return result
}

// ResolveVariant applies the open or closed compatibility rule for one union
// or interface runtime type tag. A false known result is possible only for an
// open union and preserves the unknown tag for forward-compatible clients.
func (s Snapshot) ResolveVariant(id, variant TypeID) (descriptor TypeDescriptor, known bool, err error) {
	parent, ok := s.types[id]
	if !ok || (parent.Kind != UnionType && parent.Kind != InterfaceType) {
		return TypeDescriptor{}, false, fmt.Errorf("type %q is not a union or interface", id)
	}
	if slices.Contains(parent.Variants, variant) {
		resolved, ok := s.types[variant]
		if !ok {
			return TypeDescriptor{}, false, fmt.Errorf("type %q has unresolved variant %q", id, variant)
		}
		return cloneDescriptor(resolved), true, nil
	}
	if parent.Kind == UnionType && parent.Open {
		return TypeDescriptor{}, false, nil
	}
	return TypeDescriptor{}, false, fmt.Errorf("closed %s %q rejects variant %q", parent.Kind, id, variant)
}

func validateDescriptor(descriptor TypeDescriptor) error {
	if !typeIDPattern.MatchString(string(descriptor.ID)) {
		return fmt.Errorf("invalid schema type identifier %q", descriptor.ID)
	}
	if !descriptor.Input && !descriptor.Output {
		return fmt.Errorf("schema type %q has no input or output position", descriptor.ID)
	}
	if descriptor.MaxDepth < 0 {
		return fmt.Errorf("schema type %q has negative maximum depth", descriptor.ID)
	}
	if descriptor.Open && descriptor.Kind != EnumType && descriptor.Kind != UnionType {
		return fmt.Errorf("%s type %q cannot be open", descriptor.Kind, descriptor.ID)
	}
	if descriptor.ElementNullable && descriptor.Kind != ListType && descriptor.Kind != MapType {
		return fmt.Errorf("%s type %q cannot declare element nullability", descriptor.Kind, descriptor.ID)
	}
	if err := validateDescriptorShape(descriptor); err != nil {
		return err
	}
	if err := validateDescriptorPosition(descriptor); err != nil {
		return err
	}
	if err := validateFieldDescriptors(descriptor); err != nil {
		return err
	}
	return validateVariantIDs(descriptor)
}

func validateDescriptorShape(descriptor TypeDescriptor) error {
	switch descriptor.Kind {
	case ScalarType:
		if descriptor.Element != "" || len(descriptor.Fields) != 0 || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 || descriptor.Scalar == nil {
			return fmt.Errorf("scalar type %q has composite members", descriptor.ID)
		}
	case ObjectType, InputObjectType:
		if len(descriptor.Fields) == 0 || descriptor.Element != "" || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 || descriptor.Scalar != nil {
			return fmt.Errorf("%s type %q requires fields", descriptor.Kind, descriptor.ID)
		}
	case InterfaceType:
		if len(descriptor.Fields) == 0 || len(descriptor.Variants) == 0 || descriptor.Element != "" || len(descriptor.EnumValues) != 0 || descriptor.Scalar != nil {
			return fmt.Errorf("interface type %q requires fields and object variants", descriptor.ID)
		}
	case ListType, MapType:
		if descriptor.Element == "" || len(descriptor.Fields) != 0 || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 || descriptor.Scalar != nil {
			return fmt.Errorf("%s type %q requires an element type", descriptor.Kind, descriptor.ID)
		}
	case EnumType:
		if len(descriptor.EnumValues) == 0 || len(descriptor.Variants) != 0 || descriptor.Element != "" || len(descriptor.Fields) != 0 || descriptor.Scalar != nil {
			return fmt.Errorf("enum type %q requires values", descriptor.ID)
		}
		return validateEnumValues(descriptor)
	case UnionType:
		if len(descriptor.Variants) == 0 || descriptor.Input || !descriptor.Output || descriptor.Element != "" || len(descriptor.Fields) != 0 || len(descriptor.EnumValues) != 0 || descriptor.Scalar != nil {
			return fmt.Errorf("union type %q requires output variants", descriptor.ID)
		}
	case OneOfType:
		if !descriptor.Input || descriptor.Output || len(descriptor.Fields) == 0 || descriptor.Element != "" || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 || descriptor.Scalar != nil {
			return fmt.Errorf("one-of type %q must be input-only with members", descriptor.ID)
		}
	default:
		return fmt.Errorf("schema type %q has unknown kind %q", descriptor.ID, descriptor.Kind)
	}
	return nil
}

func validateDescriptorPosition(descriptor TypeDescriptor) error {
	if descriptor.Kind == ObjectType && descriptor.Input {
		return fmt.Errorf("object type %q is output-only", descriptor.ID)
	}
	if descriptor.Kind == InterfaceType && descriptor.Input {
		return fmt.Errorf("interface type %q is output-only", descriptor.ID)
	}
	if descriptor.Kind == InputObjectType && (!descriptor.Input || descriptor.Output) {
		return fmt.Errorf("input object %q must be input-only", descriptor.ID)
	}
	return nil
}

func validateFieldDescriptors(descriptor TypeDescriptor) error {
	for _, name := range sortedKeys(descriptor.Fields) {
		field := descriptor.Fields[name]
		if !typeIDPattern.MatchString(name) || field.Type == "" {
			return fmt.Errorf("schema type %q has invalid field %q", descriptor.ID, name)
		}
		if descriptor.Kind == OneOfType && field.Required {
			return fmt.Errorf("one-of type %q member %q cannot be required", descriptor.ID, name)
		}
		if field.Default != nil && descriptor.Kind != InputObjectType && descriptor.Kind != OneOfType {
			return fmt.Errorf("schema type %q output field %q cannot declare an input default", descriptor.ID, name)
		}
	}
	return nil
}

func validateVariantIDs(descriptor TypeDescriptor) error {
	seenVariants := make(map[TypeID]bool, len(descriptor.Variants))
	for _, variant := range descriptor.Variants {
		if !typeIDPattern.MatchString(string(variant)) || seenVariants[variant] {
			return fmt.Errorf("schema type %q has invalid or duplicate variant %q", descriptor.ID, variant)
		}
		seenVariants[variant] = true
	}
	return nil
}

func validateEnumValues(descriptor TypeDescriptor) error {
	seen := make(map[string]bool, len(descriptor.EnumValues))
	for _, value := range descriptor.EnumValues {
		if !typeIDPattern.MatchString(value) || seen[value] {
			return fmt.Errorf("enum type %q has invalid or duplicate value %q", descriptor.ID, value)
		}
		seen[value] = true
	}
	return nil
}

func validateCatalogReferences(types map[TypeID]TypeDescriptor) error {
	for _, identifier := range sortedKeys(types) {
		descriptor := types[identifier]
		if err := validateElementReference(types, descriptor); err != nil {
			return err
		}
		if err := validateFieldReferences(types, descriptor); err != nil {
			return err
		}
		if err := validateVariantReferences(types, descriptor); err != nil {
			return err
		}
	}
	return nil
}

func validateElementReference(types map[TypeID]TypeDescriptor, descriptor TypeDescriptor) error {
	if descriptor.Element == "" {
		return nil
	}
	reference, ok := types[descriptor.Element]
	if !ok {
		return fmt.Errorf("schema type %q references unknown element type %q", descriptor.ID, descriptor.Element)
	}
	if descriptor.Input && !reference.Input {
		return fmt.Errorf("input container %q references non-input element %q", descriptor.ID, descriptor.Element)
	}
	if descriptor.Output && !reference.Output {
		return fmt.Errorf("output container %q references non-output element %q", descriptor.ID, descriptor.Element)
	}
	return nil
}

func validateFieldReferences(types map[TypeID]TypeDescriptor, descriptor TypeDescriptor) error {
	names := make([]string, 0, len(descriptor.Fields))
	for name := range descriptor.Fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		field := descriptor.Fields[name]
		reference, ok := types[field.Type]
		if !ok {
			return fmt.Errorf("schema type %q field %q references unknown type %q", descriptor.ID, name, field.Type)
		}
		inputField := descriptor.Kind == InputObjectType || descriptor.Kind == OneOfType
		if inputField && !reference.Input {
			return fmt.Errorf("input type %q field %q references non-input type %q", descriptor.ID, name, field.Type)
		}
		if !inputField && !reference.Output {
			return fmt.Errorf("output type %q field %q references non-output type %q", descriptor.ID, name, field.Type)
		}
	}
	return nil
}

func validateVariantReferences(types map[TypeID]TypeDescriptor, descriptor TypeDescriptor) error {
	for _, variant := range descriptor.Variants {
		reference, ok := types[variant]
		if !ok {
			return fmt.Errorf("schema type %q references unknown variant %q", descriptor.ID, variant)
		}
		if !reference.Output || reference.Kind != ObjectType {
			return fmt.Errorf("%s %q requires output object variant %q", descriptor.Kind, descriptor.ID, variant)
		}
		if descriptor.Kind == InterfaceType {
			if err := validateInterfaceVariant(descriptor, reference); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateInterfaceVariant(contract, implementation TypeDescriptor) error {
	for _, name := range sortedKeys(contract.Fields) {
		field := contract.Fields[name]
		actual, exists := implementation.Fields[name]
		if !exists || actual.Type != field.Type || actual.Required != field.Required || actual.Nullable != field.Nullable {
			return fmt.Errorf("interface %q variant %q does not implement field %q", contract.ID, implementation.ID, name)
		}
	}
	return nil
}

func sortedKeys[K ~string, V any](values map[K]V) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func cloneDescriptor(descriptor TypeDescriptor) TypeDescriptor {
	result := descriptor
	result.Fields = make(map[string]FieldDescriptor, len(descriptor.Fields))
	for name, field := range descriptor.Fields {
		field.Default = append(json.RawMessage(nil), field.Default...)
		result.Fields[name] = field
	}
	result.Variants = append([]TypeID(nil), descriptor.Variants...)
	result.EnumValues = append([]string(nil), descriptor.EnumValues...)
	if descriptor.Scalar != nil {
		scalar := *descriptor.Scalar
		scalar.AcceptedWireShapes = append([]JSONShape(nil), descriptor.Scalar.AcceptedWireShapes...)
		scalar.Conformance = make([]ScalarConformanceVector, len(descriptor.Scalar.Conformance))
		for index, vector := range descriptor.Scalar.Conformance {
			scalar.Conformance[index] = ScalarConformanceVector{
				Input:     append(json.RawMessage(nil), vector.Input...),
				Canonical: append(json.RawMessage(nil), vector.Canonical...),
			}
		}
		result.Scalar = &scalar
	}
	return result
}

func cloneTypeMap(input map[TypeID]TypeDescriptor) map[TypeID]TypeDescriptor {
	result := maps.Clone(input)
	for identifier := range result {
		result[identifier] = cloneDescriptor(result[identifier])
	}
	return result
}

func requireBoundedRecursion(types map[TypeID]TypeDescriptor) error {
	for _, start := range sortedKeys(types) {
		descriptor := types[start]
		if descriptor.MaxDepth > 0 {
			continue
		}
		seen := make(map[TypeID]bool)
		var reaches func(TypeID) bool
		reaches = func(id TypeID) bool {
			if seen[id] {
				return false
			}
			seen[id] = true
			for _, ref := range typeReferences(types[id]) {
				if ref == start || reaches(ref) {
					return true
				}
			}
			return false
		}
		if reaches(start) {
			return fmt.Errorf("recursive schema type %q requires positive maximum depth", start)
		}
	}
	return nil
}

func typeReferences(descriptor TypeDescriptor) []TypeID {
	refs := append([]TypeID(nil), descriptor.Variants...)
	if descriptor.Element != "" {
		refs = append(refs, descriptor.Element)
	}
	for _, field := range descriptor.Fields {
		refs = append(refs, field.Type)
	}
	slices.Sort(refs)
	return refs
}
