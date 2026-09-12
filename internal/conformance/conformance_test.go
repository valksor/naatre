package conformance_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

type canonicalFixture struct {
	Profile               string                       `json:"profile"`
	CanonicalVersion      string                       `json:"canonicalVersion"`
	HashAlgorithm         string                       `json:"hashAlgorithm"`
	ScalarProfile         string                       `json:"scalarProfile"`
	RequiredScalarVectors []string                     `json:"requiredScalarVectors"`
	JSONVectors           []canonicalJSONVector        `json:"jsonVectors"`
	HashVectors           []canonicalHashVector        `json:"hashVectors"`
	EquivalenceVectors    []canonicalEquivalenceVector `json:"equivalenceVectors"`
	IsolationVectors      []canonicalIsolationVector   `json:"isolationVectors"`
	Malformed             []canonicalMalformedVector   `json:"malformed"`
}

type canonicalJSONVector struct {
	Name       string `json:"name"`
	Input      string `json:"input"`
	ParsedType string `json:"parsedType"`
	Canonical  string `json:"canonical"`
}

type canonicalHashVector struct {
	canonicalJSONVector
	Purpose     protocol.HashPurpose `json:"purpose"`
	DomainInput string               `json:"domainInput"`
	Digest      string               `json:"digest"`
}

type canonicalEquivalenceVector struct {
	Name    string               `json:"name"`
	Purpose protocol.HashPurpose `json:"purpose"`
	Left    string               `json:"left"`
	Right   string               `json:"right"`
	Equal   bool                 `json:"equal"`
}

type canonicalIsolationVector struct {
	Name          string               `json:"name"`
	DocumentLeft  string               `json:"documentLeft"`
	DocumentRight string               `json:"documentRight"`
	Purpose       protocol.HashPurpose `json:"purpose"`
	PayloadLeft   string               `json:"payloadLeft"`
	PayloadRight  string               `json:"payloadRight"`
	DocumentEqual bool                 `json:"documentEqual"`
	IdentityEqual bool                 `json:"identityEqual"`
}

type canonicalMalformedVector struct {
	Name        string `json:"name"`
	Input       string `json:"input"`
	InputBase64 string `json:"inputBase64"`
	Code        string `json:"code"`
}

func TestPortableScalarVectors(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile string `json:"profile"`
		Vectors []struct {
			Name      string            `json:"name"`
			Kind      schema.ScalarKind `json:"kind"`
			Input     string            `json:"input"`
			Canonical string            `json:"canonical"`
			Valid     *bool             `json:"valid"`
		} `json:"vectors"`
	}
	readFixture(t, "scalars.json", &fixture)
	if fixture.Profile != "core.scalar.c14n-1" || len(fixture.Vectors) == 0 {
		t.Fatal("scalar fixture requires profile and vectors")
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			if vector.Name == "" || vector.Kind == "" || vector.Input == "" || vector.Valid == nil || (*vector.Valid && vector.Canonical == "") {
				t.Fatal("scalar vector is incomplete")
			}
			value, err := schema.ParseScalar(vector.Kind, json.RawMessage(vector.Input))
			if !*vector.Valid {
				if err == nil {
					t.Fatal("accepted invalid scalar vector")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseScalar: %v", err)
			}
			canonical, err := value.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			if string(canonical) != vector.Canonical {
				t.Fatalf("canonical = %s, want %s", canonical, vector.Canonical)
			}
		})
	}
}

func TestPortableCanonicalVectors(t *testing.T) {
	t.Parallel()
	assertPortableCanonicalContract(t)
}

func assertPortableCanonicalContract(t *testing.T) {
	t.Helper()
	fixture := loadCanonicalFixture(t)
	assertCanonicalVectorGroups(t, fixture)
	assertRequiredScalarVectors(t, fixture)
}

func loadCanonicalFixture(t *testing.T) canonicalFixture {
	t.Helper()
	var fixture canonicalFixture
	readFixture(t, "canonical.json", &fixture)
	assertCanonicalFixtureHeader(t, fixture)
	return fixture
}

func assertCanonicalVectorGroups(t *testing.T, fixture canonicalFixture) {
	t.Helper()
	seenNames := make(map[string]bool)
	assertCanonicalJSONVectors(t, fixture.JSONVectors, seenNames)
	assertCanonicalHashVectors(t, fixture, seenNames)
	assertCanonicalEquivalenceVectors(t, fixture.EquivalenceVectors, seenNames)
	assertCanonicalIsolationVectors(t, fixture.IsolationVectors, seenNames)
	assertCanonicalMalformedVectors(t, fixture.Malformed, seenNames)
}

func assertCanonicalFixtureHeader(t *testing.T, fixture canonicalFixture) {
	t.Helper()
	if fixture.Profile != "core.interop.c14n-1" || fixture.CanonicalVersion != "c14n-1" || fixture.HashAlgorithm != "sha-256" || fixture.ScalarProfile != "core.scalar.c14n-1" {
		t.Fatal("canonical fixture requires exact profile identifiers")
	}
	if len(fixture.JSONVectors) == 0 || len(fixture.HashVectors) == 0 || len(fixture.EquivalenceVectors) == 0 || len(fixture.IsolationVectors) == 0 || len(fixture.Malformed) == 0 {
		t.Fatal("canonical fixture requires every vector class")
	}
}

func assertCanonicalJSONVectors(t *testing.T, vectors []canonicalJSONVector, seenNames map[string]bool) {
	t.Helper()
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			assertCanonicalJSONVector(t, vector)
		})
		assertUniqueCanonicalName(t, seenNames, vector.Name)
	}
}

func assertCanonicalHashVectors(t *testing.T, fixture canonicalFixture, seenNames map[string]bool) {
	t.Helper()
	domainPurposes := make(map[protocol.HashPurpose]bool)
	domainDigests := make(map[string]bool)
	for _, vector := range fixture.HashVectors {
		t.Run(vector.Name, func(t *testing.T) { assertCanonicalHashVector(t, fixture, vector) })
		assertUniqueCanonicalName(t, seenNames, vector.Name)
		if strings.HasPrefix(vector.Name, "same-payload-") {
			domainPurposes[vector.Purpose] = true
			if domainDigests[vector.Digest] {
				t.Fatalf("same payload reused digest %s", vector.Digest)
			}
			domainDigests[vector.Digest] = true
		}
	}
	if len(domainPurposes) != 6 || len(domainDigests) != 6 {
		t.Fatalf("domain separation coverage = %d purposes and %d digests, want six", len(domainPurposes), len(domainDigests))
	}
}

func assertCanonicalHashVector(t *testing.T, fixture canonicalFixture, vector canonicalHashVector) {
	t.Helper()
	if vector.Name == "" || vector.Input == "" || vector.ParsedType == "" || vector.Canonical == "" {
		t.Fatal("canonical hash vector is incomplete")
	}
	canonicalize := protocol.CanonicalizeHashPayload
	if strings.HasPrefix(vector.Name, "same-payload-") {
		canonicalize = func(_ protocol.HashPurpose, input []byte, limits protocol.Limits) ([]byte, error) {
			return protocol.CanonicalizeJSON(input, limits)
		}
	}
	canonical, err := canonicalize(vector.Purpose, []byte(vector.Input), protocol.Limits{})
	if err != nil || string(canonical) != vector.Canonical {
		t.Fatalf("CanonicalizeHashPayload = %s, %v; want %s", canonical, err, vector.Canonical)
	}
	if actual := canonicalJSONType(t, canonical); actual != vector.ParsedType {
		t.Fatalf("parsed type = %s, want %s", actual, vector.ParsedType)
	}
	expectedDomain := "naatre:" + string(vector.Purpose) + ":" + fixture.CanonicalVersion + "\n" + string(canonical)
	if vector.DomainInput != expectedDomain {
		t.Fatalf("domain input = %q, want %q", vector.DomainInput, expectedDomain)
	}
	digest, err := protocol.SemanticHash(vector.Purpose, canonical)
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	if digest.Algorithm != fixture.HashAlgorithm || digest.CanonicalVersion != fixture.CanonicalVersion || digest.Hex != vector.Digest {
		t.Fatalf("digest = %#v, want %s/%s/%s", digest, fixture.HashAlgorithm, fixture.CanonicalVersion, vector.Digest)
	}
}

func assertCanonicalEquivalenceVectors(t *testing.T, vectors []canonicalEquivalenceVector, seenNames map[string]bool) {
	t.Helper()
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			left := canonicalDigest(t, vector.Purpose, vector.Left)
			right := canonicalDigest(t, vector.Purpose, vector.Right)
			if equal := left == right; equal != vector.Equal {
				t.Fatalf("digest equality = %t, want %t", equal, vector.Equal)
			}
		})
		assertUniqueCanonicalName(t, seenNames, vector.Name)
	}
}

func assertCanonicalIsolationVectors(t *testing.T, vectors []canonicalIsolationVector, seenNames map[string]bool) {
	t.Helper()
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			assertPurposeIsolationPayload(t, vector.Purpose, vector.PayloadLeft)
			assertPurposeIsolationPayload(t, vector.Purpose, vector.PayloadRight)
			documentEqual := canonicalDigest(t, protocol.DocumentHash, vector.DocumentLeft) == canonicalDigest(t, protocol.DocumentHash, vector.DocumentRight)
			identityEqual := canonicalDigest(t, vector.Purpose, vector.PayloadLeft) == canonicalDigest(t, vector.Purpose, vector.PayloadRight)
			if documentEqual != vector.DocumentEqual || identityEqual != vector.IdentityEqual {
				grid := fmt.Sprintf("document:%t identity:%t", documentEqual, identityEqual)
				want := fmt.Sprintf("document:%t identity:%t", vector.DocumentEqual, vector.IdentityEqual)
				t.Fatalf("equality = %s, want %s", grid, want)
			}
		})
		assertUniqueCanonicalName(t, seenNames, vector.Name)
	}
}

func assertCanonicalMalformedVectors(t *testing.T, vectors []canonicalMalformedVector, seenNames map[string]bool) {
	t.Helper()
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Name == "" || vector.Code == "" || (vector.Input == "") == (vector.InputBase64 == "") {
				t.Fatal("malformed canonical vector is incomplete")
			}
			input := []byte(vector.Input)
			if vector.InputBase64 != "" {
				var err error
				input, err = base64.StdEncoding.DecodeString(vector.InputBase64)
				if err != nil {
					t.Fatalf("decode malformed bytes: %v", err)
				}
			}
			_, err := protocol.CanonicalizeJSON(input, protocol.Limits{})
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != vector.Code {
				t.Fatalf("error = %v, want diagnostic %s", err, vector.Code)
			}
		})
		assertUniqueCanonicalName(t, seenNames, vector.Name)
	}
}

func assertCanonicalJSONVector(t *testing.T, vector canonicalJSONVector) []byte {
	t.Helper()
	if vector.Name == "" || vector.Input == "" || vector.ParsedType == "" || vector.Canonical == "" {
		t.Fatal("canonical vector is incomplete")
	}
	canonical, err := protocol.CanonicalizeJSON([]byte(vector.Input), protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeJSON: %v", err)
	}
	if string(canonical) != vector.Canonical {
		t.Fatalf("canonical = %s, want %s", canonical, vector.Canonical)
	}
	if actual := canonicalJSONType(t, canonical); actual != vector.ParsedType {
		t.Fatalf("parsed type = %s, want %s", actual, vector.ParsedType)
	}
	return canonical
}

func canonicalJSONType(t *testing.T, canonical []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(canonical, &value); err != nil {
		t.Fatalf("decode canonical JSON: %v", err)
	}
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		t.Fatalf("unknown canonical JSON type %T", value)
		return ""
	}
}

func canonicalDigest(t *testing.T, purpose protocol.HashPurpose, input string) string {
	t.Helper()
	canonical, err := protocol.CanonicalizeHashPayload(purpose, []byte(input), protocol.Limits{})
	if err != nil {
		t.Fatalf("CanonicalizeHashPayload: %v", err)
	}
	digest, err := protocol.SemanticHash(purpose, canonical)
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	return digest.Hex
}

func assertPurposeIsolationPayload(t *testing.T, purpose protocol.HashPurpose, input string) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("decode isolation payload: %v", err)
	}
	required := map[protocol.HashPurpose][]string{
		protocol.ResultCacheHash: {"document", "schema", "variables", "capabilities"},
		protocol.ApprovalHash:    {"document", "schema", "policyRevision", "capabilities", "approvalMetadata"},
	}[purpose]
	if len(required) == 0 || len(payload) != len(required) {
		t.Fatalf("%s isolation payload has %d members, want exactly %#v", purpose, len(payload), required)
	}
	for _, name := range required {
		if _, ok := payload[name]; !ok {
			t.Fatalf("%s isolation payload misses %q", purpose, name)
		}
	}
	for _, name := range []string{"document", "schema"} {
		var members map[string]json.RawMessage
		var digest protocol.Digest
		if err := json.Unmarshal(payload[name], &members); err != nil || len(members) != 3 || members["algorithm"] == nil || members["canonicalVersion"] == nil || members["digest"] == nil {
			t.Fatalf("%s isolation payload has non-exact %s digest record", purpose, name)
		}
		if err := json.Unmarshal(payload[name], &digest); err != nil || digest.Algorithm != "sha-256" || digest.CanonicalVersion != "c14n-1" || !isLowerHexDigest(digest.Hex) {
			t.Fatalf("%s isolation payload has invalid %s digest record", purpose, name)
		}
	}
}

func isLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, current := range value {
		if !strings.ContainsRune("0123456789abcdef", current) {
			return false
		}
	}
	return true
}

func assertUniqueCanonicalName(t *testing.T, seen map[string]bool, name string) {
	t.Helper()
	if name == "" || seen[name] {
		t.Fatalf("canonical fixture has empty or duplicate vector name %q", name)
	}
	seen[name] = true
}

func assertRequiredScalarVectors(t *testing.T, fixture canonicalFixture) {
	t.Helper()
	var scalars struct {
		Profile string `json:"profile"`
		Vectors []struct {
			Name      string            `json:"name"`
			Kind      schema.ScalarKind `json:"kind"`
			Input     string            `json:"input"`
			Canonical string            `json:"canonical"`
			Valid     bool              `json:"valid"`
		} `json:"vectors"`
	}
	readFixture(t, "scalars.json", &scalars)
	if scalars.Profile != fixture.ScalarProfile {
		t.Fatalf("scalar profile = %q, want %q", scalars.Profile, fixture.ScalarProfile)
	}
	available := make(map[string]bool, len(scalars.Vectors))
	for _, vector := range scalars.Vectors {
		available[vector.Name] = true
	}
	for _, required := range fixture.RequiredScalarVectors {
		if !available[required] {
			t.Errorf("canonical profile requires missing scalar vector %q", required)
		}
	}
}

func readFixture(t *testing.T, name string, destination any) {
	t.Helper()
	path := filepath.Join("..", "..", "conformance", "v1", name)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := protocol.ValidateJSON(content, protocol.Limits{}); err != nil {
		t.Fatalf("validate strict JSON in %s: %v", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("decode %s trailing data: %v", path, err)
	}
}

func runCases[T any](t *testing.T, prefix string, cases []T, name func(T) string, run func(*testing.T, T)) {
	t.Helper()
	for _, current := range cases {
		current := current
		t.Run(prefix+name(current), func(t *testing.T) {
			t.Parallel()
			run(t, current)
		})
	}
}
