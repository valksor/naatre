package cbor

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/big"
	"time"
	"unicode/utf8"
)

// Unmarshal strictly decodes one cbor-det-1 value.
func Unmarshal(input []byte, limits Limits) (any, error) {
	return UnmarshalContext(context.Background(), input, limits)
}

// UnmarshalContext is Unmarshal with cooperative cancellation between items.
func UnmarshalContext(ctx context.Context, input []byte, limits Limits) (any, error) {
	if ctx == nil {
		return nil, &Error{Code: "INVALID_CONTEXT"}
	}
	resolved, err := resolveLimits(limits)
	if err != nil {
		return nil, err
	}
	if len(input) > resolved.MaxBytes {
		return nil, &Error{Code: "LIMIT_BYTES", Offset: resolved.MaxBytes}
	}
	decoder := decoder{ctx: ctx, input: input, limits: resolved}
	value, err := decoder.value(1, integerNative)
	if err != nil {
		return nil, err
	}
	if decoder.offset != len(input) {
		return nil, decoder.failure("TRAILING_DATA", decoder.offset)
	}
	return value, nil
}

type integerMode uint8

const (
	integerNative integerMode = iota
	integerSigned64
	integerUnsigned64
	integerArbitrary
)

type decoder struct {
	ctx       context.Context
	input     []byte
	offset    int
	tags      int
	allocated int
	limits    Limits
}

func (d *decoder) value(depth int, mode integerMode) (any, error) {
	if err := d.ctx.Err(); err != nil {
		return nil, d.failure("CANCELLED", d.offset)
	}
	if depth > d.limits.MaxDepth {
		return nil, d.failure("LIMIT_DEPTH", d.offset)
	}
	start := d.offset
	initial, err := d.byte()
	if err != nil {
		return nil, err
	}
	major, additional := initial>>5, initial&0x1f
	switch major {
	case 0, 1:
		argument, err := d.argument(additional, start)
		if err != nil {
			return nil, err
		}
		return d.integer(major, argument, mode, start)
	case 2:
		return d.byteString(additional, start, d.limits.MaxByteStringBytes, "LIMIT_BYTE_STRING")
	case 3:
		return d.text(additional, start)
	case 4:
		return d.array(additional, start, depth)
	case 5:
		return d.stringMap(additional, start, depth)
	case 6:
		tag, err := d.argument(additional, start)
		if err != nil {
			return nil, err
		}
		d.tags++
		if d.tags > d.limits.MaxTags {
			return nil, d.failure("LIMIT_TAGS", start)
		}
		return d.semanticTag(tag, depth, start)
	case 7:
		return d.simple(additional, start)
	default:
		return nil, d.failure("MALFORMED", start)
	}
}

func (d *decoder) integer(major byte, argument uint64, mode integerMode, start int) (any, error) {
	switch mode {
	case integerNative:
		if major == 0 {
			if argument > uint64(maxSafeInteger) {
				return nil, d.failure("INTEGER_RANGE", start)
			}
			return int64(argument), nil
		}
		if argument >= uint64(maxSafeInteger) {
			return nil, d.failure("INTEGER_RANGE", start)
		}
		return -1 - int64(argument), nil
	case integerSigned64:
		if major == 0 {
			if argument > math.MaxInt64 {
				return nil, d.failure("INTEGER_RANGE", start)
			}
			return Int64(argument), nil
		}
		if argument > math.MaxInt64 {
			return nil, d.failure("INTEGER_RANGE", start)
		}
		return Int64(-1 - int64(argument)), nil
	case integerUnsigned64:
		if major != 0 {
			return nil, d.failure("INTEGER_RANGE", start)
		}
		return UInt64(argument), nil
	case integerArbitrary:
		value := new(big.Int).SetUint64(argument)
		if major == 1 {
			value.Add(value, big.NewInt(1))
			value.Neg(value)
		}
		return value, nil
	default:
		return nil, d.failure("MALFORMED", start)
	}
}

func (d *decoder) byteString(additional byte, start, maximum int, code string) ([]byte, error) {
	length, err := d.argument(additional, start)
	if err != nil {
		return nil, err
	}
	if length > uint64(maximum) {
		return nil, d.failure(code, start)
	}
	if length > uint64(len(d.input)-d.offset) {
		return nil, d.failure("TRUNCATED", d.offset)
	}
	if err := d.reserve(int(length), start); err != nil {
		return nil, err
	}
	result := bytes.Clone(d.input[d.offset : d.offset+int(length)])
	d.offset += int(length)
	return result, nil
}

func (d *decoder) text(additional byte, start int) (string, error) {
	payload, err := d.byteString(additional, start, d.limits.MaxTextBytes, "LIMIT_TEXT")
	if err != nil {
		return "", err
	}
	if !utf8.Valid(payload) {
		return "", d.failure("INVALID_UTF8", start)
	}
	if err := d.reserve(len(payload), start); err != nil {
		return "", err
	}
	return string(payload), nil
}

func (d *decoder) array(additional byte, start, depth int) ([]any, error) {
	length, err := d.argument(additional, start)
	if err != nil {
		return nil, err
	}
	if length > uint64(d.limits.MaxArrayItems) {
		return nil, d.failure("LIMIT_ARRAY", start)
	}
	if length > uint64(maxInt()/16) {
		return nil, d.failure("LIMIT_ALLOCATION", start)
	}
	if err := d.reserve(int(length)*16, start); err != nil {
		return nil, err
	}
	result := make([]any, 0, int(length))
	for range length {
		item, err := d.value(depth+1, integerNative)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (d *decoder) stringMap(additional byte, start, depth int) (map[string]any, error) {
	length, err := d.argument(additional, start)
	if err != nil {
		return nil, err
	}
	if length > uint64(d.limits.MaxMapPairs) {
		return nil, d.failure("LIMIT_MAP", start)
	}
	if length > uint64(maxInt()/32) {
		return nil, d.failure("LIMIT_ALLOCATION", start)
	}
	if err := d.reserve(int(length)*32, start); err != nil {
		return nil, err
	}
	result := make(map[string]any, int(length))
	var previous []byte
	for range length {
		key, encoded, keyStart, err := d.stringKey(depth)
		if err != nil {
			return nil, err
		}
		if _, exists := result[key]; exists {
			return nil, d.failure("DUPLICATE_KEY", keyStart)
		}
		if previous != nil && deterministicCompare(previous, encoded) >= 0 {
			return nil, d.failure("MAP_ORDER", keyStart)
		}
		previous = encoded
		value, err := d.value(depth+1, integerNative)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

func (d *decoder) stringKey(depth int) (string, []byte, int, error) {
	start := d.offset
	if start >= len(d.input) || d.input[start]>>5 != 3 {
		return "", nil, start, d.failure("NON_STRING_KEY", start)
	}
	value, err := d.value(depth+1, integerNative)
	if err != nil {
		return "", nil, start, err
	}
	return value.(string), d.input[start:d.offset], start, nil
}

func deterministicCompare(left, right []byte) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return bytes.Compare(left, right)
}

func (d *decoder) semanticTag(tag uint64, depth, start int) (any, error) {
	switch tag {
	case tagTimestamp:
		text, err := d.taggedText(depth)
		if err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != text {
			return nil, d.failure("INVALID_TIMESTAMP", start)
		}
		return Timestamp{Time: parsed}, nil
	case tagBigPos, tagBigNeg:
		return d.taggedBigInt(tag, start)
	case tagDecimal:
		return d.taggedDecimal(depth, start)
	case tagUUID:
		value, err := d.rawByteString(d.limits.MaxByteStringBytes)
		if err != nil {
			return nil, err
		}
		if len(value) != 16 {
			return nil, d.failure("INVALID_UUID", start)
		}
		var uuid UUID
		copy(uuid[:], value)
		return uuid, nil
	case tagInt64:
		return d.directInteger(integerSigned64)
	case tagUInt64:
		return d.directInteger(integerUnsigned64)
	case tagDuration:
		integer, err := d.arbitraryInteger(depth + 1)
		if err != nil {
			return nil, err
		}
		return NewDuration(integer)
	default:
		return nil, d.failure("FORBIDDEN_TAG", start)
	}
}

func (d *decoder) taggedText(depth int) (string, error) {
	value, err := d.value(depth+1, integerNative)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", d.failure("INVALID_TAG_CONTENT", d.offset)
	}
	return text, nil
}

func (d *decoder) taggedBigInt(tag uint64, start int) (BigInt, error) {
	magnitude, err := d.rawByteString(d.limits.MaxBignumBytes)
	if err != nil {
		return BigInt{}, err
	}
	if len(magnitude) > 0 && magnitude[0] == 0 {
		return BigInt{}, d.failure("NON_PREFERRED_BIGNUM", start)
	}
	value := new(big.Int).SetBytes(magnitude)
	if tag == tagBigNeg {
		value.Add(value, big.NewInt(1))
		value.Neg(value)
	}
	return NewBigInt(value)
}

func (d *decoder) rawByteString(maximum int) ([]byte, error) {
	start := d.offset
	initial, err := d.byte()
	if err != nil {
		return nil, err
	}
	if initial>>5 != 2 {
		return nil, d.failure("INVALID_TAG_CONTENT", start)
	}
	return d.byteString(initial&0x1f, start, maximum, "LIMIT_BIGNUM")
}

func (d *decoder) taggedDecimal(depth, start int) (Decimal, error) {
	arrayStart := d.offset
	initial, err := d.byte()
	if err != nil {
		return Decimal{}, err
	}
	if initial>>5 != 4 {
		return Decimal{}, d.failure("INVALID_DECIMAL", arrayStart)
	}
	length, err := d.argument(initial&0x1f, arrayStart)
	if err != nil || length != 2 {
		return Decimal{}, d.failure("INVALID_DECIMAL", arrayStart)
	}
	exponentValue, err := d.directInteger(integerSigned64)
	if err != nil {
		return Decimal{}, err
	}
	mantissa, err := d.arbitraryInteger(depth + 1)
	if err != nil {
		return Decimal{}, err
	}
	exponent, exponentOK := exponentValue.(Int64)
	if !exponentOK {
		return Decimal{}, d.failure("INVALID_DECIMAL", start)
	}
	normalized, err := NewDecimal(int64(exponent), mantissa)
	if err != nil || normalized.Exponent != int64(exponent) || normalized.mantissa.Cmp(mantissa) != 0 {
		return Decimal{}, d.failure("NON_PREFERRED_DECIMAL", start)
	}
	return normalized, nil
}

func (d *decoder) directInteger(mode integerMode) (any, error) {
	start := d.offset
	initial, err := d.byte()
	if err != nil {
		return nil, err
	}
	major := initial >> 5
	if major != 0 && major != 1 {
		return nil, d.failure("INVALID_TAG_CONTENT", start)
	}
	argument, err := d.argument(initial&0x1f, start)
	if err != nil {
		return nil, err
	}
	return d.integer(major, argument, mode, start)
}

func (d *decoder) arbitraryInteger(depth int) (*big.Int, error) {
	value, err := d.value(depth, integerArbitrary)
	if err != nil {
		return nil, err
	}
	switch integer := value.(type) {
	case *big.Int:
		return integer, nil
	case BigInt:
		if integer.value == nil {
			return nil, d.failure("INVALID_TAG_CONTENT", d.offset)
		}
		return new(big.Int).Set(integer.value), nil
	default:
		return nil, d.failure("INVALID_TAG_CONTENT", d.offset)
	}
}

func (d *decoder) simple(additional byte, start int) (any, error) {
	switch additional {
	case 20:
		return false, nil
	case 21:
		return true, nil
	case 22:
		return nil, nil
	case 27:
		if len(d.input)-d.offset < 8 {
			return nil, d.failure("TRUNCATED", d.offset)
		}
		bits := binary.BigEndian.Uint64(d.input[d.offset : d.offset+8])
		d.offset += 8
		value := math.Float64frombits(bits)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, d.failure("NON_FINITE_FLOAT", start)
		}
		if value == 0 && math.Signbit(value) {
			return nil, d.failure("NON_PREFERRED_FLOAT", start)
		}
		return value, nil
	case 25, 26:
		return nil, d.failure("FLOAT_WIDTH", start)
	case 31:
		return nil, d.failure("INDEFINITE_LENGTH", start)
	default:
		return nil, d.failure("FORBIDDEN_SIMPLE", start)
	}
}

func (d *decoder) argument(additional byte, start int) (uint64, error) {
	sizes := [...]int{24: 1, 25: 2, 26: 4, 27: 8}
	if additional < 24 {
		return uint64(additional), nil
	}
	if additional == 31 {
		return 0, d.failure("INDEFINITE_LENGTH", start)
	}
	if additional > 27 {
		return 0, d.failure("MALFORMED", start)
	}
	size := sizes[additional]
	if len(d.input)-d.offset < size {
		return 0, d.failure("TRUNCATED", d.offset)
	}
	var value uint64
	for _, current := range d.input[d.offset : d.offset+size] {
		value = value<<8 | uint64(current)
	}
	d.offset += size
	minimum := [...]uint64{24: 24, 25: 1 << 8, 26: 1 << 16, 27: 1 << 32}
	if value < minimum[additional] {
		return 0, d.failure("NON_PREFERRED_INTEGER", start)
	}
	return value, nil
}

func (d *decoder) byte() (byte, error) {
	if d.offset >= len(d.input) {
		return 0, d.failure("TRUNCATED", d.offset)
	}
	value := d.input[d.offset]
	d.offset++
	return value, nil
}

func (d *decoder) reserve(size, offset int) error {
	if size < 0 || d.allocated > d.limits.MaxAllocationBytes-size {
		return d.failure("LIMIT_ALLOCATION", offset)
	}
	d.allocated += size
	return nil
}

func (d *decoder) failure(code string, offset int) *Error {
	return &Error{Code: code, Offset: offset}
}

func resolveLimits(limits Limits) (Limits, error) {
	defaults := DefaultLimits()
	fields := []*int{&limits.MaxBytes, &limits.MaxDepth, &limits.MaxArrayItems, &limits.MaxMapPairs, &limits.MaxTextBytes, &limits.MaxByteStringBytes, &limits.MaxBignumBytes, &limits.MaxTags, &limits.MaxAllocationBytes}
	defaultValues := []int{defaults.MaxBytes, defaults.MaxDepth, defaults.MaxArrayItems, defaults.MaxMapPairs, defaults.MaxTextBytes, defaults.MaxByteStringBytes, defaults.MaxBignumBytes, defaults.MaxTags, defaults.MaxAllocationBytes}
	for index, field := range fields {
		if *field == 0 {
			*field = defaultValues[index]
		}
		if *field < 1 {
			return Limits{}, &Error{Code: "INVALID_LIMITS"}
		}
	}
	return limits, nil
}

func maxInt() int { return int(^uint(0) >> 1) }
