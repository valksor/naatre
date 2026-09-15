package cbor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
)

func TestMarshalDeterministicVectors(t *testing.T) {
	t.Parallel()
	large, _ := ParseBigInt("18446744073709551616")
	decimal, _ := NewDecimal(-2, big.NewInt(123))
	duration, _ := ParseDuration("-5")
	uuid := UUID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	negativeZero := math.Copysign(0, -1)

	tests := []struct {
		name  string
		value any
		hex   string
	}{
		{"null", nil, "f6"},
		{"boolean", true, "f5"},
		{"string", "Naatre", "664e6161747265"},
		{"int32-min", int32(math.MinInt32), "3a7fffffff"},
		{"int32-max", int32(math.MaxInt32), "1a7fffffff"},
		{"int64-max", Int64(math.MaxInt64), "d9ea601b7fffffffffffffff"},
		{"uint64-max", UInt64(math.MaxUint64), "d9ea611bffffffffffffffff"},
		{"bigint", large, "c249010000000000000000"},
		{"decimal", decimal, "c48221187b"},
		{"binary64", 1.5, "fb3ff8000000000000"},
		{"negative-zero-normalized", negativeZero, "fb0000000000000000"},
		{"duration", duration, "d9ea6224"},
		{"uuid", uuid, "d8255000112233445566778899aabbccddeeff"},
		{"bytes", Bytes{0x00, 0xff}, "4200ff"},
		{"empty-list", []any{}, "80"},
		{"empty-map", map[string]any{}, "a0"},
		{"non-bmp-map-order", map[string]any{"😀": int32(2), "a": int32(1)}, "a261610164f09f988002"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := Marshal(testCase.value)
			if err != nil {
				t.Fatal(err)
			}
			if actual := hex.EncodeToString(encoded); actual != testCase.hex {
				t.Fatalf("Marshal() = %s, want %s", actual, testCase.hex)
			}
			decoded, err := Unmarshal(encoded, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(roundTrip, encoded) {
				t.Fatalf("round trip = %x, want %x", roundTrip, encoded)
			}
		})
	}
}

func TestTimestampPrecisionAndJSONProjection(t *testing.T) {
	t.Parallel()
	instant, err := time.Parse(time.RFC3339Nano, "2026-09-15T10:11:12.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Marshal(Timestamp{Time: instant})
	if err != nil {
		t.Fatal(err)
	}
	jsonBytes, err := DecodeJSON(encoded, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if string(jsonBytes) != `"2026-09-15T10:11:12.123456789Z"` {
		t.Fatalf("timestamp JSON = %s", jsonBytes)
	}
}

func TestExtendedScalarJSONProjection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hex  string
		json string
	}{
		{"int64", "d9ea601b7fffffffffffffff", `"9223372036854775807"`},
		{"uint64", "d9ea611bffffffffffffffff", `"18446744073709551615"`},
		{"bigint", "c249010000000000000000", `"18446744073709551616"`},
		{"decimal", "c48221187b", `"1.23"`},
		{"duration", "d9ea6224", `"-5"`},
		{"uuid", "d8255000112233445566778899aabbccddeeff", `"00112233-4455-6677-8899-aabbccddeeff"`},
		{"bytes", "4200ff", `"AP8"`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			input, _ := hex.DecodeString(testCase.hex)
			projected, err := DecodeJSON(input, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if string(projected) != testCase.json {
				t.Fatalf("DecodeJSON() = %s, want %s", projected, testCase.json)
			}
		})
	}
}

func TestJSONAndCBORSemanticDocumentIdentity(t *testing.T) {
	t.Parallel()
	jsonDocument := []byte(`{"operations":[{"select":[],"kind":"query","name":"Q"}]}`)
	canonicalJSON, err := protocol.CanonicalizeDocument(jsonDocument, protocol.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	jsonValue, err := protocol.DecodeJSONValue(jsonDocument)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Marshal(jsonValue)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := DecodeJSON(encoded, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	canonicalCBOR, err := protocol.CanonicalizeDocument(projected, protocol.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	jsonIdentity, _ := protocol.SemanticHash(protocol.DocumentHash, canonicalJSON)
	cborIdentity, _ := protocol.SemanticHash(protocol.DocumentHash, canonicalCBOR)
	if jsonIdentity != cborIdentity {
		t.Fatalf("semantic hashes differ: %#v != %#v", jsonIdentity, cborIdentity)
	}
	if sha256.Sum256(jsonDocument) == sha256.Sum256(encoded) {
		t.Fatal("representation digests unexpectedly match")
	}
}

func TestUnmarshalRejectsNonDeterministicAndMalformedForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hex  string
		code string
	}{
		{"duplicate-key", "a2616101616102", "DUPLICATE_KEY"},
		{"forbidden-tag", "d82a00", "FORBIDDEN_TAG"},
		{"non-preferred-zero", "1800", "NON_PREFERRED_INTEGER"},
		{"invalid-text", "61ff", "INVALID_UTF8"},
		{"indefinite-list", "9fff", "INDEFINITE_LENGTH"},
		{"float16", "f93e00", "FLOAT_WIDTH"},
		{"negative-zero", "fb8000000000000000", "NON_PREFERRED_FLOAT"},
		{"non-string-key", "a10102", "NON_STRING_KEY"},
		{"map-order", "a264f09f988002616101", "MAP_ORDER"},
		{"truncated", "a16161", "TRUNCATED"},
		{"trailing", "f6f6", "TRAILING_DATA"},
		{"nested-int64-tag", "d9ea60d9ea6001", "INVALID_TAG_CONTENT"},
		{"non-preferred-bignum", "c2420001", "NON_PREFERRED_BIGNUM"},
		{"non-preferred-decimal", "c482001878", "NON_PREFERRED_DECIMAL"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			input, err := hex.DecodeString(testCase.hex)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Unmarshal(input, Limits{})
			var decoderError *Error
			if !errors.As(err, &decoderError) || decoderError.Code != testCase.code {
				t.Fatalf("Unmarshal() error = %v, want %s", err, testCase.code)
			}
		})
	}
}

func TestUnmarshalEnforcesBudgetsAndCancellation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		hex    string
		limits Limits
		code   string
	}{
		{"depth", "818180", Limits{MaxDepth: 2}, "LIMIT_DEPTH"},
		{"bignum", "c249010000000000000000", Limits{MaxBignumBytes: 8}, "LIMIT_BIGNUM"},
		{"array", "83010203", Limits{MaxArrayItems: 2}, "LIMIT_ARRAY"},
		{"map", "a2616101616202", Limits{MaxMapPairs: 1}, "LIMIT_MAP"},
		{"allocation", "8261616162", Limits{MaxAllocationBytes: 33}, "LIMIT_ALLOCATION"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			input, _ := hex.DecodeString(testCase.hex)
			_, err := Unmarshal(input, testCase.limits)
			var decoderError *Error
			if !errors.As(err, &decoderError) || decoderError.Code != testCase.code {
				t.Fatalf("Unmarshal() error = %v, want %s", err, testCase.code)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := UnmarshalContext(ctx, []byte{0xf6}, Limits{})
	var decoderError *Error
	if !errors.As(err, &decoderError) || decoderError.Code != "CANCELLED" {
		t.Fatalf("cancelled decode error = %v", err)
	}
}

func TestMissingIsOmittedOnlyFromMaps(t *testing.T) {
	t.Parallel()
	encoded, err := Marshal(map[string]any{"missing": Missing{}, "null": nil})
	if err != nil {
		t.Fatal(err)
	}
	if actual := hex.EncodeToString(encoded); actual != "a1646e756c6cf6" {
		t.Fatalf("missing map = %s", actual)
	}
	if _, err := Marshal([]any{Missing{}}); err == nil {
		t.Fatal("missing list element was accepted")
	}
}

func FuzzUnmarshal(f *testing.F) {
	for _, seed := range []string{"f6", "a2616101616202", "c48221187b", "c482303b8230303030303030", "a16161", "9fff"} {
		input, err := hex.DecodeString(seed)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		value, err := Unmarshal(input, Limits{MaxBytes: 4096, MaxArrayItems: 128, MaxMapPairs: 128, MaxAllocationBytes: 16 << 10})
		if err != nil {
			return
		}
		encoded, err := Marshal(value)
		if err != nil {
			t.Fatalf("accepted value cannot be re-encoded: %v", err)
		}
		if !reflect.DeepEqual(encoded, input) {
			t.Fatalf("accepted non-deterministic form: input=%x encoded=%x", input, encoded)
		}
	})
}
