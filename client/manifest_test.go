package client

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestManifestRoundTripsPersistedOperationsAndRejectsUnknownInput(t *testing.T) {
	t.Parallel()
	document := generatorFixtureDocument(t)
	digest, err := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	manifest, err := NewManifest(ManifestOperation{
		Name: "GetAccount", Kind: protocol.Query,
		Persisted: protocol.PersistedReference{Algorithm: digest.Algorithm, CanonicalVersion: digest.CanonicalVersion, Digest: digest.Hex},
	})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	encoded, err := manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"algorithm":"sha-256"`)) || bytes.Contains(encoded, []byte(`"Algorithm"`)) {
		t.Fatalf("manifest persisted reference is not canonical wire JSON: %s", encoded)
	}
	loaded, err := LoadManifest(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	request, err := loaded.Request("GetAccount")
	if err != nil || request.OperationKind() != protocol.Query {
		t.Fatalf("Request = %#v, %v", request, err)
	}
	if _, err := loaded.Request("Missing"); err == nil {
		t.Fatal("Request accepted unknown operation")
	}
	unknown := bytes.Replace(encoded, []byte(`"version":"1"`), []byte(`"unknown":true,"version":"1"`), 1)
	if _, err := LoadManifest(bytes.NewReader(unknown), int64(len(unknown))); err == nil {
		t.Fatal("LoadManifest accepted unknown field")
	}
	if _, err := LoadManifest(strings.NewReader("{}x"), 2); err == nil {
		t.Fatal("LoadManifest accepted oversized input")
	} else {
		var clientFailure *Error
		if !errors.As(err, &clientFailure) || clientFailure.Code != "MANIFEST_LIMIT_EXCEEDED" {
			t.Fatalf("oversized error = %v", err)
		}
	}
}
