package reflectadapter

import (
	"bytes"
	"encoding/json"
)

// Optional preserves the distinction between a missing input-object member,
// an explicitly null member, and a present typed value. Its zero value is
// missing. It is intended for fields of reflected input structs.
type Optional[T any] struct {
	value   T
	present bool
	null    bool
}

func (Optional[T]) reflectadapterOptional() {}

// Present reports whether the member appeared in the request input.
func (o Optional[T]) Present() bool { return o.present }

// Null reports whether the member appeared with the explicit null value.
func (o Optional[T]) Null() bool { return o.present && o.null }

// Value returns the typed value only when the member was present and non-null.
func (o Optional[T]) Value() (T, bool) { return o.value, o.present && !o.null }

// UnmarshalJSON implements the presence-aware input conversion contract.
func (o *Optional[T]) UnmarshalJSON(input []byte) error {
	o.present = true
	if bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		o.null = true
		o.value = *new(T)
		return nil
	}
	o.null = false
	return json.Unmarshal(input, &o.value)
}
