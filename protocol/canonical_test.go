package protocol_test

import (
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestCanonicalizeJSONUsesUTF16KeyOrderWithoutUnicodeNormalization(t *testing.T) {
	t.Parallel()

	input := []byte("{\"\\ue000\":2,\"😀\":1,\"object\":{},\"a\":\"e\\u0301\",\"empty\":[]}")
	canonical, err := protocol.CanonicalizeJSON(input, protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeJSON: %v", err)
	}
	want := "{\"a\":\"é\",\"empty\":[],\"object\":{},\"😀\":1,\"\":2}"
	if string(canonical) != want {
		t.Fatalf("canonical JSON = %s, want %s", canonical, want)
	}
}

func TestCanonicalizeJSONPreservesArraysAndDistinguishesEmptyContainers(t *testing.T) {
	t.Parallel()

	canonical, err := protocol.CanonicalizeJSON([]byte(`{"z":[],"a":{},"ordered":[3,2,1]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeJSON: %v", err)
	}
	if got, want := string(canonical), `{"a":{},"ordered":[3,2,1],"z":[]}`; got != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}
}

func TestCanonicalizeJSONPreservesReplacementCharacter(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`{"x":"�"}`, `{"x":"\ufffd"}`, `{"�":1,"":2}`} {
		canonical, err := protocol.CanonicalizeJSON([]byte(input), protocol.Limits{})
		if err != nil {
			t.Fatalf("CanonicalizeJSON(%s): %v", input, err)
		}
		if !strings.Contains(string(canonical), "�") {
			t.Fatalf("canonical JSON deleted U+FFFD: %s", canonical)
		}
	}
}

func TestSemanticHashPinsDomainVersionAndDigest(t *testing.T) {
	t.Parallel()

	digest, err := protocol.SemanticHash(protocol.DocumentHash, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	if digest.Algorithm != "sha-256" || digest.CanonicalVersion != "c14n-1" {
		t.Fatalf("digest metadata = %#v", digest)
	}
	if want := "b809e62e9151db9918fb436114be52cefba449c768fd9ee800b22b9eba490428"; digest.Hex != want {
		t.Fatalf("digest = %s, want %s", digest.Hex, want)
	}

	approval, err := protocol.SemanticHash(protocol.ApprovalHash, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("approval SemanticHash: %v", err)
	}
	if approval.Hex == digest.Hex {
		t.Fatal("different hash purposes produced the same digest")
	}
}

func TestCanonicalizeJSONRejectsDuplicateAndInvalidInput(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`{"a":1,"\u0061":2}`,
		`{"a":"\ud800"}`,
		`{"a":1} false`,
	} {
		if _, err := protocol.CanonicalizeJSON([]byte(input), protocol.Limits{}); err == nil {
			t.Fatalf("CanonicalizeJSON(%s) succeeded, want error", input)
		}
	}
}
