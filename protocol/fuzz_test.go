package protocol_test

import (
	"testing"

	"github.com/valksor/naatre/protocol"
)

func FuzzStrictDecoder(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"version":"1","document":{"operations":[]}}`),
		[]byte(`{"a":1,"\u0061":2}`),
		[]byte(`{"x":"\ud800"}`),
		[]byte(`{"x":"�"}`),
		[]byte(`[[[[[[[[[]]]]]]]]]`),
		[]byte(`1e999999999999999999999`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		options := protocol.DecodeOptions{Limits: protocol.Limits{MaxBytes: 4096, MaxTokens: 512, MaxDepth: 16, MaxStringBytes: 1024, MaxMembers: 128, MaxArrayItems: 128, MaxNumberBytes: 64}}
		_, _ = protocol.DecodeRequest(input, options)
		_, _ = protocol.CanonicalizeJSON(input, options.Limits)
	})
}
