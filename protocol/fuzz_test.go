package protocol_test

import (
	"bytes"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func FuzzStrictDecoder(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"version":"1","document":{"operations":[]}}`),
		[]byte(`{"a":1,"\u0061":2}`),
		[]byte(`{"operations":[],"\u006fperations":[]}`),
		[]byte(`{"x":"\ud800"}`),
		{'{', '"', 'x', '"', ':', '"', 0xe2, 0x82},
		[]byte(`{"x":"�"}`),
		[]byte(`{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"echo","args":{"value":{"$literal":{"$var":"inert"}}}}}]}]}`),
		[]byte(`[[[[[[[[[]]]]]]]]]`),
		[]byte(`1e999999999999999999999`),
		[]byte(`{"operations":[{"name":"Q","kind":"query","select":[{"$index":{"as":"i","at":1e1000000}}]}]}`),
		[]byte(`{"operations":[`),
		bytes.Repeat([]byte{'['}, 128),
		bytes.Repeat([]byte(`{"x":`), 128),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		options := protocol.DecodeOptions{Limits: protocol.Limits{MaxBytes: 4096, MaxTokens: 512, MaxDepth: 16, MaxStringBytes: 1024, MaxMembers: 128, MaxArrayItems: 128, MaxNumberBytes: 64, MaxLiteralBytes: 1024}}
		_, _ = protocol.DecodeRequest(input, options)
		_, _ = protocol.DecodeResponse(input, options)
		document, documentErr := protocol.DecodeDocument(input, options.Limits)
		if documentErr == nil {
			canonical, canonicalErr := protocol.CanonicalizeDocument(input, options.Limits)
			if canonicalErr != nil || !bytes.Equal(document.CanonicalJSON(), canonical) {
				t.Fatalf("decoded AST canonical mismatch: %v", canonicalErr)
			}
			roundTrip, roundTripErr := protocol.DecodeDocument(document.CanonicalJSON(), options.Limits)
			if roundTripErr != nil || !bytes.Equal(roundTrip.CanonicalJSON(), canonical) {
				t.Fatalf("canonical AST round trip mismatch: %v", roundTripErr)
			}
		}
		_, _ = protocol.CanonicalizeJSON(input, options.Limits)
	})
}
