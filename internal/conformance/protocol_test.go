package conformance_test

import (
	"errors"
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestPortableProtocolVectors(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Profile   string           `json:"profile"`
		Requests  []protocolVector `json:"requests"`
		Responses []protocolVector `json:"responses"`
	}
	readFixture(t, "protocol.json", &fixture)
	if fixture.Profile != "core.protocol-1" || len(fixture.Requests) == 0 || len(fixture.Responses) == 0 {
		t.Fatal("protocol fixture requires its exact profile plus request and response vectors")
	}
	for _, vector := range fixture.Requests {
		vector := vector
		t.Run("request/"+vector.Name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeRequest([]byte(vector.Input), protocol.DecodeOptions{})
			assertProtocolVector(t, vector, err)
		})
	}
	for _, vector := range fixture.Responses {
		vector := vector
		t.Run("response/"+vector.Name, func(t *testing.T) {
			t.Parallel()
			_, err := protocol.DecodeResponse([]byte(vector.Input), protocol.DecodeOptions{})
			assertProtocolVector(t, vector, err)
		})
	}
}

type protocolVector struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Valid *bool  `json:"valid"`
	Code  string `json:"code"`
	Phase string `json:"phase"`
}

func assertProtocolVector(t *testing.T, vector protocolVector, err error) {
	t.Helper()
	if vector.Name == "" || vector.Input == "" || vector.Valid == nil ||
		(*vector.Valid && (vector.Code != "" || vector.Phase != "")) ||
		(!*vector.Valid && (vector.Code == "" || vector.Phase == "")) {
		t.Fatal("protocol vector metadata is inconsistent")
	}
	if *vector.Valid {
		if err != nil {
			t.Fatalf("decode valid vector: %v", err)
		}
		return
	}
	var diagnostic *protocol.Diagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Code != vector.Code || diagnostic.Phase != vector.Phase {
		t.Fatalf("decode error = %v, want %s in %s phase", err, vector.Code, vector.Phase)
	}
}
