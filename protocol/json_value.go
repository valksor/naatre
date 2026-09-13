package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// DecodeJSONValue decodes exactly one JSON value while preserving numbers
// without coercing them through float64.
func DecodeJSONValue(input []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}
