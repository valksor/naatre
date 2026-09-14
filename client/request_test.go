package client

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/valksor/naatre/protocol"
)

const testDocument = `{"operations":[{"name":"Ping","kind":"query","variables":[{"name":"amount","type":"Decimal"}],"select":[{"$call":{"name":"ping","args":{"amount":{"$var":"amount"}}}}]}]}`

func TestRequestIsImmutableCanonicalAndPersistable(t *testing.T) {
	t.Parallel()
	document, err := protocol.DecodeDocument([]byte(testDocument), protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	base, err := NewRequest(document, "Ping")
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	first, err := base.WithVariable("amount", json.RawMessage(`"9007199254740993.000000000000000001"`))
	if err != nil {
		t.Fatalf("WithVariable: %v", err)
	}
	first, err = first.WithVariable("empty", json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("WithVariable empty: %v", err)
	}
	second, err := base.WithVariable("empty", json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("WithVariable empty second: %v", err)
	}
	second, err = second.WithVariable("amount", json.RawMessage(`"9007199254740993.000000000000000001"`))
	if err != nil {
		t.Fatalf("WithVariable amount second: %v", err)
	}
	firstJSON, err := first.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON first: %v", err)
	}
	secondJSON, err := second.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON second: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("canonical requests differ:\n%s\n%s", firstJSON, secondJSON)
	}
	baseJSON, err := base.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON base: %v", err)
	}
	if bytes.Contains(baseJSON, []byte("amount"+`\":\"9007199254740993`)) {
		t.Fatalf("base request was mutated: %s", baseJSON)
	}
	decoded, err := protocol.DecodeRequest(firstJSON, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	amount, present := decoded.Variable("amount")
	if !present || string(amount) != `"9007199254740993.000000000000000001"` {
		t.Fatalf("lossless scalar = %s, present %v", amount, present)
	}
	digest, err := first.DocumentDigest()
	if err != nil {
		t.Fatalf("DocumentDigest: %v", err)
	}
	want, err := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	if digest != want {
		t.Fatalf("digest = %#v, want %#v", digest, want)
	}
	persisted, err := NewPersistedRequest(protocol.PersistedReference{
		Algorithm: digest.Algorithm, CanonicalVersion: digest.CanonicalVersion, Digest: digest.Hex,
	}, "Ping")
	if err != nil {
		t.Fatalf("NewPersistedRequest: %v", err)
	}
	persistedJSON, err := persisted.CanonicalJSON()
	if err != nil {
		t.Fatalf("persisted CanonicalJSON: %v", err)
	}
	if _, err := protocol.DecodeRequest(persistedJSON, protocol.DecodeOptions{}); err != nil {
		t.Fatalf("persisted DecodeRequest: %v", err)
	}
}

func TestRequestRejectsInvalidValuesWithoutMutatingBase(t *testing.T) {
	t.Parallel()
	document, err := protocol.DecodeDocument([]byte(testDocument), protocol.Limits{})
	if err != nil {
		t.Fatalf("DecodeDocument: %v", err)
	}
	request, err := NewRequest(document, "Ping")
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for name, raw := range map[string]json.RawMessage{
		"bad-name": json.RawMessage(`1`),
		"valid":    json.RawMessage(`{"duplicate":1,"duplicate":2}`),
	} {
		if _, err := request.WithVariable(name, raw); err == nil {
			t.Fatalf("WithVariable(%q) accepted invalid input", name)
		}
	}
	if _, err := request.WithID(""); err == nil {
		t.Fatal("WithID accepted empty ID")
	}
	if _, err := request.WithCapabilities("streaming", "streaming"); err == nil {
		t.Fatal("WithCapabilities accepted duplicate capability")
	}
	if _, err := NewRequest(document, "Missing"); err == nil {
		t.Fatal("NewRequest accepted absent operation")
	}
	if _, err := NewPersistedRequest(protocol.PersistedReference{Algorithm: "sha-256", CanonicalVersion: "c14n-1", Digest: "bad"}, "Ping"); err == nil {
		t.Fatal("NewPersistedRequest accepted malformed digest")
	}
}
