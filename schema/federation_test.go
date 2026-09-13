package schema_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestComposeFederationIsDeterministicAcrossManifestOrder(t *testing.T) {
	t.Parallel()
	users := federationManifest(t, "users", "users-api", federationDocument(t, "users-r1", "User", "String", "query.user"))
	orders := federationManifest(t, "orders", "orders-api", federationDocument(t, "orders-r1", "Order", "Int64", "query.order"))
	orders.EntityFetches = []schema.EntityFetch{{Type: "Order", OperationID: "query.order", Keys: []string{"id"}}}
	users.EntityFetches = []schema.EntityFetch{{
		Type: "User", OperationID: "query.user", Keys: []string{"id"},
		Requires: []schema.EntityFetchDependency{{ServiceID: "orders", Type: "Order"}},
	}}
	options := federationOptions(users, orders)

	left, err := schema.ComposeFederation([]schema.ServiceManifest{users, orders}, options)
	if err != nil {
		t.Fatalf("ComposeFederation: %v", err)
	}
	right, err := schema.ComposeFederation([]schema.ServiceManifest{orders, users}, options)
	if err != nil {
		t.Fatalf("ComposeFederation(reordered): %v", err)
	}
	leftJSON, err := left.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := right.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftJSON, rightJSON) {
		t.Fatalf("composition depends on manifest order:\n%s\n%s", leftJSON, rightJSON)
	}
	leftHash, err := left.Schema().Hash()
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := right.Schema().Hash()
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatalf("schema hash depends on manifest order: %#v != %#v", leftHash, rightHash)
	}
	leftCompositionHash, err := left.Hash()
	if err != nil {
		t.Fatal(err)
	}
	rightCompositionHash, err := right.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if leftCompositionHash != rightCompositionHash {
		t.Fatalf("composition hash depends on manifest order: %#v != %#v", leftCompositionHash, rightCompositionHash)
	}
	if owner, ok := left.OperationOwner("query.order"); !ok || owner != "orders" {
		t.Fatalf("query.order owner = %q/%v", owner, ok)
	}
	if got := left.Services(); !slices.Equal(got, []string{"orders", "users"}) {
		t.Fatalf("services = %v", got)
	}
	services := left.Services()
	services[0] = "mutated"
	if got := left.Services(); !slices.Equal(got, []string{"orders", "users"}) {
		t.Fatalf("services alias internal state: %v", got)
	}
	service, ok := left.Service("users")
	if !ok {
		t.Fatal("users service binding missing")
	}
	service.EndpointReference = "mutated"
	if again, _ := left.Service("users"); again.EndpointReference != "users-api" {
		t.Fatalf("service binding aliases internal state: %#v", again)
	}
	operation, ok := left.Operation("query.user")
	if !ok {
		t.Fatal("query.user operation missing")
	}
	operation.Cost = 999
	if again, _ := left.Operation("query.user"); again.Cost == 999 {
		t.Fatalf("operation descriptor aliases internal state: %#v", again)
	}
	routes := left.EntityRoutes()
	if len(routes) != 2 {
		t.Fatalf("entity routes = %#v", routes)
	}
	routes[0].Keys[0] = "mutated"
	routes[1].Requires = nil
	again := left.EntityRoutes()
	if again[0].Keys[0] != "id" || len(again[1].Requires) != 1 {
		t.Fatalf("entity routes alias internal state: %#v", again)
	}
}

func TestComposeFederationFailureSelectionIsDeterministic(t *testing.T) {
	t.Parallel()
	left := federationManifest(t, "left", "left-api", federationDocument(t, "left-r1", "Left", "String", "query.left"))
	right := federationManifest(t, "right", "right-api", federationDocument(t, "right-r1", "Right", "String", "query.right"))
	left.OwnedOperations, left.OwnedMembers = nil, nil
	right.OwnedOperations, right.OwnedMembers = nil, nil
	for index := 0; index < 20; index++ {
		manifests := []schema.ServiceManifest{left, right}
		if index%2 != 0 {
			slices.Reverse(manifests)
		}
		_, err := schema.ComposeFederation(manifests, federationOptions(left, right))
		var compositionErr *schema.CompositionError
		if !errors.As(err, &compositionErr) || compositionErr.Code != schema.CodeFederationInvalidManifest ||
			compositionErr.ServiceID != "" || compositionErr.Definition != "query.left" {
			t.Fatalf("iteration %d error = %#v (%v)", index, compositionErr, err)
		}
	}
}

func TestComposeFederationRejectsDuplicateServiceIDsAndUnknownClaims(t *testing.T) {
	t.Parallel()
	manifest := federationManifest(t, "users", "users-api", federationDocument(t, "users-r1", "User", "String", "query.user"))
	_, err := schema.ComposeFederation([]schema.ServiceManifest{manifest, manifest}, federationOptions(manifest))
	assertCompositionCode(t, err, schema.CodeFederationInvalidManifest)

	manifest.OwnedOperations = append(manifest.OwnedOperations, "query.missing")
	_, err = schema.ComposeFederation([]schema.ServiceManifest{manifest}, federationOptions(manifest))
	assertCompositionCode(t, err, schema.CodeFederationInvalidManifest)
}

func TestComposeFederationRejectsClaimsMissingFromOwningServiceSchema(t *testing.T) {
	t.Parallel()
	left := federationManifest(t, "left", "left-api", federationDocument(t, "left-r1", "Left", "String", "query.left"))
	right := federationManifest(t, "right", "right-api", federationDocument(t, "right-r1", "Right", "String", "query.right"))
	left.OwnedOperations, right.OwnedOperations = right.OwnedOperations, left.OwnedOperations
	left.OwnedMembers, right.OwnedMembers = right.OwnedMembers, left.OwnedMembers
	_, err := schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	assertCompositionCode(t, err, schema.CodeFederationInvalidManifest)

	left = federationManifest(t, "left", "left-api", federationDocument(t, "left-r1", "Left", "String", "query.left"))
	right = federationManifest(t, "right", "right-api", federationDocument(t, "right-r1", "Right", "String", "query.right"))
	left.EntityFetches = []schema.EntityFetch{{Type: "Right", OperationID: "query.left", Keys: []string{"id"}}}
	_, err = schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	assertCompositionCode(t, err, schema.CodeFederationInvalidManifest)
}

func TestComposeFederationRejectsConflictingDocumentMetadata(t *testing.T) {
	t.Parallel()
	for _, metadata := range []string{"retired", "trait"} {
		metadata := metadata
		t.Run(metadata, func(t *testing.T) {
			leftDocument := federationDocumentWithMetadata(t, "left-r1", "Left", "query.left", metadata, "left")
			rightDocument := federationDocumentWithMetadata(t, "right-r1", "Right", "query.right", metadata, "right")
			left := federationManifest(t, "left", "left-api", leftDocument)
			right := federationManifest(t, "right", "right-api", rightDocument)
			_, err := schema.ComposeFederation([]schema.ServiceManifest{right, left}, federationOptions(left, right))
			assertCompositionCode(t, err, schema.CodeFederationTypeConflict)
		})
	}
}

func TestComposeFederationRejectsDirectiveIncompatibility(t *testing.T) {
	t.Parallel()
	left := federationManifest(t, "left", "left-api", federationDocumentWithDirective(t, "left-r1", "Left", "query.left", 1))
	right := federationManifest(t, "right", "right-api", federationDocumentWithDirective(t, "right-r1", "Right", "query.right", 2))
	_, err := schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	assertCompositionCode(t, err, schema.CodeFederationTypeConflict)
}

func TestComposeFederationRejectsConflictingOwnership(t *testing.T) {
	t.Parallel()
	document := federationDocument(t, "shared-r1", "User", "String", "query.user")
	left := federationManifest(t, "left", "left-api", document)
	right := federationManifest(t, "right", "right-api", document)
	_, err := schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	assertCompositionCode(t, err, schema.CodeFederationOwnershipConflict)
}

func TestComposeFederationRejectsIncompatibleTypes(t *testing.T) {
	t.Parallel()
	left := federationManifest(t, "left", "left-api", federationDocument(t, "left-r1", "User", "String", "query.user"))
	right := federationManifest(t, "right", "right-api", federationDocument(t, "right-r1", "User", "Int64", "query.otherUser"))
	right.OwnedOperations = []string{"query.otherUser"}
	right.OwnedMembers = []string{"User.value.resolver"}
	_, err := schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	assertCompositionCode(t, err, schema.CodeFederationTypeConflict)
}

func TestComposeFederationRejectsEntityFetchCycles(t *testing.T) {
	t.Parallel()
	users := federationManifest(t, "users", "users-api", federationDocument(t, "users-r1", "User", "String", "query.user"))
	orders := federationManifest(t, "orders", "orders-api", federationDocument(t, "orders-r1", "Order", "Int64", "query.order"))
	users.EntityFetches = []schema.EntityFetch{{
		Type: "User", OperationID: "query.user", Keys: []string{"id"},
		Requires: []schema.EntityFetchDependency{{ServiceID: "orders", Type: "Order"}},
	}}
	orders.EntityFetches = []schema.EntityFetch{{
		Type: "Order", OperationID: "query.order", Keys: []string{"id"},
		Requires: []schema.EntityFetchDependency{{ServiceID: "users", Type: "User"}},
	}}
	_, err := schema.ComposeFederation([]schema.ServiceManifest{users, orders}, federationOptions(users, orders))
	assertCompositionCode(t, err, schema.CodeFederationEntityCycle)
}

func TestComposeFederationRejectsUnknownEntityKeysAndDependencies(t *testing.T) {
	t.Parallel()
	users := federationManifest(t, "users", "users-api", federationDocument(t, "users-r1", "User", "String", "query.user"))
	users.EntityFetches = []schema.EntityFetch{{Type: "User", OperationID: "query.user", Keys: []string{"secret"}}}
	_, err := schema.ComposeFederation([]schema.ServiceManifest{users}, federationOptions(users))
	assertCompositionCode(t, err, schema.CodeFederationInvalidManifest)

	users.EntityFetches = []schema.EntityFetch{{
		Type: "User", OperationID: "query.user", Keys: []string{"id"},
		Requires: []schema.EntityFetchDependency{{ServiceID: "unlisted", Type: "User"}},
	}}
	_, err = schema.ComposeFederation([]schema.ServiceManifest{users}, federationOptions(users))
	assertCompositionCode(t, err, schema.CodeFederationInvalidManifest)
}

func TestComposeFederationRejectsUntrustedEndpointAndSchemaDrift(t *testing.T) {
	t.Parallel()
	manifest := federationManifest(t, "users", "https://attacker.invalid", federationDocument(t, "users-r1", "User", "String", "query.user"))
	options := schema.FederationOptions{Revision: "federation-r1", Services: map[string]schema.ServiceTrust{
		"users": {
			Audience: "naatre:users", EndpointReference: "users-api", FederationProfile: "core.federation-1",
			SchemaRevision: manifest.SchemaRevision, SchemaDigest: manifest.SchemaDigest,
		},
	}}
	_, err := schema.ComposeFederation([]schema.ServiceManifest{manifest}, options)
	assertCompositionCode(t, err, schema.CodeFederationUntrustedService)
	options.Services["users"] = schema.ServiceTrust{
		Audience: "naatre:users", EndpointReference: "https://attacker.invalid", FederationProfile: "core.federation-1",
		SchemaRevision: manifest.SchemaRevision, SchemaDigest: manifest.SchemaDigest,
	}
	_, err = schema.ComposeFederation([]schema.ServiceManifest{manifest}, options)
	assertCompositionCode(t, err, schema.CodeFederationUntrustedService)

	manifest.EndpointReference = "users-api"
	options.Services["users"] = schema.ServiceTrust{
		Audience: "naatre:users", EndpointReference: "users-api", FederationProfile: "core.federation-1",
		SchemaRevision: manifest.SchemaRevision, SchemaDigest: manifest.SchemaDigest,
	}
	manifest.SchemaDigest = "sha256:" + strings.Repeat("0", 64)
	_, err = schema.ComposeFederation([]schema.ServiceManifest{manifest}, options)
	assertCompositionCode(t, err, schema.CodeFederationSchemaMismatch)
}

func TestComposeFederationRequiresOperatorPinnedProvenanceAndProfile(t *testing.T) {
	t.Parallel()
	manifest := federationManifest(t, "users", "users-api", federationDocument(t, "users-r1", "User", "String", "query.user"))
	options := federationOptions(manifest)

	other := federationDocument(t, "users-r2", "User", "String", "query.user")
	otherDigest, err := other.Hash()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Schema = other
	manifest.SchemaRevision = other.Revision()
	manifest.SchemaDigest = "sha256:" + otherDigest.Hex
	_, err = schema.ComposeFederation([]schema.ServiceManifest{manifest}, options)
	assertCompositionCode(t, err, schema.CodeFederationUntrustedService)

	manifest = federationManifest(t, "users", "users-api", federationDocument(t, "users-r1", "User", "String", "query.user"))
	manifest.FederationProfile = "core.federation-2"
	_, err = schema.ComposeFederation([]schema.ServiceManifest{manifest}, federationOptions(manifest))
	assertCompositionCode(t, err, schema.CodeFederationProfileMismatch)
}

func TestComposeFederationPreservesAndChecksPinnedReferences(t *testing.T) {
	t.Parallel()
	common := schema.SchemaReference{
		URI: "urn:example:common", Revision: "common-r1",
		Digest: "sha256:" + strings.Repeat("a", 64),
	}
	left := federationManifest(t, "left", "left-api", federationDocumentWithReference(t, "left-r1", "Left", "query.left", common))
	right := federationManifest(t, "right", "right-api", federationDocumentWithReference(t, "right-r1", "Right", "query.right", common))
	composition, err := schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	if err != nil {
		t.Fatal(err)
	}
	references := composition.Schema().ExportOptions().References
	if len(references) != 3 || !slices.Contains(references, common) {
		t.Fatalf("composed references = %#v", references)
	}

	conflict := common
	conflict.Revision = "common-r2"
	right = federationManifest(t, "right", "right-api", federationDocumentWithReference(t, "right-r1", "Right", "query.right", conflict))
	_, err = schema.ComposeFederation([]schema.ServiceManifest{left, right}, federationOptions(left, right))
	assertCompositionCode(t, err, schema.CodeFederationTypeConflict)
}

func federationDocument(t *testing.T, revision, typeID, valueType, operationID string) schema.Document {
	t.Helper()
	input := fmt.Sprintf(`{
		"version":"1","canonicalVersion":"c14n-1","revision":%q,
		"types":[{"id":%q,"name":%q,"kind":"object","output":true,"entity":{"keys":["id"]},"fields":[
			{"id":%q,"name":"id","type":"ID"},
			{"id":%q,"name":"value","type":%q}
		]}],
		"operations":[{"id":%q,"name":%q,"kind":"query","output":%q,"effect":"read"}],
		"members":[{"id":%q,"name":"value","owner":%q,"kind":"field","output":%q,"effect":"read"}]
	}`, revision, typeID, typeID, typeID+".id", typeID+".value", valueType,
		operationID, strings.TrimPrefix(operationID, "query."), typeID, typeID+".value.resolver", typeID, valueType)
	document, err := schema.ParseDocument([]byte(input), schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	return document
}

func federationDocumentWithDirective(t *testing.T, revision, typeID, operationID string, cost uint64) schema.Document {
	t.Helper()
	input := fmt.Sprintf(`{
		"version":"1","canonicalVersion":"c14n-1","revision":%q,
		"types":[{"id":%q,"name":%q,"kind":"object","output":true,"entity":{"keys":["id"]},"fields":[
			{"id":%q,"name":"id","type":"ID"},
			{"id":%q,"name":"value","type":"String"}
		]}],
		"operations":[{"id":%q,"name":%q,"kind":"query","output":%q,"effect":"read"}],
		"members":[{"id":%q,"name":"value","owner":%q,"kind":"field","output":"String","effect":"read"}],
		"directives":[{"id":"vendor.trace","name":"trace","version":"1","capability":"vendor.trace-1","locations":["field"],"phases":["validation","execution"],"effect":"read","cost":%d,"deterministic":true,"compatibility":"dangerous"}]
	}`, revision, typeID, typeID, typeID+".id", typeID+".value", operationID,
		strings.TrimPrefix(operationID, "query."), typeID, typeID+".value.resolver", typeID, cost)
	document, err := schema.ParseDocument([]byte(input), schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	return document
}

func federationDocumentWithMetadata(t *testing.T, revision, typeID, operationID, kind, value string) schema.Document {
	t.Helper()
	document := federationDocument(t, revision, typeID, "String", operationID)
	canonical, err := document.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(canonical, &wire); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "retired":
		wire["retired"] = []any{map[string]any{"id": "Legacy", "reason": value}}
	case "trait":
		wire["traits"] = []any{map[string]any{"id": "vendor.note", "semantics": "documentation", "value": map[string]any{"owner": value}}}
	default:
		t.Fatalf("unknown metadata kind %q", kind)
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	document, err = schema.ParseDocument(encoded, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument(metadata): %v", err)
	}
	return document
}

func federationDocumentWithReference(t *testing.T, revision, typeID, operationID string, reference schema.SchemaReference) schema.Document {
	t.Helper()
	document := federationDocument(t, revision, typeID, "String", operationID)
	canonical, err := document.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(canonical, &wire); err != nil {
		t.Fatal(err)
	}
	wire["references"] = []schema.SchemaReference{reference}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	document, err = schema.ParseDocument(encoded, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("ParseDocument(reference): %v", err)
	}
	return document
}

func federationManifest(t *testing.T, serviceID, endpoint string, document schema.Document) schema.ServiceManifest {
	t.Helper()
	digest, err := document.Hash()
	if err != nil {
		t.Fatal(err)
	}
	operations := document.Operations()
	members := document.Members()
	return schema.ServiceManifest{
		ID: serviceID, Audience: "naatre:" + serviceID, EndpointReference: endpoint,
		FederationProfile: "core.federation-1",
		Schema:            document, SchemaRevision: document.Revision(), SchemaDigest: "sha256:" + digest.Hex,
		OwnedOperations: []string{operations[0].ID}, OwnedMembers: []string{members[0].ID},
	}
}

func federationOptions(manifests ...schema.ServiceManifest) schema.FederationOptions {
	services := make(map[string]schema.ServiceTrust, len(manifests))
	for _, manifest := range manifests {
		services[manifest.ID] = schema.ServiceTrust{
			Audience: manifest.Audience, EndpointReference: manifest.EndpointReference,
			FederationProfile: manifest.FederationProfile, SchemaRevision: manifest.SchemaRevision, SchemaDigest: manifest.SchemaDigest,
		}
	}
	return schema.FederationOptions{Revision: "federation-r1", Services: services}
}

func assertCompositionCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	var compositionErr *schema.CompositionError
	if !errors.As(err, &compositionErr) || compositionErr.Code != code {
		t.Fatalf("error = %T %v, want %s", err, err, code)
	}
}
