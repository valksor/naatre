package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

// InputValue is an immutable, schema-directed input value. It preserves
// missing, null, scalar, list, object, enum, and one-of state without relying
// on host-language numeric coercion.
type InputValue struct {
	typeID       TypeID
	presence     Presence
	scalar       *Value
	list         []InputValue
	object       map[string]InputValue
	enum         string
	enumKnown    bool
	activeMember string
}

func (v InputValue) Type() TypeID         { return v.typeID }
func (v InputValue) Presence() Presence   { return v.presence }
func (v InputValue) IsMissing() bool      { return v.presence == PresenceMissing }
func (v InputValue) IsNull() bool         { return v.presence == PresenceNull }
func (v InputValue) ActiveMember() string { return v.activeMember }

func (v InputValue) Scalar() (Value, bool) {
	if v.scalar == nil {
		return Value{}, false
	}
	return *v.scalar, true
}

func (v InputValue) List() ([]InputValue, bool) {
	if v.list == nil {
		return nil, false
	}
	return slices.Clone(v.list), true
}

func (v InputValue) Object() (map[string]InputValue, bool) {
	if v.object == nil {
		return nil, false
	}
	return maps.Clone(v.object), true
}

// Enum returns the enum spelling, whether the value is declared by the
// schema, and whether this InputValue is an enum.
func (v InputValue) Enum() (value string, known bool, ok bool) {
	if v.presence != PresenceValue || v.enum == "" {
		return "", false, false
	}
	return v.enum, v.enumKnown, true
}

// MarshalJSON returns deterministic schema-directed JSON. Missing has no wire
// representation and therefore returns ErrMissingValue.
func (v InputValue) MarshalJSON() ([]byte, error) {
	if v.IsMissing() {
		return nil, ErrMissingValue
	}
	if v.IsNull() {
		return []byte("null"), nil
	}
	if v.scalar != nil {
		return v.scalar.MarshalJSON()
	}
	if v.enum != "" {
		return json.Marshal(v.enum)
	}
	if v.list != nil {
		return marshalInputList(v.list)
	}
	if v.object != nil {
		return marshalInputObject(v.object)
	}
	return nil, fmt.Errorf("input value %q has no representation", v.typeID)
}

// CoerceInput validates and canonicalizes one input value. A nil raw message
// represents missing; explicit JSON null is accepted only when nullable.
func CoerceInput(types Snapshot, typeID TypeID, raw json.RawMessage, nullable bool) (InputValue, error) {
	state := coercionState{types: types, active: make(map[TypeID]int)}
	return state.coerce(typeID, raw, nullable)
}

type coercionState struct {
	types  Snapshot
	active map[TypeID]int
}

func (s *coercionState) coerce(typeID TypeID, raw json.RawMessage, nullable bool) (InputValue, error) {
	descriptor, ok := s.types.types[typeID]
	if !ok || !descriptor.Input {
		return InputValue{}, fmt.Errorf("type %q is not available in input position", typeID)
	}
	if raw == nil {
		return InputValue{typeID: typeID, presence: PresenceMissing}, nil
	}
	if err := protocol.ValidateJSON(raw, protocol.Limits{}); err != nil {
		return InputValue{}, fmt.Errorf("type %q received invalid JSON: %w", typeID, err)
	}
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		if !nullable {
			return InputValue{}, fmt.Errorf("non-null input %q is null", typeID)
		}
		return InputValue{typeID: typeID, presence: PresenceNull}, nil
	}
	if descriptor.MaxDepth > 0 && s.active[typeID] > descriptor.MaxDepth {
		return InputValue{}, fmt.Errorf("input type %q exceeds maximum depth", typeID)
	}
	s.active[typeID]++
	defer func() { s.active[typeID]-- }()

	switch descriptor.Kind {
	case ScalarType:
		return s.coerceScalar(typeID, raw)
	case ListType:
		return s.coerceList(descriptor, raw)
	case MapType:
		return s.coerceMap(descriptor, raw)
	case InputObjectType, OneOfType:
		return s.coerceObject(descriptor, raw)
	case EnumType:
		return coerceEnum(descriptor, raw)
	case ObjectType, InterfaceType, UnionType:
		return InputValue{}, fmt.Errorf("type %q is output-only", typeID)
	default:
		return InputValue{}, fmt.Errorf("type %q has unknown input kind", typeID)
	}
}

func (s *coercionState) coerceScalar(typeID TypeID, raw json.RawMessage) (InputValue, error) {
	value, err := CanonicalizeScalar(s.types, typeID, raw)
	if err != nil {
		return InputValue{}, err
	}
	return InputValue{typeID: typeID, presence: PresenceValue, scalar: &value}, nil
}

// CanonicalizeScalar validates and canonicalizes a built-in or registered
// custom scalar for input, output serialization, and semantic hashing.
func CanonicalizeScalar(types Snapshot, typeID TypeID, raw json.RawMessage) (Value, error) {
	descriptor, ok := types.types[typeID]
	if !ok || descriptor.Kind != ScalarType {
		return Value{}, fmt.Errorf("type %q is not a scalar", typeID)
	}
	if knownScalar(ScalarKind(typeID)) {
		return ParseScalar(ScalarKind(typeID), raw)
	}
	runtime, ok := types.scalars[typeID]
	if !ok {
		return Value{}, fmt.Errorf("custom scalar %q has no deterministic profile", typeID)
	}
	canonical, err := runtime.canonicalize(raw)
	if err != nil {
		return Value{}, fmt.Errorf("canonicalize custom scalar %q: %w", typeID, err)
	}
	return Value{kind: ScalarKind(typeID), presence: PresenceValue, canonical: string(canonical)}, nil
}

func (s *coercionState) coerceList(descriptor TypeDescriptor, raw json.RawMessage) (InputValue, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return InputValue{}, fmt.Errorf("list input %q requires an array", descriptor.ID)
	}
	result := make([]InputValue, len(items))
	for index, item := range items {
		coerced, err := s.coerce(descriptor.Element, item, descriptor.ElementNullable)
		if err != nil {
			return InputValue{}, fmt.Errorf("list input %q item %d: %w", descriptor.ID, index, err)
		}
		result[index] = coerced
	}
	return InputValue{typeID: descriptor.ID, presence: PresenceValue, list: result}, nil
}

func (s *coercionState) coerceMap(descriptor TypeDescriptor, raw json.RawMessage) (InputValue, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return InputValue{}, fmt.Errorf("map input %q requires an object", descriptor.ID)
	}
	result := make(map[string]InputValue, len(members))
	for _, key := range sortedKeys(members) {
		if !utf8.ValidString(key) {
			return InputValue{}, fmt.Errorf("map input %q has invalid UTF-8 key", descriptor.ID)
		}
		coerced, err := s.coerce(descriptor.Element, members[key], descriptor.ElementNullable)
		if err != nil {
			return InputValue{}, fmt.Errorf("map input %q key %q: %w", descriptor.ID, key, err)
		}
		result[key] = coerced
	}
	return InputValue{typeID: descriptor.ID, presence: PresenceValue, object: result}, nil
}

func (s *coercionState) coerceObject(descriptor TypeDescriptor, raw json.RawMessage) (InputValue, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return InputValue{}, fmt.Errorf("object input %q requires an object", descriptor.ID)
	}
	if unknown, exists := firstUnknownInputMember(descriptor.Fields, members); exists {
		return InputValue{}, fmt.Errorf("object input %q has unknown field %q", descriptor.ID, unknown)
	}
	result, active, err := s.coerceDeclaredFields(descriptor, members)
	if err != nil {
		return InputValue{}, err
	}
	if descriptor.Kind == OneOfType && active == "" {
		return InputValue{}, fmt.Errorf("one-of input %q requires one active member", descriptor.ID)
	}
	return InputValue{typeID: descriptor.ID, presence: PresenceValue, object: result, activeMember: active}, nil
}

func (s *coercionState) coerceDeclaredFields(descriptor TypeDescriptor, members map[string]json.RawMessage) (map[string]InputValue, string, error) {
	result := make(map[string]InputValue, len(descriptor.Fields))
	active := ""
	for _, name := range sortedKeys(descriptor.Fields) {
		field := descriptor.Fields[name]
		coerced, err := s.coerceDeclaredField(descriptor, name, field, members)
		if err != nil {
			return nil, "", err
		}
		result[name] = coerced
		if descriptor.Kind == OneOfType && !coerced.IsMissing() {
			if active != "" {
				return nil, "", fmt.Errorf("one-of input %q has multiple active members", descriptor.ID)
			}
			active = name
		}
	}
	return result, active, nil
}

func (s *coercionState) coerceDeclaredField(descriptor TypeDescriptor, name string, field FieldDescriptor, members map[string]json.RawMessage) (InputValue, error) {
	member, present := members[name]
	if !present && field.Default != nil {
		member, present = field.Default, true
	}
	if !present && field.Required {
		return InputValue{}, fmt.Errorf("object input %q requires field %q", descriptor.ID, name)
	}
	coerced, err := s.coerce(field.Type, member, field.Nullable)
	if err != nil {
		return InputValue{}, fmt.Errorf("object input %q field %q: %w", descriptor.ID, name, err)
	}
	return coerced, nil
}

func firstUnknownInputMember(fields map[string]FieldDescriptor, members map[string]json.RawMessage) (string, bool) {
	for _, name := range sortedKeys(members) {
		if _, ok := fields[name]; !ok {
			return name, true
		}
	}
	return "", false
}

func coerceEnum(descriptor TypeDescriptor, raw json.RawMessage) (InputValue, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) {
		return InputValue{}, fmt.Errorf("enum input %q requires a valid string", descriptor.ID)
	}
	known := slices.Contains(descriptor.EnumValues, value)
	if !known && !descriptor.Open {
		return InputValue{}, fmt.Errorf("closed enum input %q rejects value %q", descriptor.ID, value)
	}
	return InputValue{typeID: descriptor.ID, presence: PresenceValue, enum: value, enumKnown: known}, nil
}

func validateInputDefaults(snapshot Snapshot) error {
	for _, identifier := range sortedKeys(snapshot.types) {
		descriptor := snapshot.types[identifier]
		if descriptor.Kind != InputObjectType && descriptor.Kind != OneOfType {
			continue
		}
		defaults := 0
		for _, name := range sortedKeys(descriptor.Fields) {
			field := descriptor.Fields[name]
			if field.Default == nil {
				continue
			}
			defaults++
			if _, err := CoerceInput(snapshot, field.Type, field.Default, field.Nullable); err != nil {
				return fmt.Errorf("schema type %q field %q has invalid default: %w", descriptor.ID, name, err)
			}
		}
		if descriptor.Kind == OneOfType && defaults > 1 {
			return fmt.Errorf("one-of schema %q has multiple defaulted members", descriptor.ID)
		}
	}
	return nil
}

func marshalInputList(values []InputValue) ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			output.WriteByte(',')
		}
		encoded, err := value.MarshalJSON()
		if err != nil {
			return nil, err
		}
		output.Write(encoded)
	}
	output.WriteByte(']')
	return output.Bytes(), nil
}

func marshalInputObject(values map[string]InputValue) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if !value.IsMissing() {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var output bytes.Buffer
	output.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			output.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		encodedValue, err := values[key].MarshalJSON()
		if err != nil {
			return nil, err
		}
		output.Write(encodedKey)
		output.WriteByte(':')
		output.Write(encodedValue)
	}
	output.WriteByte('}')
	canonical, err := protocol.CanonicalizeJSON(output.Bytes(), protocol.Limits{})
	if err != nil {
		return nil, fmt.Errorf("canonicalize input object: %w", err)
	}
	return canonical, nil
}
