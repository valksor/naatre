package conformance_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
)

type persistedFixture struct {
	Profile          string                    `json:"profile"`
	Algorithm        string                    `json:"algorithm"`
	CanonicalVersion string                    `json:"canonicalVersion"`
	IdentityVectors  []persistedIdentityVector `json:"identityVectors"`
	ExcludedIdentity []string                  `json:"excludedIdentity"`
	ModeCases        []persistedModeCase       `json:"modeCases"`
	ApprovalCases    []persistedApprovalCase   `json:"approvalCases"`
	LifecycleCases   []persistedLifecycleCase  `json:"lifecycleCases"`
}

type persistedIdentityVector struct {
	Name          string          `json:"name"`
	Document      json.RawMessage `json:"document"`
	Digest        string          `json:"digest"`
	EqualTo       string          `json:"equalTo"`
	DifferentFrom string          `json:"differentFrom"`
}

type persistedModeCase struct {
	Name              string `json:"name"`
	InlineAccepted    bool   `json:"inlineAccepted"`
	RegistersOnLookup bool   `json:"registersOnLookup"`
	Authenticated     bool   `json:"authenticated"`
	Quota             bool   `json:"quota"`
	CostApproval      bool   `json:"costApproval"`
}

type persistedApprovalCase struct {
	Name                   string `json:"name"`
	Query                  bool   `json:"query"`
	GET                    bool   `json:"get"`
	PublicOutput           bool   `json:"publicOutput"`
	AuthorizationInvariant bool   `json:"authorizationInvariant"`
	Shared                 bool   `json:"shared"`
}

type persistedLifecycleCase struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

func TestPersistedOperationFixture(t *testing.T) {
	t.Parallel()
	var fixture persistedFixture
	readFixture(t, "persisted.json", &fixture)
	if fixture.Profile != "core.persisted-1" || fixture.Algorithm != "sha-256" || fixture.CanonicalVersion != "c14n-1" {
		t.Fatalf("persisted fixture header = %#v", fixture)
	}
	assertPersistedIdentity(t, fixture.IdentityVectors)
	if !slices.Equal(fixture.ExcludedIdentity, []string{"requestId", "correlationId", "runtimeVariables", "principal", "authToken"}) {
		t.Fatalf("excluded identity fields = %v", fixture.ExcludedIdentity)
	}
	assertPersistedModes(t, fixture.ModeCases)
	assertPersistedApprovals(t, fixture.ApprovalCases)
	assertPersistedLifecycle(t, fixture.LifecycleCases)
}

func assertPersistedIdentity(t *testing.T, vectors []persistedIdentityVector) {
	t.Helper()
	digests := make(map[string]string, len(vectors))
	for _, vector := range vectors {
		document, err := protocol.DecodeDocument(vector.Document, protocol.Limits{})
		if err != nil {
			t.Fatalf("%s DecodeDocument: %v", vector.Name, err)
		}
		digest, err := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
		if err != nil {
			t.Fatalf("%s SemanticHash: %v", vector.Name, err)
		}
		if digest.Hex != vector.Digest {
			t.Errorf("%s digest = %s, want %s", vector.Name, digest.Hex, vector.Digest)
		}
		digests[vector.Name] = digest.Hex
	}
	for _, vector := range vectors {
		if vector.EqualTo != "" && digests[vector.Name] != digests[vector.EqualTo] {
			t.Errorf("%s digest differs from %s", vector.Name, vector.EqualTo)
		}
		if vector.DifferentFrom != "" && digests[vector.Name] == digests[vector.DifferentFrom] {
			t.Errorf("%s digest equals %s", vector.Name, vector.DifferentFrom)
		}
	}
}

func assertPersistedModes(t *testing.T, cases []persistedModeCase) {
	t.Helper()
	if len(cases) != 3 || cases[0].Name != "lookup-only" || cases[1].Name != "register-at-deploy" || cases[2].Name != "automatic" {
		t.Fatalf("persisted mode cases = %#v", cases)
	}
	for _, test := range cases {
		if test.RegistersOnLookup {
			t.Fatalf("mode registers on lookup: %#v", test)
		}
		if test.Name == "automatic" && (!test.InlineAccepted || !test.Authenticated || !test.Quota || !test.CostApproval) {
			t.Fatalf("automatic mode is uncontrolled: %#v", test)
		}
	}
}

func assertPersistedApprovals(t *testing.T, cases []persistedApprovalCase) {
	t.Helper()
	want := []string{"query-private", "get-private", "shared-public-invariant", "shared-missing-public", "shared-auth-variant"}
	names := make([]string, 0, len(cases))
	for _, test := range cases {
		names = append(names, test.Name)
		if test.Shared && (!test.PublicOutput || !test.AuthorizationInvariant) {
			t.Fatalf("unsafe shared approval: %#v", test)
		}
		if (test.Query || test.GET) && !test.PublicOutput && test.Shared {
			t.Fatalf("query or GET promoted cache scope: %#v", test)
		}
	}
	if !slices.Equal(names, want) {
		t.Fatalf("persisted approval cases = %v, want %v", names, want)
	}
}

func assertPersistedLifecycle(t *testing.T, cases []persistedLifecycleCase) {
	t.Helper()
	want := []persistedLifecycleCase{
		{Name: "not-found", Code: "PERSISTED_NOT_FOUND"},
		{Name: "tampered", Code: "PERSISTED_DOCUMENT_MISMATCH"},
		{Name: "collision", Code: "PERSISTED_HASH_COLLISION"},
		{Name: "stale-schema", Code: "PERSISTED_SCHEMA_STALE"},
		{Name: "stale-policy", Code: "PERSISTED_POLICY_STALE"},
		{Name: "revoked", Code: "PERSISTED_REVOKED"},
		{Name: "expired", Code: "PERSISTED_EXPIRED"},
		{Name: "tenant-isolated", Code: "PERSISTED_NOT_FOUND"},
		{Name: "storage-failure", Code: "PERSISTED_STORAGE"},
	}
	if !slices.Equal(cases, want) {
		t.Fatalf("persisted lifecycle cases = %#v, want %#v", cases, want)
	}
}
