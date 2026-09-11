package schema

import "fmt"

// TaggedValue is the explicit Go representation of a union or interface
// output. Its fields are private so a tag cannot be changed independently of
// its value after construction. The executor isolates the contained value
// before completion.
type TaggedValue struct {
	variant TypeID
	value   any
}

// Tag constructs an explicit union or interface output value.
func Tag(variant TypeID, value any) (TaggedValue, error) {
	if !typeIDPattern.MatchString(string(variant)) {
		return TaggedValue{}, fmt.Errorf("invalid tagged value type %q", variant)
	}
	return TaggedValue{variant: variant, value: value}, nil
}

// MustTag constructs a tagged value and panics when the variant identifier is
// invalid. It is intended for static registration and test fixtures.
func MustTag(variant TypeID, value any) TaggedValue {
	tagged, err := Tag(variant, value)
	if err != nil {
		panic(err)
	}
	return tagged
}

// Variant returns the stable schema type identifier carried by this value.
func (v TaggedValue) Variant() TypeID {
	return v.variant
}

// Value returns the value associated with the tag. Runtime completion copies
// it before validation, so later handler mutation cannot change an outcome.
func (v TaggedValue) Value() any {
	return v.value
}
