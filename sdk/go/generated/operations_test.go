package generated

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/valksor/naatre/client"
	"github.com/valksor/naatre/protocol"
)

func TestGeneratedOperationMatchesSharedRequestAndManifest(t *testing.T) {
	t.Parallel()
	operation, err := NewGetAccount()
	if err != nil {
		t.Fatalf("NewGetAccount: %v", err)
	}
	variables := map[string]json.RawMessage{
		"id": []byte(`"acct-1"`), "nickname": []byte(`null`), "tags": []byte(`[]`), "filter": []byte(`{}`),
	}
	for name, value := range variables {
		operation, err = operation.WithVariable(name, value)
		if err != nil {
			t.Fatalf("WithVariable %s: %v", name, err)
		}
	}
	actual, err := operation.Request().CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := sharedRequest(t)
	if !bytes.Equal(actual, want) {
		t.Fatalf("generated request mismatch:\n%s\n%s", actual, want)
	}
	if operation.Request().OperationKind() != protocol.Query {
		t.Fatalf("generated operation kind = %q", operation.Request().OperationKind())
	}

	manifest, err := Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	encoded, err := manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("manifest CanonicalJSON: %v", err)
	}
	checked, err := os.ReadFile("operations.json")
	if err != nil {
		t.Fatalf("read checked manifest: %v", err)
	}
	if !bytes.Equal(encoded, checked) {
		t.Fatalf("generated manifest mismatch:\n%s\n%s", encoded, checked)
	}
}

func TestGeneratedResultPreservesSelectedStates(t *testing.T) {
	t.Parallel()
	result, err := DecodeGetAccountResult([]byte(`{"profile":{"display":"Ada","nickname":null}}`))
	if err != nil {
		t.Fatalf("DecodeGetAccountResult: %v", err)
	}
	profile, present := result.Profile.Value()
	if !present || result.Profile.Presence() != client.Present {
		t.Fatalf("profile = %#v", result.Profile)
	}
	display, present := profile.Display.Value()
	if !present || display != "Ada" || profile.Nickname.Presence() != client.Null {
		t.Fatalf("nested result = %#v", profile)
	}
	if result.Later.Presence() != client.Pending {
		t.Fatalf("pending result = %#v", result.Later)
	}

	missing, err := DecodeGetAccountResult([]byte(`{}`))
	if err != nil {
		t.Fatalf("DecodeGetAccountResult missing: %v", err)
	}
	if missing.Profile.Presence() != client.Missing || missing.Later.Presence() != client.Pending {
		t.Fatalf("missing result = %#v", missing)
	}
}

func TestGeneratedNestedDecoderDirectly(t *testing.T) {
	t.Parallel()
	profile, err := decodeGetAccountResultProfile([]byte(`{"display":"Ada"}`))
	if err != nil {
		t.Fatalf("decodeGetAccountResultProfile: %v", err)
	}
	display, present := profile.Display.Value()
	if !present || display != "Ada" || profile.Nickname.Presence() != client.Missing {
		t.Fatalf("profile = %#v", profile)
	}
}

func sharedRequest(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile("../../../conformance/v1/generator-output.json")
	if err != nil {
		t.Fatalf("read generator output: %v", err)
	}
	var output struct {
		Operations []struct {
			Request json.RawMessage `json:"request"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(content, &output); err != nil || len(output.Operations) != 1 {
		t.Fatalf("decode generator output: %v", err)
	}
	request, err := protocol.CanonicalizeJSON(output.Operations[0].Request, protocol.Limits{})
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	return request
}
