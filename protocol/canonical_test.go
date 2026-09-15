package protocol_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestCanonicalizeJSONRejectsBareUnsafeIntegers(t *testing.T) {
	t.Parallel()
	// Bare integer literals outside the JS-safe range must be rejected so large
	// integers travel as string scalars, matching the PHP/TS/Python/Dart/CBOR
	// paths. Safe-range integers and the string form remain accepted.
	rejected := []string{
		`{"value":9007199254740992}`,    // 2^53 (first unsafe positive)
		`{"value":9007199254740993}`,    // 2^53 + 1
		`{"value":-9007199254740992}`,   // first unsafe negative
		`[1,2,99999999999999999999999]`, // nested, far beyond int64
	}
	for _, input := range rejected {
		var diagnostic *protocol.Diagnostic
		if _, err := protocol.CanonicalizeJSON([]byte(input), protocol.Limits{}); err == nil || !errors.As(err, &diagnostic) || diagnostic.Code != "MALFORMED_JSON" {
			t.Fatalf("CanonicalizeJSON(%s) = %v, want MALFORMED_JSON diagnostic", input, err)
		}
	}
	accepted := map[string]string{
		`{"value":9007199254740991}`:   `{"value":9007199254740991}`,   // 2^53 - 1 (max safe)
		`{"value":-9007199254740991}`:  `{"value":-9007199254740991}`,  // min safe
		`{"value":"9007199254740993"}`: `{"value":"9007199254740993"}`, // string scalar stays exact
		`{"value":1e21}`:               `{"value":1e+21}`,              // float literal, not a bare integer
	}
	for input, want := range accepted {
		canonical, err := protocol.CanonicalizeJSON([]byte(input), protocol.Limits{})
		if err != nil || string(canonical) != want {
			t.Fatalf("CanonicalizeJSON(%s) = %s, %v; want %s", input, canonical, err, want)
		}
	}
}

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
	underflow, err := protocol.CanonicalizeJSON([]byte(`{"negative":-1e-400,"positive":1e-400}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeJSON finite underflow: %v", err)
	}
	if got, want := string(underflow), `{"negative":0,"positive":0}`; got != want {
		t.Fatalf("canonical underflow = %s, want %s", got, want)
	}
}

func TestCanonicalizeDocumentSortsOnlyRequiredCapabilities(t *testing.T) {
	t.Parallel()

	input := []byte(`{"requires":["vendor.audit-1","core.language-1"],"operations":[{"select":[{"$field":{"name":"b"}},{"$field":{"name":"a"}}],"name":"Q","kind":"query"}]}`)
	canonical, err := protocol.CanonicalizeDocument(input, protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeDocument: %v", err)
	}
	want := `{"operations":[{"kind":"query","name":"Q","select":[{"$field":{"name":"b"}},{"$field":{"name":"a"}}]}],"requires":["core.language-1","vendor.audit-1"]}`
	if string(canonical) != want {
		t.Fatalf("canonical document = %s, want %s", canonical, want)
	}
}

func TestCanonicalizeDocumentRejectsInvalidRequiredCapabilities(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`[]`,
		`{"operations":[],"requires":"core.language-1"}`,
		`{"operations":[],"requires":[1]}`,
		`{"operations":[],"requires":["bad capability"]}`,
		`{"operations":[],"requires":["é"]}`,
		`{"operations":[],"requires":["core.language-1","core.language-1"]}`,
	} {
		if _, err := protocol.CanonicalizeDocument([]byte(input), protocol.Limits{}); err == nil {
			t.Fatalf("CanonicalizeDocument(%s) succeeded, want error", input)
		}
	}
}

func TestCanonicalizeSchemaSortsExtensionDescriptorsAndSets(t *testing.T) {
	t.Parallel()
	input := []byte(`{"revision":"schema-r1","types":[],"operations":[],"members":[],"extensions":[{"id":"org.example.zeta","points":["response","validation"],"directives":["zeta","alpha"]},{"id":"com.example.alpha","points":["validation"]}]}`)
	canonical, err := protocol.CanonicalizeSchema(input, protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeSchema: %v", err)
	}
	want := `{"extensions":[{"id":"com.example.alpha","points":["validation"]},{"directives":["alpha","zeta"],"id":"org.example.zeta","points":["response","validation"]}],"members":[],"operations":[],"revision":"schema-r1","types":[]}`
	if string(canonical) != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}
}

func TestCanonicalizeSchemaNormalizesRegistrationAndSetOrder(t *testing.T) {
	t.Parallel()

	left := []byte(`{"revision":"schema-7","capabilities":["vendor.audit-1","core.language-1"],"types":[{"id":"Result","kind":"union","variants":["User","Admin"]},{"id":"Slug","kind":"scalar","scalar":{"acceptedWireShapes":["string","number"]}},{"id":"Role","kind":"enum","enumValues":["USER","ADMIN"]}]}`)
	right := []byte(`{"types":[{"enumValues":["ADMIN","USER"],"kind":"enum","id":"Role"},{"scalar":{"acceptedWireShapes":["number","string"]},"kind":"scalar","id":"Slug"},{"variants":["Admin","User"],"kind":"union","id":"Result"}],"capabilities":["core.language-1","vendor.audit-1"],"revision":"schema-7"}`)
	leftCanonical, leftErr := protocol.CanonicalizeSchema(left, protocol.Limits{})
	rightCanonical, rightErr := protocol.CanonicalizeSchema(right, protocol.Limits{})
	if leftErr != nil || rightErr != nil {
		t.Fatalf("CanonicalizeSchema errors = %v, %v", leftErr, rightErr)
	}
	if string(leftCanonical) != string(rightCanonical) {
		t.Fatalf("schema registration order changed identity:\n%s\n%s", leftCanonical, rightCanonical)
	}
}

func TestCanonicalizeSchemaNormalizesPortableDeclarationArrays(t *testing.T) {
	t.Parallel()

	left := []byte(`{"revision":"schema-r1","types":[{"id":"User","fields":[{"id":"User.z","traits":[{"id":"z.trait"},{"id":"a.trait"}]},{"id":"User.a"}],"enumMembers":[{"id":"User.Z"},{"id":"User.A"}],"variantMembers":[{"id":"User.Z"},{"id":"User.A"}],"retired":[{"id":"User.old-z"},{"id":"User.old-a"}],"entity":{"keys":["z","a"]},"capabilities":["z.cap","a.cap"],"traits":[{"id":"z.trait"},{"id":"a.trait"}]}],"operations":[{"id":"query.z"},{"id":"query.a","capabilities":["z.cap","a.cap"],"traits":[{"id":"z.trait"},{"id":"a.trait"}]}],"members":[{"id":"User.z"},{"id":"User.a"}],"retired":[{"id":"old.z"},{"id":"old.a"}],"references":[{"uri":"urn:z","revision":"r2"},{"uri":"urn:a","revision":"r1"}],"traits":[{"id":"z.trait"},{"id":"a.trait"}]}`)
	right := []byte(`{"traits":[{"id":"a.trait"},{"id":"z.trait"}],"references":[{"revision":"r1","uri":"urn:a"},{"revision":"r2","uri":"urn:z"}],"retired":[{"id":"old.a"},{"id":"old.z"}],"members":[{"id":"User.a"},{"id":"User.z"}],"operations":[{"traits":[{"id":"a.trait"},{"id":"z.trait"}],"capabilities":["a.cap","z.cap"],"id":"query.a"},{"id":"query.z"}],"types":[{"traits":[{"id":"a.trait"},{"id":"z.trait"}],"capabilities":["a.cap","z.cap"],"entity":{"keys":["a","z"]},"retired":[{"id":"User.old-a"},{"id":"User.old-z"}],"variantMembers":[{"id":"User.A"},{"id":"User.Z"}],"enumMembers":[{"id":"User.A"},{"id":"User.Z"}],"fields":[{"id":"User.a"},{"traits":[{"id":"a.trait"},{"id":"z.trait"}],"id":"User.z"}],"id":"User"}],"revision":"schema-r1"}`)
	leftCanonical, leftErr := protocol.CanonicalizeSchema(left, protocol.Limits{})
	rightCanonical, rightErr := protocol.CanonicalizeSchema(right, protocol.Limits{})
	if leftErr != nil || rightErr != nil {
		t.Fatalf("CanonicalizeSchema errors = %v, %v", leftErr, rightErr)
	}
	if string(leftCanonical) != string(rightCanonical) {
		t.Fatalf("portable declaration order changed identity:\n%s\n%s", leftCanonical, rightCanonical)
	}
}

func TestCanonicalizeSchemaRejectsInvalidAndDuplicateIdentifiers(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`{"revision":"schema-7","types":[{"id":"bad id","kind":"object"}]}`,
		`{"revision":"schema-7","types":[{"id":"User","kind":"object"},{"id":"User","kind":"object"}]}`,
		`{"revision":"schema-7","types":[{"id":"Role","kind":"enum","enumValues":["ADMIN","ADMIN"]}]}`,
		`{"revision":"schema-7","types":[{"id":"Result","kind":"union","variants":["bad id"]}]}`,
		`{"revision":"schema-7","types":[{"id":"Slug","kind":"scalar","scalar":{"acceptedWireShapes":["string","string"]}}]}`,
	} {
		if _, err := protocol.CanonicalizeSchema([]byte(input), protocol.Limits{}); err == nil {
			t.Fatalf("CanonicalizeSchema(%s) succeeded, want error", input)
		}
	}
}

func TestCanonicalizeHashPayloadNormalizesCapabilitySets(t *testing.T) {
	t.Parallel()

	left, err := protocol.CanonicalizeHashPayload(protocol.ApprovalHash, []byte(`{"capabilities":["vendor.audit-1","core.language-1"]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeHashPayload: %v", err)
	}
	right, err := protocol.CanonicalizeHashPayload(protocol.ApprovalHash, []byte(`{"capabilities":["core.language-1","vendor.audit-1"]}`), protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeHashPayload: %v", err)
	}
	if string(left) != string(right) {
		t.Fatalf("capability order changed approval identity: %s != %s", left, right)
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

func TestFederationHashHasDedicatedDomain(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"profile":"core.federation-1"}`)
	federation, err := protocol.SemanticHash(protocol.FederationHash, payload)
	if err != nil {
		t.Fatalf("federation SemanticHash: %v", err)
	}
	signed, err := protocol.SemanticHash(protocol.SignedMessageHash, payload)
	if err != nil {
		t.Fatalf("signed-message SemanticHash: %v", err)
	}
	if federation == signed || federation.Hex == signed.Hex {
		t.Fatal("federation and signed-message hashes share a domain")
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
