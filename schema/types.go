package schema

import (
	"errors"
	"fmt"
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
	Type       TypeID
	Required   bool
	Nullable   bool
	Deprecated string
}

// TypeDescriptor is the language-neutral description of one named type.
type TypeDescriptor struct {
	ID      TypeID
	Kind    TypeKind
	Input   bool
	Output  bool
	Open    bool
	Element TypeID
	// ElementNullable distinguishes a nullable container element from the
	// default non-null element contract. It is meaningful only for lists and
	// maps.
	ElementNullable bool
	Fields          map[string]FieldDescriptor
	Variants        []TypeID
	EnumValues      []string
	MaxDepth        int
}

// Catalog collects type declarations during startup.
type Catalog struct {
	mu     sync.Mutex
	types  map[TypeID]TypeDescriptor
	frozen bool
}

// Snapshot is an immutable, concurrent-readable type catalog.
type Snapshot struct {
	types map[TypeID]TypeDescriptor
}

// NewCatalog creates a catalog containing every core scalar.
func NewCatalog() *Catalog {
	types := make(map[TypeID]TypeDescriptor)
	for _, scalar := range []ScalarKind{Boolean, String, ID, Int32, Float64, Int64, UInt64, BigInt, Decimal, Timestamp, Duration, UUID, Bytes} {
		identifier := TypeID(scalar)
		types[identifier] = TypeDescriptor{ID: identifier, Kind: ScalarType, Input: true, Output: true}
	}
	return &Catalog{types: types}
}

// Register adds one explicit named type declaration.
func (c *Catalog) Register(descriptor TypeDescriptor) error {
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
	return nil
}

// Freeze resolves all references and returns an isolated immutable snapshot.
func (c *Catalog) Freeze() (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, descriptor := range c.types {
		if descriptor.Element != "" {
			reference, ok := c.types[descriptor.Element]
			if !ok {
				return Snapshot{}, fmt.Errorf("schema type %q references unknown element type %q", descriptor.ID, descriptor.Element)
			}
			if descriptor.Input && !reference.Input {
				return Snapshot{}, fmt.Errorf("input container %q references non-input element %q", descriptor.ID, descriptor.Element)
			}
			if descriptor.Output && !reference.Output {
				return Snapshot{}, fmt.Errorf("output container %q references non-output element %q", descriptor.ID, descriptor.Element)
			}
		}
		for name, field := range descriptor.Fields {
			reference, ok := c.types[field.Type]
			if !ok {
				return Snapshot{}, fmt.Errorf("schema type %q field %q references unknown type %q", descriptor.ID, name, field.Type)
			}
			if descriptor.Kind == InputObjectType || descriptor.Kind == OneOfType {
				if !reference.Input {
					return Snapshot{}, fmt.Errorf("input type %q field %q references non-input type %q", descriptor.ID, name, field.Type)
				}
			} else if !reference.Output {
				return Snapshot{}, fmt.Errorf("output type %q field %q references non-output type %q", descriptor.ID, name, field.Type)
			}
		}
		for _, variant := range descriptor.Variants {
			reference, ok := c.types[variant]
			if !ok {
				return Snapshot{}, fmt.Errorf("schema type %q references unknown variant %q", descriptor.ID, variant)
			}
			if descriptor.Kind == UnionType && !reference.Output {
				return Snapshot{}, fmt.Errorf("union %q references non-output variant %q", descriptor.ID, variant)
			}
		}
	}
	if err := requireBoundedRecursion(c.types); err != nil {
		return Snapshot{}, err
	}
	c.frozen = true
	return Snapshot{types: cloneTypes(c.types)}, nil
}

// Lookup returns an isolated descriptor copy.
func (s Snapshot) Lookup(id TypeID) (TypeDescriptor, bool) {
	descriptor, ok := s.types[id]
	return cloneDescriptor(descriptor), ok
}

// Descriptors returns all types sorted by identifier.
func (s Snapshot) Descriptors() []TypeDescriptor {
	identifiers := make([]string, 0, len(s.types))
	for identifier := range s.types {
		identifiers = append(identifiers, string(identifier))
	}
	slices.Sort(identifiers)
	result := make([]TypeDescriptor, 0, len(identifiers))
	for _, identifier := range identifiers {
		result = append(result, cloneDescriptor(s.types[TypeID(identifier)]))
	}
	return result
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
	switch descriptor.Kind {
	case ScalarType:
		if descriptor.Element != "" || len(descriptor.Fields) != 0 || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 {
			return fmt.Errorf("scalar type %q has composite members", descriptor.ID)
		}
	case ObjectType, InterfaceType, InputObjectType:
		if len(descriptor.Fields) == 0 || descriptor.Element != "" || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 {
			return fmt.Errorf("%s type %q requires fields", descriptor.Kind, descriptor.ID)
		}
	case ListType, MapType:
		if descriptor.Element == "" || len(descriptor.Fields) != 0 || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 {
			return fmt.Errorf("%s type %q requires an element type", descriptor.Kind, descriptor.ID)
		}
	case EnumType:
		if len(descriptor.EnumValues) == 0 || len(descriptor.Variants) != 0 || descriptor.Element != "" || len(descriptor.Fields) != 0 {
			return fmt.Errorf("enum type %q requires values", descriptor.ID)
		}
		seen := make(map[string]bool, len(descriptor.EnumValues))
		for _, value := range descriptor.EnumValues {
			if !typeIDPattern.MatchString(value) || seen[value] {
				return fmt.Errorf("enum type %q has invalid or duplicate value %q", descriptor.ID, value)
			}
			seen[value] = true
		}
	case UnionType:
		if len(descriptor.Variants) == 0 || !descriptor.Output || descriptor.Element != "" || len(descriptor.Fields) != 0 || len(descriptor.EnumValues) != 0 {
			return fmt.Errorf("union type %q requires output variants", descriptor.ID)
		}
	case OneOfType:
		if !descriptor.Input || descriptor.Output || len(descriptor.Fields) == 0 || descriptor.Element != "" || len(descriptor.Variants) != 0 || len(descriptor.EnumValues) != 0 {
			return fmt.Errorf("one-of type %q must be input-only with members", descriptor.ID)
		}
	default:
		return fmt.Errorf("schema type %q has unknown kind %q", descriptor.ID, descriptor.Kind)
	}
	if descriptor.Kind == ObjectType && descriptor.Input {
		return fmt.Errorf("object type %q is output-only", descriptor.ID)
	}
	if descriptor.Kind == InterfaceType && descriptor.Input {
		return fmt.Errorf("interface type %q is output-only", descriptor.ID)
	}
	if descriptor.Kind == InputObjectType && (!descriptor.Input || descriptor.Output) {
		return fmt.Errorf("input object %q must be input-only", descriptor.ID)
	}
	for name, field := range descriptor.Fields {
		if !typeIDPattern.MatchString(name) || field.Type == "" {
			return fmt.Errorf("schema type %q has invalid field %q", descriptor.ID, name)
		}
	}
	return nil
}

func cloneTypes(input map[TypeID]TypeDescriptor) map[TypeID]TypeDescriptor {
	result := make(map[TypeID]TypeDescriptor, len(input))
	for identifier, descriptor := range input {
		result[identifier] = cloneDescriptor(descriptor)
	}
	return result
}

func cloneDescriptor(descriptor TypeDescriptor) TypeDescriptor {
	result := descriptor
	result.Fields = make(map[string]FieldDescriptor, len(descriptor.Fields))
	for name, field := range descriptor.Fields {
		result.Fields[name] = field
	}
	result.Variants = append([]TypeID(nil), descriptor.Variants...)
	result.EnumValues = append([]string(nil), descriptor.EnumValues...)
	return result
}

func requireBoundedRecursion(types map[TypeID]TypeDescriptor) error {
	for start, descriptor := range types {
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
	return refs
}
