// Package schema defines Naatre's language-neutral types and portable values.
package schema

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ScalarKind identifies a portable scalar codec.
type ScalarKind string

const (
	Boolean   ScalarKind = "Boolean"
	String    ScalarKind = "String"
	ID        ScalarKind = "ID"
	Int32     ScalarKind = "Int32"
	Float64   ScalarKind = "Float64"
	Int64     ScalarKind = "Int64"
	UInt64    ScalarKind = "UInt64"
	BigInt    ScalarKind = "BigInt"
	Decimal   ScalarKind = "Decimal"
	Timestamp ScalarKind = "Timestamp"
	Duration  ScalarKind = "Duration"
	UUID      ScalarKind = "UUID"
	Bytes     ScalarKind = "Bytes"
)

// Presence distinguishes an absent value, explicit null, and a present value.
type Presence uint8

const (
	PresenceMissing Presence = iota
	PresenceNull
	PresenceValue
)

var (
	// ErrMissingValue indicates that an absent value cannot be serialized.
	ErrMissingValue    = errors.New("missing value has no JSON representation")
	errInvalidScalar   = errors.New("invalid scalar value")
	integerPattern     = regexp.MustCompile(`^-?[0-9]+$`)
	jsonIntegerPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)
	unsignedPattern    = regexp.MustCompile(`^[0-9]+$`)
	decimalPattern     = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
	jsonNumberPattern  = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	timestampPattern   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9](?:\.[0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
	uuidPattern        = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

// Value is an immutable scalar value with explicit presence state.
type Value struct {
	kind      ScalarKind
	presence  Presence
	canonical string
}

// Missing constructs an absent value of the declared scalar kind.
func Missing(kind ScalarKind) Value {
	return Value{kind: kind, presence: PresenceMissing}
}

// Null constructs an explicit null value of the declared scalar kind.
func Null(kind ScalarKind) Value {
	return Value{kind: kind, presence: PresenceNull, canonical: "null"}
}

// Kind returns the declared scalar kind.
func (v Value) Kind() ScalarKind { return v.kind }

// Presence returns the value's presence state.
func (v Value) Presence() Presence { return v.presence }

// IsMissing reports whether the value is absent.
func (v Value) IsMissing() bool { return v.presence == PresenceMissing }

// IsNull reports whether the value is explicitly null.
func (v Value) IsNull() bool { return v.presence == PresenceNull }

// MarshalJSON returns the scalar's canonical JSON encoding.
func (v Value) MarshalJSON() ([]byte, error) {
	if v.IsMissing() {
		return nil, ErrMissingValue
	}
	return []byte(v.canonical), nil
}

// ParseScalar validates raw JSON using the declared scalar codec and stores its
// canonical JSON representation without converting extended numbers through
// binary floating point.
func ParseScalar(kind ScalarKind, raw json.RawMessage) (Value, error) {
	if !knownScalar(kind) {
		return Value{}, fmt.Errorf("parse %s: %w", kind, errInvalidScalar)
	}
	if bytes.Equal(raw, []byte("null")) {
		return Null(kind), nil
	}

	canonical, err := canonicalScalar(kind, raw)
	if err != nil {
		return Value{}, fmt.Errorf("parse %s: %w", kind, err)
	}
	return Value{kind: kind, presence: PresenceValue, canonical: canonical}, nil
}

func knownScalar(kind ScalarKind) bool {
	switch kind {
	case Boolean, String, ID, Int32, Float64, Int64, UInt64, BigInt, Decimal, Timestamp, Duration, UUID, Bytes:
		return true
	default:
		return false
	}
}

func canonicalScalar(kind ScalarKind, raw []byte) (string, error) {
	switch kind {
	case Boolean:
		if bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false")) {
			return string(raw), nil
		}
		return "", errInvalidScalar
	case String, ID:
		value, err := parseJSONString(raw)
		if err != nil {
			return "", err
		}
		return canonicalJSONString(value), nil
	case Int32:
		return canonicalInt32(raw)
	case Float64:
		return canonicalFloat64(raw)
	case Int64:
		return canonicalSignedString(raw, 64)
	case UInt64:
		return canonicalUnsignedString(raw, 64)
	case BigInt:
		return canonicalBigInt(raw)
	case Decimal:
		return canonicalDecimal(raw)
	case Timestamp:
		return canonicalTimestamp(raw)
	case Duration:
		return canonicalDuration(raw)
	case UUID:
		return canonicalUUID(raw)
	case Bytes:
		return canonicalBytes(raw)
	default:
		return "", fmt.Errorf("%w: unknown kind %q", errInvalidScalar, kind)
	}
}

func parseJSONString(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("%w: invalid UTF-8", errInvalidScalar)
	}
	if !validJSONSurrogates(raw) {
		return "", fmt.Errorf("%w: unpaired Unicode surrogate", errInvalidScalar)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: expected JSON string", errInvalidScalar)
	}
	return value, nil
}

func validJSONSurrogates(raw []byte) bool {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '\\' {
			continue
		}
		index++
		if index >= len(raw) {
			return false
		}
		if raw[index] != 'u' {
			continue
		}
		first, ok := decodeHexCodeUnit(raw, index+1)
		if !ok {
			return false
		}
		index += 4
		if first >= 0xdc00 && first <= 0xdfff {
			return false
		}
		if first < 0xd800 || first > 0xdbff {
			continue
		}
		if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
			return false
		}
		second, ok := decodeHexCodeUnit(raw, index+3)
		if !ok || second < 0xdc00 || second > 0xdfff {
			return false
		}
		index += 6
	}
	return true
}

func decodeHexCodeUnit(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(raw[start:start+4]), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(value), true
}

func canonicalInt32(raw []byte) (string, error) {
	text := string(raw)
	if !jsonIntegerPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	if text == "-0" {
		return "", errInvalidScalar
	}
	value, err := strconv.ParseInt(text, 10, 32)
	if err != nil {
		return "", fmt.Errorf("%w: Int32 range", errInvalidScalar)
	}
	return strconv.FormatInt(value, 10), nil
}

func canonicalFloat64(raw []byte) (string, error) {
	text := string(raw)
	if !jsonNumberPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return "", fmt.Errorf("%w: finite Float64 required", errInvalidScalar)
	}
	if value == 0 {
		return "0", nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode Float64: %w", err)
	}
	return string(encoded), nil
}

func canonicalSignedString(raw []byte, bits int) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil || !integerPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	value, err := strconv.ParseInt(text, 10, bits)
	if err != nil {
		return "", fmt.Errorf("%w: signed integer range", errInvalidScalar)
	}
	return quote(strconv.FormatInt(value, 10)), nil
}

func canonicalUnsignedString(raw []byte, bits int) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil || !unsignedPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	value, err := strconv.ParseUint(text, 10, bits)
	if err != nil {
		return "", fmt.Errorf("%w: unsigned integer range", errInvalidScalar)
	}
	return quote(strconv.FormatUint(value, 10)), nil
}

func canonicalBigInt(raw []byte) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil || !integerPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return "", errInvalidScalar
	}
	return quote(value.String()), nil
}

func canonicalDecimal(raw []byte) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil || !decimalPattern.MatchString(text) {
		return "", errInvalidScalar
	}

	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	parts := strings.SplitN(text, ".", 2)
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	if fraction != "" {
		integer += "." + fraction
	}
	if negative && integer != "0" {
		integer = "-" + integer
	}
	return quote(integer), nil
}

func canonicalTimestamp(raw []byte) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil {
		return "", err
	}
	if !timestampPattern.MatchString(text) {
		return "", fmt.Errorf("%w: RFC3339 timestamp grammar", errInvalidScalar)
	}
	value, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return "", fmt.Errorf("%w: RFC3339 timestamp", errInvalidScalar)
	}
	utc := value.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 {
		return "", fmt.Errorf("%w: RFC3339 UTC year range", errInvalidScalar)
	}
	return quote(utc.Format(time.RFC3339Nano)), nil
}

func canonicalDuration(raw []byte) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil || !integerPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: duration range", errInvalidScalar)
	}
	return quote(strconv.FormatInt(value, 10)), nil
}

func canonicalUUID(raw []byte) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil || !uuidPattern.MatchString(text) {
		return "", errInvalidScalar
	}
	return quote(strings.ToLower(text)), nil
}

func canonicalBytes(raw []byte) (string, error) {
	text, err := parseJSONString(raw)
	if err != nil {
		return "", err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(text)
	if err == nil {
		return quote(base64.RawURLEncoding.EncodeToString(decoded)), nil
	}
	return "", fmt.Errorf("%w: base64 bytes", errInvalidScalar)
}

func quote(value string) string {
	return canonicalJSONString(value)
}

func canonicalJSONString(value string) string {
	const hexadecimal = "0123456789abcdef"
	var output strings.Builder
	output.WriteByte('"')
	for _, current := range value {
		switch current {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(current)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if current < 0x20 {
				output.WriteString(`\u00`)
				output.WriteByte(hexadecimal[byte(current)>>4])
				output.WriteByte(hexadecimal[byte(current)&0x0f])
			} else {
				output.WriteRune(current)
			}
		}
	}
	output.WriteByte('"')
	return output.String()
}
