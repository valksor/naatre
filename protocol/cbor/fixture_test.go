package cbor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

type goldenFixture struct {
	Profile       string `json:"profile"`
	CodecRevision string `json:"codecRevision"`
	Vectors       []struct {
		Name     string          `json:"name"`
		Type     string          `json:"type"`
		Value    json.RawMessage `json:"value"`
		Exponent int64           `json:"exponent"`
		Mantissa string          `json:"mantissa"`
		Hex      string          `json:"hex"`
	} `json:"vectors"`
	Rejections []struct {
		Name      string `json:"name"`
		Hex       string `json:"hex"`
		Code      string `json:"code"`
		Cancelled bool   `json:"cancelled"`
		Limits    struct {
			Depth       int `json:"depth"`
			BignumBytes int `json:"bignumBytes"`
		} `json:"limits"`
	} `json:"rejections"`
	SemanticIdentity struct {
		CanonicalJSON            string `json:"canonicalJSON"`
		CBORHex                  string `json:"cborHex"`
		DocumentHash             string `json:"documentHash"`
		JSONRepresentationSHA256 string `json:"jsonRepresentationSHA256"`
		CBORRepresentationSHA256 string `json:"cborRepresentationSHA256"`
	} `json:"semanticIdentity"`
}

func TestLanguageNeutralGoldenFixture(t *testing.T) {
	t.Parallel()
	fixture := loadGoldenFixture(t)
	if fixture.Profile != Profile || fixture.CodecRevision != CodecRevision {
		t.Fatalf("fixture profile = %s@%s", fixture.Profile, fixture.CodecRevision)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			value := fixtureValue(t, vector.Type, vector.Value, vector.Exponent, vector.Mantissa)
			encoded, err := Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if actual := hex.EncodeToString(encoded); actual != vector.Hex {
				t.Fatalf("golden bytes = %s, want %s", actual, vector.Hex)
			}
		})
	}
	for _, rejection := range fixture.Rejections {
		t.Run(rejection.Name, func(t *testing.T) {
			t.Parallel()
			input, err := hex.DecodeString(rejection.Hex)
			if err != nil {
				t.Fatal(err)
			}
			limits := Limits{MaxDepth: rejection.Limits.Depth, MaxBignumBytes: rejection.Limits.BignumBytes}
			ctx := context.Background()
			if rejection.Cancelled {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, err = UnmarshalContext(ctx, input, limits)
			var decoderError *Error
			if !errors.As(err, &decoderError) || decoderError.Code != rejection.Code {
				t.Fatalf("rejection = %v, want %s", err, rejection.Code)
			}
		})
	}
}

func TestGoldenSemanticAndRepresentationDigests(t *testing.T) {
	t.Parallel()
	fixture := loadGoldenFixture(t)
	canonical := []byte(fixture.SemanticIdentity.CanonicalJSON)
	cborBytes, err := hex.DecodeString(fixture.SemanticIdentity.CBORHex)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := DecodeJSON(cborBytes, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if string(projected) != string(canonical) {
		t.Fatalf("CBOR projection = %s, want %s", projected, canonical)
	}
	digest, err := protocol.SemanticHash(protocol.DocumentHash, projected)
	if err != nil || digest.Hex != fixture.SemanticIdentity.DocumentHash {
		t.Fatalf("document hash = %s, %v", digest.Hex, err)
	}
	jsonSum, cborSum := sha256.Sum256(canonical), sha256.Sum256(cborBytes)
	if hex.EncodeToString(jsonSum[:]) != fixture.SemanticIdentity.JSONRepresentationSHA256 ||
		hex.EncodeToString(cborSum[:]) != fixture.SemanticIdentity.CBORRepresentationSHA256 || jsonSum == cborSum {
		t.Fatal("representation digests do not match the independent fixture")
	}
}

func loadGoldenFixture(t *testing.T) goldenFixture {
	t.Helper()
	content, err := os.ReadFile("../../conformance/v1/cbor.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFixture
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func fixtureValue(t *testing.T, kind string, raw json.RawMessage, exponent int64, mantissa string) any {
	t.Helper()
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			t.Fatal(err)
		}
	}
	switch kind {
	case "Null":
		return nil
	case "Boolean":
		return true
	case "String":
		return text
	case "ID":
		return ID(text)
	case "Int32":
		value, err := strconv.ParseInt(string(raw), 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		return int32(value)
	case "BoundedInteger":
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return value
	case "Int64":
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return Int64(value)
	case "UInt64":
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return UInt64(value)
	case "BigInt":
		value, err := ParseBigInt(text)
		if err != nil {
			t.Fatal(err)
		}
		return value
	case "Decimal":
		coefficient, ok := new(big.Int).SetString(mantissa, 10)
		if !ok {
			t.Fatal("invalid fixture Decimal")
		}
		value, err := NewDecimal(exponent, coefficient)
		if err != nil {
			t.Fatal(err)
		}
		return value
	case "Float64":
		if text == "-0" {
			return math.Copysign(0, -1)
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		return value
	case "Timestamp":
		value, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			t.Fatal(err)
		}
		return Timestamp{Time: value}
	case "Duration":
		value, err := ParseDuration(text)
		if err != nil {
			t.Fatal(err)
		}
		return value
	case "UUID":
		bytes, err := hex.DecodeString(strings.ReplaceAll(text, "-", ""))
		if err != nil || len(bytes) != 16 {
			t.Fatalf("invalid fixture UUID: %v", err)
		}
		var value UUID
		copy(value[:], bytes)
		return value
	case "Bytes":
		value, err := base64.RawURLEncoding.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		return Bytes(value)
	case "List", "Map":
		value, err := protocol.DecodeJSONValue(raw)
		if err != nil {
			t.Fatal(err)
		}
		return value
	case "Enum":
		return Enum(text)
	case "Union":
		var value struct {
			Type  string          `json:"$type"`
			Value json.RawMessage `json:"$value"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		inner, err := protocol.DecodeJSONValue(value.Value)
		if err != nil {
			t.Fatal(err)
		}
		return Union{Type: value.Type, Value: inner}
	case "object":
		return map[string]any{"missing": Missing{}, "null": nil}
	default:
		t.Fatalf("unknown fixture type %q", kind)
		return nil
	}
}
