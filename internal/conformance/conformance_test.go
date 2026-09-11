package conformance_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

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
	var fixture struct {
		Profile string `json:"profile"`
		Vectors []struct {
			Name             string               `json:"name"`
			Input            string               `json:"input"`
			Canonical        string               `json:"canonical"`
			Purpose          protocol.HashPurpose `json:"purpose"`
			Algorithm        string               `json:"algorithm"`
			CanonicalVersion string               `json:"canonicalVersion"`
			Digest           string               `json:"digest"`
		} `json:"vectors"`
		Malformed []struct {
			Name  string `json:"name"`
			Input string `json:"input"`
			Code  string `json:"code"`
		} `json:"malformed"`
	}
	readFixture(t, "canonical.json", &fixture)
	if fixture.Profile != "core.document.c14n-1" || len(fixture.Vectors) == 0 || len(fixture.Malformed) == 0 {
		t.Fatal("canonical fixture requires profile and vectors")
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Name == "" || vector.Input == "" || vector.Canonical == "" {
				t.Fatal("canonical vector is incomplete")
			}
			hasPurpose := vector.Purpose != ""
			hasDigestMetadata := vector.Algorithm != "" || vector.CanonicalVersion != "" || vector.Digest != ""
			if hasPurpose != hasDigestMetadata || (hasPurpose && (vector.Algorithm == "" || vector.CanonicalVersion == "" || vector.Digest == "")) {
				t.Fatal("canonical vector hash metadata must be complete or absent")
			}
			canonical, err := protocol.CanonicalizeJSON([]byte(vector.Input), protocol.Limits{})
			if err != nil {
				t.Fatalf("CanonicalizeJSON: %v", err)
			}
			if string(canonical) != vector.Canonical {
				t.Fatalf("canonical = %s, want %s", canonical, vector.Canonical)
			}
			if vector.Purpose == "" {
				return
			}
			digest, err := protocol.SemanticHash(vector.Purpose, canonical)
			if err != nil {
				t.Fatalf("SemanticHash: %v", err)
			}
			if digest.Algorithm != vector.Algorithm || digest.CanonicalVersion != vector.CanonicalVersion || digest.Hex != vector.Digest {
				t.Fatalf("digest = %#v", digest)
			}
		})
	}
	for _, vector := range fixture.Malformed {
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Name == "" || vector.Input == "" || vector.Code == "" {
				t.Fatal("malformed canonical vector is incomplete")
			}
			_, err := protocol.CanonicalizeJSON([]byte(vector.Input), protocol.Limits{})
			var diagnostic *protocol.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != vector.Code {
				t.Fatalf("error = %v, want diagnostic %s", err, vector.Code)
			}
		})
	}
}

func readFixture(t *testing.T, name string, destination any) {
	t.Helper()
	path := filepath.Join("..", "..", "conformance", "v1", name)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
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
