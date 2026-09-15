// Package cbor implements Naatre's deterministic unary CBOR profile.
package cbor

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

const (
	// Profile is the exact capability selected by unary CBOR negotiation.
	Profile = "transport.cbor.unary-1"
	// CodecRevision identifies the deterministic mapping implemented here.
	CodecRevision = "cbor-det-1"

	RequestMediaType  = "application/vnd.naatre.request+cbor;profile=" + Profile + ";version=1"
	ResponseMediaType = "application/vnd.naatre.response+cbor;profile=" + Profile + ";version=1"

	tagTimestamp   uint64 = 0
	tagBigPos      uint64 = 2
	tagBigNeg      uint64 = 3
	tagDecimal     uint64 = 4
	tagUUID        uint64 = 37
	tagInt64       uint64 = 60000
	tagUInt64      uint64 = 60001
	tagDuration    uint64 = 60002
	maxSafeInteger int64  = 1<<53 - 1
)

// Limits bounds decoding work before a complete value is constructed.
// Zero fields select the finite defaults returned by DefaultLimits.
type Limits struct {
	MaxBytes           int
	MaxDepth           int
	MaxArrayItems      int
	MaxMapPairs        int
	MaxTextBytes       int
	MaxByteStringBytes int
	MaxBignumBytes     int
	MaxTags            int
	MaxAllocationBytes int
}

// DefaultLimits returns the cbor-det-1 decoder budgets.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:           1 << 20,
		MaxDepth:           64,
		MaxArrayItems:      100_000,
		MaxMapPairs:        10_000,
		MaxTextBytes:       1 << 20,
		MaxByteStringBytes: 1 << 20,
		MaxBignumBytes:     128,
		MaxTags:            16,
		MaxAllocationBytes: 8 << 20,
	}
}

// Error is a stable strict-decoder failure.
type Error struct {
	Code   string
	Offset int
}

func (e *Error) Error() string { return fmt.Sprintf("CBOR %s at byte %d", e.Code, e.Offset) }

// Missing represents an omitted object or map member. It is invalid at the
// top level and in lists because those positions cannot be missing.
type Missing struct{}

// ID and Enum retain their declared schema kinds while using CBOR text.
type ID string
type Enum string

// Int64 and UInt64 use profile tags so their JSON string representation is
// recoverable without schema-dependent decoding.
type Int64 int64
type UInt64 uint64

// BigInt is an arbitrary precision signed integer.
type BigInt struct{ value *big.Int }

// NewBigInt constructs an immutable BigInt copy.
func NewBigInt(value *big.Int) (BigInt, error) {
	if value == nil {
		return BigInt{}, errors.New("BigInt value is required")
	}
	return BigInt{value: new(big.Int).Set(value)}, nil
}

// ParseBigInt parses a canonical base-10 integer.
func ParseBigInt(value string) (BigInt, error) {
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok || parsed.String() != value || value == "-0" {
		return BigInt{}, errors.New("BigInt must use minimal decimal digits")
	}
	return NewBigInt(parsed)
}

// String returns the canonical decimal value.
func (v BigInt) String() string {
	if v.value == nil {
		return ""
	}
	return v.value.String()
}

// Decimal is an RFC 8949 decimal fraction: mantissa multiplied by ten raised
// to Exponent. The normalized form has no trailing mantissa zero.
type Decimal struct {
	Exponent int64
	mantissa *big.Int
}

// NewDecimal constructs and normalizes a decimal fraction.
func NewDecimal(exponent int64, mantissa *big.Int) (Decimal, error) {
	if mantissa == nil {
		return Decimal{}, errors.New("Decimal mantissa is required")
	}
	value := new(big.Int).Set(mantissa)
	if value.Sign() == 0 {
		return Decimal{mantissa: value}, nil
	}
	ten := big.NewInt(10)
	quotient, remainder := new(big.Int), new(big.Int)
	for {
		quotient.QuoRem(value, ten, remainder)
		if remainder.Sign() != 0 {
			break
		}
		if exponent == int64(^uint64(0)>>1) {
			return Decimal{}, errors.New("Decimal exponent overflow")
		}
		value.Set(quotient)
		exponent++
	}
	return Decimal{Exponent: exponent, mantissa: value}, nil
}

// Mantissa returns an immutable copy of the decimal coefficient.
func (d Decimal) Mantissa() *big.Int {
	if d.mantissa == nil {
		return nil
	}
	return new(big.Int).Set(d.mantissa)
}

// Timestamp is a UTC RFC 3339 timestamp with nanosecond precision.
type Timestamp struct{ time.Time }

// Duration is a signed arbitrary-precision count of nanoseconds.
type Duration struct{ nanoseconds *big.Int }

// NewDuration constructs an immutable duration count.
func NewDuration(nanoseconds *big.Int) (Duration, error) {
	if nanoseconds == nil {
		return Duration{}, errors.New("Duration nanoseconds are required")
	}
	return Duration{nanoseconds: new(big.Int).Set(nanoseconds)}, nil
}

// ParseDuration parses canonical signed integer nanoseconds.
func ParseDuration(value string) (Duration, error) {
	integer, err := ParseBigInt(value)
	if err != nil {
		return Duration{}, err
	}
	return NewDuration(integer.value)
}

// String returns canonical signed integer nanoseconds.
func (d Duration) String() string {
	if d.nanoseconds == nil {
		return ""
	}
	return d.nanoseconds.String()
}

// UUID is the RFC 4122 16-byte representation.
type UUID [16]byte

// Bytes is a native CBOR byte string.
type Bytes []byte

// Union is the exact open tagged-union structural value.
type Union struct {
	Type  string
	Value any
}
