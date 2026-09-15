package cbor

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
)

// DecodeJSON converts deterministic CBOR into the canonical JSON form consumed
// by Naatre's encoding-neutral protocol decoder.
func DecodeJSON(input []byte, limits Limits) ([]byte, error) {
	return DecodeJSONContext(context.Background(), input, limits)
}

// DecodeJSONContext is DecodeJSON with cooperative cancellation.
func DecodeJSONContext(ctx context.Context, input []byte, limits Limits) ([]byte, error) {
	resolved, err := resolveLimits(limits)
	if err != nil {
		return nil, err
	}
	value, err := UnmarshalContext(ctx, input, limits)
	if err != nil {
		return nil, err
	}
	jsonValue, err := jsonValue(value, resolved.MaxAllocationBytes)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(jsonValue)
	if err != nil {
		return nil, err
	}
	if len(encoded) > resolved.MaxAllocationBytes {
		return nil, &Error{Code: "LIMIT_ALLOCATION"}
	}
	return encoded, nil
}

func jsonValue(value any, allocationLimit int) (any, error) {
	switch current := value.(type) {
	case nil, bool, string, int32, int64, uint64, float64:
		return current, nil
	case ID:
		return string(current), nil
	case Enum:
		return string(current), nil
	case Int64:
		return big.NewInt(int64(current)).String(), nil
	case UInt64:
		return new(big.Int).SetUint64(uint64(current)).String(), nil
	case BigInt:
		if current.value == nil {
			return nil, errors.New("invalid BigInt")
		}
		return current.value.String(), nil
	case Decimal:
		return decimalString(current, allocationLimit)
	case Timestamp:
		return current.UTC().Format(time.RFC3339Nano), nil
	case Duration:
		if current.nanoseconds == nil {
			return nil, errors.New("invalid Duration")
		}
		return current.nanoseconds.String(), nil
	case UUID:
		return uuidString(current), nil
	case Bytes:
		return base64.RawURLEncoding.EncodeToString(current), nil
	case []byte:
		return base64.RawURLEncoding.EncodeToString(current), nil
	case []any:
		result := make([]any, len(current))
		for index, item := range current {
			converted, err := jsonValue(item, allocationLimit)
			if err != nil {
				return nil, err
			}
			result[index] = converted
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, item := range current {
			converted, err := jsonValue(item, allocationLimit)
			if err != nil {
				return nil, err
			}
			result[key] = converted
		}
		return result, nil
	default:
		return nil, errors.New("CBOR value has no JSON representation")
	}
}

func decimalString(value Decimal, allocationLimit int) (string, error) {
	if value.mantissa == nil {
		return "", errors.New("invalid Decimal")
	}
	if value.mantissa.Sign() == 0 {
		return "0", nil
	}
	negative := value.mantissa.Sign() < 0
	digits := new(big.Int).Abs(value.mantissa).String()
	var result string
	switch {
	case value.Exponent >= 0:
		if value.Exponent > int64(min(maxInt(), allocationLimit)-len(digits)) {
			return "", errors.New("Decimal JSON form exceeds allocation limit")
		}
		result = digits + strings.Repeat("0", int(value.Exponent))
	case value.Exponent < -int64(min(maxInt(), allocationLimit)):
		return "", errors.New("Decimal JSON form exceeds allocation limit")
	case -value.Exponent < int64(len(digits)):
		position := len(digits) + int(value.Exponent)
		result = digits[:position] + "." + digits[position:]
	default:
		zeros := -value.Exponent - int64(len(digits))
		if zeros > int64(min(maxInt(), allocationLimit)-len(digits)-2) {
			return "", errors.New("Decimal JSON form exceeds allocation limit")
		}
		result = "0." + strings.Repeat("0", int(zeros)) + digits
	}
	if negative {
		result = "-" + result
	}
	return result, nil
}

func uuidString(value UUID) string {
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded)
}
