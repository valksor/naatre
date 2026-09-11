package schema_test

import (
	"encoding/json"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestPresenceDistinguishesMissingNullAndValue(t *testing.T) {
	t.Parallel()

	missing := schema.Missing(schema.String)
	if !missing.IsMissing() || missing.IsNull() {
		t.Fatalf("Missing(String) state = %v, want missing only", missing.Presence())
	}

	null := schema.Null(schema.String)
	if null.IsMissing() || !null.IsNull() {
		t.Fatalf("Null(String) state = %v, want null only", null.Presence())
	}

	value, err := schema.ParseScalar(schema.String, json.RawMessage(`"value"`))
	if err != nil {
		t.Fatalf("ParseScalar(String): %v", err)
	}
	if value.IsMissing() || value.IsNull() {
		t.Fatalf("parsed value state = %v, want present value", value.Presence())
	}
}

func TestParseScalarUsesLosslessExtendedNumberStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind schema.ScalarKind
		in   string
		want string
	}{
		{name: "int64 boundary", kind: schema.Int64, in: `"-9223372036854775808"`, want: `"-9223372036854775808"`},
		{name: "uint64 boundary", kind: schema.UInt64, in: `"18446744073709551615"`, want: `"18446744073709551615"`},
		{name: "big integer", kind: schema.BigInt, in: `"0009007199254740993"`, want: `"9007199254740993"`},
		{name: "decimal scale", kind: schema.Decimal, in: `"-001.23000"`, want: `"-1.23"`},
		{name: "decimal negative zero", kind: schema.Decimal, in: `"-0.000"`, want: `"0"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value, err := schema.ParseScalar(tt.kind, json.RawMessage(tt.in))
			if err != nil {
				t.Fatalf("ParseScalar(%s, %s): %v", tt.kind, tt.in, err)
			}
			got, err := value.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("canonical JSON = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseScalarNormalizesPortableEncodings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind schema.ScalarKind
		in   string
		want string
	}{
		{name: "float negative zero", kind: schema.Float64, in: `-0`, want: `0`},
		{name: "timestamp offset and precision", kind: schema.Timestamp, in: `"2026-09-11T23:30:01.123456789+03:00"`, want: `"2026-09-11T20:30:01.123456789Z"`},
		{name: "uuid case", kind: schema.UUID, in: `"550E8400-E29B-41D4-A716-446655440000"`, want: `"550e8400-e29b-41d4-a716-446655440000"`},
		{name: "bytes raw base64url", kind: schema.Bytes, in: `"SGVsbG8"`, want: `"SGVsbG8"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value, err := schema.ParseScalar(tt.kind, json.RawMessage(tt.in))
			if err != nil {
				t.Fatalf("ParseScalar(%s, %s): %v", tt.kind, tt.in, err)
			}
			got, err := value.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("canonical JSON = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseScalarRejectsLossyOrInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind schema.ScalarKind
		in   string
	}{
		{name: "numeric int64", kind: schema.Int64, in: `9007199254740993`},
		{name: "int64 overflow", kind: schema.Int64, in: `"9223372036854775808"`},
		{name: "uint64 negative", kind: schema.UInt64, in: `"-1"`},
		{name: "non-finite", kind: schema.Float64, in: `"NaN"`},
		{name: "leap second", kind: schema.Timestamp, in: `"2016-12-31T23:59:60Z"`},
		{name: "bad UUID", kind: schema.UUID, in: `"not-a-uuid"`},
		{name: "bad bytes", kind: schema.Bytes, in: `"***"`},
		{name: "padded bytes", kind: schema.Bytes, in: `"SGVsbG8="`},
		{name: "unknown null", kind: schema.ScalarKind("Unknown"), in: `null`},
		{name: "int leading zero", kind: schema.Int32, in: `01`},
		{name: "int negative zero", kind: schema.Int32, in: `-0`},
		{name: "float plus", kind: schema.Float64, in: `+1`},
		{name: "float leading dot", kind: schema.Float64, in: `.5`},
		{name: "unpaired high surrogate", kind: schema.String, in: `"\ud800"`},
		{name: "unpaired low surrogate", kind: schema.String, in: `"\udc00"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := schema.ParseScalar(tt.kind, json.RawMessage(tt.in)); err == nil {
				t.Fatalf("ParseScalar(%s, %s) succeeded, want error", tt.kind, tt.in)
			}
		})
	}
}
