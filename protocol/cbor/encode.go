package cbor

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"reflect"
	"sort"
	"time"
	"unicode/utf8"
)

// Marshal emits exactly one cbor-det-1 value.
func Marshal(value any) ([]byte, error) {
	encoder := encoder{output: make([]byte, 0, 256)}
	if err := encoder.value(value, false); err != nil {
		return nil, err
	}
	return encoder.output, nil
}

type encoder struct{ output []byte }

func (e *encoder) value(value any, mapValue bool) error {
	switch current := value.(type) {
	case Missing:
		if mapValue {
			return errMissingMapValue
		}
		return errors.New("missing value is valid only as an omitted map member")
	case nil:
		e.output = append(e.output, 0xf6)
	case bool:
		if current {
			e.output = append(e.output, 0xf5)
		} else {
			e.output = append(e.output, 0xf4)
		}
	case string:
		return e.text(current)
	case ID:
		return e.text(string(current))
	case Enum:
		return e.text(string(current))
	case int:
		return e.boundedInteger(int64(current))
	case int8:
		return e.boundedInteger(int64(current))
	case int16:
		return e.boundedInteger(int64(current))
	case int32:
		return e.boundedInteger(int64(current))
	case int64:
		return e.boundedInteger(current)
	case uint:
		if uint64(current) > uint64(maxSafeInteger) {
			return errors.New("bounded integer exceeds JSON safe range")
		}
		e.head(0, uint64(current))
	case uint8:
		e.head(0, uint64(current))
	case uint16:
		e.head(0, uint64(current))
	case uint32:
		if uint64(current) > uint64(maxSafeInteger) {
			return errors.New("bounded integer exceeds JSON safe range")
		}
		e.head(0, uint64(current))
	case uint64:
		if current > uint64(maxSafeInteger) {
			return errors.New("bounded integer exceeds JSON safe range")
		}
		e.head(0, current)
	case Int64:
		e.head(6, tagInt64)
		e.signed(int64(current))
	case UInt64:
		e.head(6, tagUInt64)
		e.head(0, uint64(current))
	case BigInt:
		return e.bigInt(current.value)
	case Decimal:
		return e.decimal(current)
	case float64:
		if math.IsNaN(current) || math.IsInf(current, 0) {
			return errors.New("Float64 must be finite")
		}
		if current == 0 {
			current = 0
		}
		e.output = append(e.output, 0xfb)
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], math.Float64bits(current))
		e.output = append(e.output, encoded[:]...)
	case float32:
		return e.value(float64(current), mapValue)
	case Timestamp:
		if current.IsZero() {
			return errors.New("Timestamp value is required")
		}
		e.head(6, tagTimestamp)
		return e.text(current.UTC().Format(time.RFC3339Nano))
	case Duration:
		if current.nanoseconds == nil {
			return errors.New("Duration value is required")
		}
		e.head(6, tagDuration)
		return e.arbitraryInteger(current.nanoseconds)
	case UUID:
		e.head(6, tagUUID)
		e.head(2, 16)
		e.output = append(e.output, current[:]...)
	case Bytes:
		e.head(2, uint64(len(current)))
		e.output = append(e.output, current...)
	case []byte:
		e.head(2, uint64(len(current)))
		e.output = append(e.output, current...)
	case []any:
		e.head(4, uint64(len(current)))
		for _, item := range current {
			if err := e.value(item, false); err != nil {
				return err
			}
		}
	case map[string]any:
		return e.stringMap(current)
	case Union:
		if current.Type == "" {
			return errors.New("Union type is required")
		}
		return e.stringMap(map[string]any{"$type": current.Type, "$value": current.Value})
	case json.Number:
		return e.jsonNumber(current)
	default:
		return e.reflected(value)
	}
	return nil
}

var errMissingMapValue = errors.New("omit missing map member")

func (e *encoder) reflected(value any) error {
	if value == nil {
		return e.value(nil, false)
	}
	reflected := reflect.ValueOf(value)
	// Only collection kinds reach this fallback; scalar and profile wrapper
	// kinds are handled by the closed type switch above.
	//nolint:exhaustive
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		length := reflected.Len()
		e.head(4, uint64(length))
		for index := range length {
			if err := e.value(reflected.Index(index).Interface(), false); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String {
			return errors.New("application map keys must be strings")
		}
		values := make(map[string]any, reflected.Len())
		iterator := reflected.MapRange()
		for iterator.Next() {
			values[iterator.Key().String()] = iterator.Value().Interface()
		}
		return e.stringMap(values)
	default:
		return errors.New("unsupported CBOR value type")
	}
}

func (e *encoder) boundedInteger(value int64) error {
	if value < -maxSafeInteger || value > maxSafeInteger {
		return errors.New("bounded integer exceeds JSON safe range")
	}
	e.signed(value)
	return nil
}

func (e *encoder) signed(value int64) {
	if value >= 0 {
		e.head(0, uint64(value))
		return
	}
	e.head(1, uint64(-(value + 1)))
}

func (e *encoder) arbitraryInteger(value *big.Int) error {
	if value == nil {
		return errors.New("integer value is required")
	}
	if value.Sign() >= 0 {
		if value.BitLen() <= 64 {
			e.head(0, value.Uint64())
			return nil
		}
	} else {
		argument := new(big.Int).Neg(value)
		argument.Sub(argument, big.NewInt(1))
		if argument.BitLen() <= 64 {
			e.head(1, argument.Uint64())
			return nil
		}
	}
	return e.bigIntValue(value)
}

func (e *encoder) bigInt(value *big.Int) error {
	if value == nil {
		return errors.New("BigInt value is required")
	}
	return e.bigIntValue(value)
}

func (e *encoder) bigIntValue(value *big.Int) error {
	magnitude := new(big.Int).Set(value)
	if value.Sign() < 0 {
		e.head(6, tagBigNeg)
		magnitude.Neg(value)
		magnitude.Sub(magnitude, big.NewInt(1))
	} else {
		e.head(6, tagBigPos)
	}
	bytes := magnitude.Bytes()
	e.head(2, uint64(len(bytes)))
	e.output = append(e.output, bytes...)
	return nil
}

func (e *encoder) decimal(value Decimal) error {
	if value.mantissa == nil {
		return errors.New("Decimal mantissa is required")
	}
	normalized, err := NewDecimal(value.Exponent, value.mantissa)
	if err != nil {
		return err
	}
	e.head(6, tagDecimal)
	e.head(4, 2)
	e.signed(normalized.Exponent)
	return e.arbitraryInteger(normalized.mantissa)
}

func (e *encoder) jsonNumber(value json.Number) error {
	text := value.String()
	if integer, ok := new(big.Int).SetString(text, 10); ok && integer.String() == text {
		if !integer.IsInt64() {
			return errors.New("JSON integer exceeds safe range")
		}
		return e.boundedInteger(integer.Int64())
	}
	number, err := value.Float64()
	if err != nil {
		return errors.New("invalid JSON number")
	}
	return e.value(number, false)
}

type mapEntry struct {
	key     string
	encoded []byte
	value   any
}

func (e *encoder) stringMap(values map[string]any) error {
	entries := make([]mapEntry, 0, len(values))
	for key, value := range values {
		if _, missing := value.(Missing); missing {
			continue
		}
		if !utf8.ValidString(key) {
			return errors.New("map key is not valid UTF-8")
		}
		keyEncoder := encoder{}
		if err := keyEncoder.text(key); err != nil {
			return err
		}
		entries = append(entries, mapEntry{key: key, encoded: keyEncoder.output, value: value})
	}
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].encoded) != len(entries[j].encoded) {
			return len(entries[i].encoded) < len(entries[j].encoded)
		}
		return string(entries[i].encoded) < string(entries[j].encoded)
	})
	e.head(5, uint64(len(entries)))
	for _, entry := range entries {
		e.output = append(e.output, entry.encoded...)
		if err := e.value(entry.value, true); err != nil {
			return err
		}
	}
	return nil
}

func (e *encoder) text(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("text is not valid UTF-8")
	}
	e.head(3, uint64(len(value)))
	e.output = append(e.output, value...)
	return nil
}

func (e *encoder) head(major byte, value uint64) {
	switch {
	case value < 24:
		e.output = append(e.output, major<<5|byte(value))
	case value <= math.MaxUint8:
		e.output = append(e.output, major<<5|24, byte(value))
	case value <= math.MaxUint16:
		e.output = append(e.output, major<<5|25, byte(value>>8), byte(value))
	case value <= math.MaxUint32:
		e.output = append(e.output, major<<5|26, byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	default:
		e.output = append(e.output, major<<5|27,
			byte(value>>56), byte(value>>48), byte(value>>40), byte(value>>32),
			byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	}
}
