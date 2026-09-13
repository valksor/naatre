package protocol_test

import (
	"testing"

	"github.com/valksor/naatre/protocol"
)

func TestValidSemanticVersionImplementsSemVerTwo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		valid   bool
	}{
		{version: "0.0.0", valid: true},
		{version: "1.0.0-alpha--beta", valid: true},
		{version: "1.0.0-alpha-", valid: true},
		{version: "1.0.0--", valid: true},
		{version: "1.0.0-x.7.z.92", valid: true},
		{version: "1.0.0+21AF26D3----117B344092BD", valid: true},
		{version: "1.0.0-alpha+build.001", valid: true},
		{version: "1", valid: false},
		{version: "v1.0.0", valid: false},
		{version: "01.0.0", valid: false},
		{version: "1.0.0-01", valid: false},
		{version: "1.0.0-alpha..beta", valid: false},
		{version: "1.0.0+build..meta", valid: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.version, func(t *testing.T) {
			t.Parallel()
			if got := protocol.ValidSemanticVersion(test.version); got != test.valid {
				t.Fatalf("ValidSemanticVersion(%q) = %t, want %t", test.version, got, test.valid)
			}
		})
	}
}

func TestValidExtensionID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id    string
		valid bool
	}{
		{id: "com.example.audit", valid: true},
		{id: "io.x-y.z9", valid: true},
		{id: "com.example-", valid: false},
		{id: "com.-example", valid: false},
		{id: "com.example..audit", valid: false},
		{id: "3rd.example.audit", valid: false},
		{id: "core.example", valid: false},
		{id: "naatre.example", valid: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.id, func(t *testing.T) {
			t.Parallel()
			if got := protocol.ValidExtensionID(test.id); got != test.valid {
				t.Fatalf("ValidExtensionID(%q) = %t, want %t", test.id, got, test.valid)
			}
		})
	}
}
