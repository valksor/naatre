package conformance_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

type federationFixture struct {
	Profile               string                         `json:"profile"`
	HashPurpose           string                         `json:"hashPurpose"`
	Signature             string                         `json:"signature"`
	VerifierAuthority     string                         `json:"verifierAuthority"`
	RequiredClaims        []string                       `json:"requiredClaims"`
	CompositionErrorCodes []string                       `json:"compositionErrorCodes"`
	ExecutionErrorCodes   []string                       `json:"executionErrorCodes"`
	Services              []federationServiceFixture     `json:"services"`
	Trust                 map[string]schema.ServiceTrust `json:"trust"`
	Composition           federationCompositionFixture   `json:"composition"`
	CompositionCases      []federationCompositionCase    `json:"compositionCases"`
	Delegation            federationDelegationFixture    `json:"delegation"`
	DelegationCases       []federationDelegationCase     `json:"delegationCases"`
	ExecutionCases        []federationExecutionCase      `json:"executionCases"`
}

type federationServiceFixture struct {
	Name              string               `json:"name"`
	ID                string               `json:"id"`
	Audience          string               `json:"audience"`
	EndpointReference string               `json:"endpointReference"`
	FederationProfile string               `json:"federationProfile"`
	SchemaRevision    string               `json:"schemaRevision"`
	SchemaDigest      string               `json:"schemaDigest"`
	Schema            json.RawMessage      `json:"schema"`
	OwnedOperations   []string             `json:"ownedOperations"`
	OwnedMembers      []string             `json:"ownedMembers"`
	EntityFetches     []schema.EntityFetch `json:"entityFetches"`
}

type federationCompositionFixture struct {
	Manifests     []string `json:"manifests"`
	Revision      string   `json:"revision"`
	CanonicalJSON string   `json:"canonicalJson"`
	Hash          string   `json:"hash"`
}

type federationCompositionCase struct {
	Name              string                          `json:"name"`
	Manifests         []string                        `json:"manifests"`
	EndpointOverrides map[string]string               `json:"endpointOverrides,omitempty"`
	ProfileOverrides  map[string]string               `json:"profileOverrides,omitempty"`
	TrustOverrides    map[string]schema.ServiceTrust  `json:"trustOverrides,omitempty"`
	EntityFetches     map[string][]schema.EntityFetch `json:"entityFetches,omitempty"`
	ExpectedCode      string                          `json:"expectedCode"`
}

type federationDelegationFixture struct {
	PrivateKeySeed string                                  `json:"privateKeySeed"`
	Issuer         string                                  `json:"issuer"`
	Now            string                                  `json:"now"`
	Principal      runtime.Principal                       `json:"principal"`
	Options        federationDelegationOptionsFixture      `json:"options"`
	Expectation    runtime.FederationDelegationExpectation `json:"expectation"`
	ExpectedToken  string                                  `json:"expectedToken"`
}

type federationDelegationOptionsFixture struct {
	Audience              string `json:"audience"`
	RequestID             string `json:"requestId"`
	OperationID           string `json:"operationId"`
	SchemaRevision        string `json:"schemaRevision"`
	ServiceSchemaRevision string `json:"serviceSchemaRevision"`
	ServiceSchemaDigest   string `json:"serviceSchemaDigest"`
	Deadline              string `json:"deadline"`
	Cost                  uint64 `json:"cost"`
	Concurrency           uint64 `json:"concurrency"`
}

type federationDelegationCase struct {
	Name             string `json:"name"`
	Mutation         string `json:"mutation"`
	VerifierAudience string `json:"verifierAudience"`
	Expected         string `json:"expected"`
}

type federationExecutionCase struct {
	Name                  string                    `json:"name"`
	Plan                  federationPlanFixture     `json:"plan"`
	Limits                federationLimitsFixture   `json:"limits"`
	MaximumDurationMillis int64                     `json:"maximumDurationMillis"`
	Remote                []federationRemoteFixture `json:"remote"`
	ExpectedData          json.RawMessage           `json:"expectedData"`
	ExpectedErrors        []federationExpectedError `json:"expectedErrors"`
	ExpectedInvocations   int64                     `json:"expectedInvocations"`
}

type federationPlanFixture struct {
	SchemaRevision string                  `json:"schemaRevision"`
	Calls          []federationCallFixture `json:"calls"`
}

type federationCallFixture struct {
	ResponseKey string          `json:"responseKey"`
	ServiceID   string          `json:"serviceId"`
	OperationID string          `json:"operationId"`
	Input       json.RawMessage `json:"input,omitempty"`
	Path        []any           `json:"path"`
	MaxAttempts uint32          `json:"maxAttempts"`
}

type federationLimitsFixture struct {
	MaxCalls       uint64 `json:"maxCalls"`
	MaxCost        uint64 `json:"maxCost"`
	MaxConcurrency uint64 `json:"maxConcurrency"`
	MaxAttempts    uint32 `json:"maxAttempts"`
}

type federationRemoteFixture struct {
	ServiceID      string                          `json:"serviceId"`
	Data           json.RawMessage                 `json:"data,omitempty"`
	SchemaRevision string                          `json:"schemaRevision"`
	TransportError bool                            `json:"transportError,omitempty"`
	WaitForCancel  bool                            `json:"waitForCancel,omitempty"`
	Errors         []runtime.FederationRemoteError `json:"errors,omitempty"`
}

type federationExpectedError struct {
	Code string `json:"code"`
	Path []any  `json:"path"`
}

func TestPortableFederationContractVectors(t *testing.T) {
	t.Parallel()
	var fixture federationFixture
	readFixture(t, "federation.json", &fixture)
	assertFederationHeader(t, fixture)
	services := indexFederationServices(t, fixture.Services)
	composition := composeFederationFixture(t, services, fixture.Trust, fixture.Composition)
	canonical, err := composition.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.Composition.CanonicalJSON {
		t.Fatalf("composition canonical JSON = %s", canonical)
	}
	hash, err := composition.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if hash.Hex != fixture.Composition.Hash {
		t.Fatalf("composition hash = %s", hash.Hex)
	}
	for _, test := range fixture.CompositionCases {
		test := test
		t.Run("composition/"+test.Name, func(t *testing.T) {
			runFederationCompositionCase(t, services, fixture.Trust, fixture.Composition.Revision, test)
		})
	}
	for _, test := range fixture.DelegationCases {
		test := test
		t.Run("delegation/"+test.Name, func(t *testing.T) {
			runFederationDelegationCase(t, fixture.Delegation, test)
		})
	}
	for _, test := range fixture.ExecutionCases {
		test := test
		t.Run("execution/"+test.Name, func(t *testing.T) {
			runFederationExecutionCase(t, composition, fixture.Delegation, test)
		})
	}
}

func assertFederationHeader(t *testing.T, fixture federationFixture) {
	t.Helper()
	if fixture.Profile != "core.federation-1" || fixture.HashPurpose != "federation" ||
		fixture.Signature != "ed25519" || fixture.VerifierAuthority != "public-key-only" {
		t.Fatal("federation fixture header is incomplete")
	}
	assertExactStrings(t, "federation composition error codes", fixture.CompositionErrorCodes, []string{
		"FEDERATION_INVALID_MANIFEST", "FEDERATION_UNTRUSTED_SERVICE", "FEDERATION_OWNERSHIP_CONFLICT",
		"FEDERATION_PROFILE_MISMATCH", "FEDERATION_SCHEMA_MISMATCH", "FEDERATION_TYPE_CONFLICT", "FEDERATION_ENTITY_CYCLE",
	})
	assertExactStrings(t, "federation execution error codes", fixture.ExecutionErrorCodes, []string{
		"FEDERATION_PLAN_INVALID", "FEDERATION_SCHEMA_MISMATCH", "FEDERATION_UNAVAILABLE",
		"UNAUTHORIZED", "RESOURCE_EXHAUSTED", "CANCELLED",
	})
	for _, values := range [][]string{fixture.CompositionErrorCodes, fixture.ExecutionErrorCodes} {
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			if value == "" || seen[value] {
				t.Fatalf("empty or duplicate federation error code %q", value)
			}
			seen[value] = true
		}
	}
	for _, claim := range []string{
		"issuer", "audience", "subject", "authorizationRevision", "requestId", "operationId",
		"schemaRevision", "serviceSchemaRevision", "serviceSchemaDigest", "expiresUnixNano", "cost", "concurrency",
	} {
		if !slices.Contains(fixture.RequiredClaims, claim) {
			t.Errorf("federation fixture lacks required claim %q", claim)
		}
	}
	required := []string{
		"malicious-endpoint-reference", "untrusted-schema-provenance", "service-profile-version-skew",
		"entity-fetch-cycle", "ownership-conflict", "type-conflict",
		"valid-delegation", "forged-delegation", "wrong-audience",
		"successful-multi-service-query", "partial-failure-retains-sibling-data", "deadline-stops-scheduling",
		"remote-schema-drift", "rolling-upgrade-version-skew", "multiplicative-retry-cost-fanout",
	}
	available := make(map[string]bool)
	for _, value := range fixture.CompositionCases {
		if !slices.Contains(fixture.CompositionErrorCodes, value.ExpectedCode) {
			t.Errorf("composition case %q uses unregistered code %q", value.Name, value.ExpectedCode)
		}
		available[value.Name] = true
	}
	for _, value := range fixture.DelegationCases {
		available[value.Name] = true
	}
	for _, value := range fixture.ExecutionCases {
		for _, expected := range value.ExpectedErrors {
			if !slices.Contains(fixture.ExecutionErrorCodes, expected.Code) {
				t.Errorf("execution case %q uses unregistered code %q", value.Name, expected.Code)
			}
		}
		available[value.Name] = true
	}
	for _, name := range required {
		if !available[name] {
			t.Errorf("federation fixture lacks %q", name)
		}
	}
}

func indexFederationServices(t *testing.T, input []federationServiceFixture) map[string]federationServiceFixture {
	t.Helper()
	result := make(map[string]federationServiceFixture, len(input))
	for _, service := range input {
		if service.Name == "" || service.ID == "" || len(service.Schema) == 0 {
			t.Fatalf("incomplete federation service fixture %#v", service)
		}
		if _, exists := result[service.Name]; exists {
			t.Fatalf("duplicate federation service fixture %q", service.Name)
		}
		result[service.Name] = service
	}
	return result
}

func parseFederationManifest(t *testing.T, service federationServiceFixture) schema.ServiceManifest {
	t.Helper()
	document, err := schema.ParseDocument(service.Schema, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("parse service %s schema: %v", service.Name, err)
	}
	digest, err := document.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if service.SchemaDigest != "sha256:"+digest.Hex {
		t.Fatalf("service %s schema digest = sha256:%s", service.Name, digest.Hex)
	}
	return schema.ServiceManifest{
		ID: service.ID, Audience: service.Audience, EndpointReference: service.EndpointReference,
		FederationProfile: service.FederationProfile,
		Schema:            document, SchemaRevision: service.SchemaRevision, SchemaDigest: service.SchemaDigest,
		OwnedOperations: slices.Clone(service.OwnedOperations), OwnedMembers: slices.Clone(service.OwnedMembers),
		EntityFetches: slices.Clone(service.EntityFetches),
	}
}

func composeFederationFixture(t *testing.T, services map[string]federationServiceFixture, trust map[string]schema.ServiceTrust, fixture federationCompositionFixture) schema.FederationComposition {
	t.Helper()
	manifests := make([]schema.ServiceManifest, 0, len(fixture.Manifests))
	for _, name := range fixture.Manifests {
		service, ok := services[name]
		if !ok {
			t.Fatalf("unknown federation service fixture %q", name)
		}
		manifests = append(manifests, parseFederationManifest(t, service))
	}
	composition, err := schema.ComposeFederation(manifests, schema.FederationOptions{Revision: fixture.Revision, Services: trust})
	if err != nil {
		t.Fatalf("compose federation fixture: %v", err)
	}
	return composition
}

func runFederationCompositionCase(t *testing.T, services map[string]federationServiceFixture, trust map[string]schema.ServiceTrust, revision string, test federationCompositionCase) {
	t.Helper()
	caseTrust := make(map[string]schema.ServiceTrust, len(trust)+len(test.TrustOverrides))
	for id, value := range trust {
		caseTrust[id] = value
	}
	for id, value := range test.TrustOverrides {
		caseTrust[id] = value
	}
	manifests := make([]schema.ServiceManifest, 0, len(test.Manifests))
	for _, name := range test.Manifests {
		manifest := parseFederationManifest(t, services[name])
		if value, ok := test.EndpointOverrides[name]; ok {
			manifest.EndpointReference = value
		}
		if value, ok := test.ProfileOverrides[name]; ok {
			manifest.FederationProfile = value
		}
		if value, ok := test.EntityFetches[name]; ok {
			manifest.EntityFetches = slices.Clone(value)
		}
		manifests = append(manifests, manifest)
	}
	_, err := schema.ComposeFederation(manifests, schema.FederationOptions{Revision: revision, Services: caseTrust})
	var compositionErr *schema.CompositionError
	if !errors.As(err, &compositionErr) || compositionErr.Code != test.ExpectedCode {
		t.Fatalf("composition error = %#v (%v), want %s", compositionErr, err, test.ExpectedCode)
	}
}

func runFederationDelegationCase(t *testing.T, fixture federationDelegationFixture, test federationDelegationCase) {
	t.Helper()
	seed, err := base64.RawURLEncoding.DecodeString(fixture.PrivateKeySeed)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("delegation seed: %v", err)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	now, err := time.Parse(time.RFC3339Nano, fixture.Now)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, fixture.Options.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := runtime.NewFederationDelegationIssuer(runtime.FederationDelegationIssuerConfig{
		PrivateKey: privateKey, Issuer: fixture.Issuer, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.Issue(runtime.WithPrincipal(context.Background(), fixture.Principal), runtime.FederationDelegationOptions{
		Audience: fixture.Options.Audience, RequestID: fixture.Options.RequestID, OperationID: fixture.Options.OperationID,
		SchemaRevision: fixture.Options.SchemaRevision, ServiceSchemaRevision: fixture.Options.ServiceSchemaRevision,
		ServiceSchemaDigest: fixture.Options.ServiceSchemaDigest, Deadline: deadline,
		Cost: fixture.Options.Cost, Concurrency: fixture.Options.Concurrency,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.ExpectedToken == "" || token != fixture.ExpectedToken {
		t.Fatalf("delegation token = %s", token)
	}
	if private := fixture.Principal.Claims["private"]; private != "" && strings.Contains(decodedFederationPayload(t, token), private) {
		t.Fatal("delegation contains arbitrary principal claim")
	}
	if test.Mutation == "signature" {
		payload, signature, ok := strings.Cut(token, ".")
		if !ok || signature == "" {
			t.Fatal("delegation token has no signature")
		}
		prefix := byte('A')
		if signature[0] == prefix {
			prefix = 'B'
		}
		token = payload + "." + string(prefix) + signature[1:]
	}
	verifier, err := runtime.NewFederationDelegationVerifier(runtime.FederationDelegationVerifierConfig{
		PublicKey: privateKey.Public().(ed25519.PublicKey), Issuer: fixture.Issuer,
		Audience: test.VerifierAudience, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	delegation, err := verifier.Verify(token, fixture.Expectation)
	if test.Expected == "ok" {
		if err != nil || delegation.Subject != fixture.Principal.Subject || delegation.AuthorizationRevision != fixture.Principal.AuthorizationRevision {
			t.Fatalf("delegation = %#v, %v", delegation, err)
		}
		return
	}
	if test.Expected != "invalid-delegation" || !errors.Is(err, runtime.ErrFederationDelegation) {
		t.Fatalf("delegation error = %v, want %s", err, test.Expected)
	}
}

func decodedFederationPayload(t *testing.T, token string) string {
	t.Helper()
	payload, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatal("delegation token has no separator")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}

func runFederationExecutionCase(t *testing.T, composition schema.FederationComposition, delegation federationDelegationFixture, test federationExecutionCase) {
	t.Helper()
	seed, err := base64.RawURLEncoding.DecodeString(delegation.PrivateKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := runtime.NewFederationDelegationIssuer(runtime.FederationDelegationIssuerConfig{
		PrivateKey: ed25519.NewKeyFromSeed(seed), Issuer: delegation.Issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := make(map[string]federationRemoteFixture, len(test.Remote))
	for _, value := range test.Remote {
		remote[value.ServiceID] = value
	}
	var invocations atomic.Int64
	invoker := runtime.FederationInvokerFunc(func(ctx context.Context, invocation runtime.FederationInvocation) (runtime.FederationRemoteResult, error) {
		invocations.Add(1)
		behavior, ok := remote[invocation.ServiceID]
		if !ok {
			return runtime.FederationRemoteResult{}, errors.New("fixture has no downstream behavior")
		}
		if behavior.WaitForCancel {
			<-ctx.Done()
			return runtime.FederationRemoteResult{}, ctx.Err()
		}
		if behavior.TransportError {
			return runtime.FederationRemoteResult{}, errors.New("fixture transport unavailable")
		}
		var data any
		if len(behavior.Data) != 0 {
			if err := json.Unmarshal(behavior.Data, &data); err != nil {
				return runtime.FederationRemoteResult{}, err
			}
		}
		return runtime.FederationRemoteResult{Data: data, SchemaRevision: behavior.SchemaRevision, Errors: behavior.Errors}, nil
	})
	coordinator, err := runtime.NewReferenceFederationCoordinator(runtime.ReferenceFederationConfig{
		Composition: composition, Delegations: issuer, Invoker: invoker, Limits: runtime.FederationLimits{
			MaxCalls: test.Limits.MaxCalls, MaxCost: test.Limits.MaxCost,
			MaxConcurrency: test.Limits.MaxConcurrency, MaxAttempts: test.Limits.MaxAttempts,
		},
		MaximumDuration: time.Duration(test.MaximumDurationMillis) * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtime.WithPrincipal(context.Background(), delegation.Principal)
	outcome := coordinator.Execute(ctx, delegation.Options.RequestID, portableFederationPlan(t, test.Plan))
	var expectedData any
	if err := json.Unmarshal(test.ExpectedData, &expectedData); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(outcome.Data, expectedData) {
		t.Fatalf("execution data = %#v, want %#v", outcome.Data, expectedData)
	}
	if len(outcome.Errors) != len(test.ExpectedErrors) {
		t.Fatalf("execution errors = %#v, want %#v", outcome.Errors, test.ExpectedErrors)
	}
	for index, expected := range test.ExpectedErrors {
		if outcome.Errors[index].Code != expected.Code || !reflect.DeepEqual(outcome.Errors[index].Path, expected.Path) {
			t.Fatalf("execution error %d = %#v, want %#v", index, outcome.Errors[index], expected)
		}
	}
	if got := invocations.Load(); got != test.ExpectedInvocations {
		t.Fatalf("execution invocations = %d, want %d", got, test.ExpectedInvocations)
	}
}

func portableFederationPlan(t *testing.T, fixture federationPlanFixture) runtime.FederationPlan {
	t.Helper()
	plan := runtime.FederationPlan{SchemaRevision: fixture.SchemaRevision, Calls: make([]runtime.FederationCall, len(fixture.Calls))}
	for index, call := range fixture.Calls {
		var input any
		if len(call.Input) != 0 {
			if err := json.Unmarshal(call.Input, &input); err != nil {
				t.Fatalf("decode federation call input: %v", err)
			}
		}
		path := make([]any, len(call.Path))
		for pathIndex, segment := range call.Path {
			switch value := segment.(type) {
			case string:
				path[pathIndex] = value
			case float64:
				indexValue := uint64(value)
				if value < 0 || float64(indexValue) != value {
					t.Fatalf("invalid portable federation path index %v", value)
				}
				path[pathIndex] = indexValue
			default:
				t.Fatalf("invalid portable federation path segment %#v", segment)
			}
		}
		plan.Calls[index] = runtime.FederationCall{
			ResponseKey: call.ResponseKey, ServiceID: call.ServiceID, OperationID: call.OperationID,
			Input: input, Path: path, MaxAttempts: call.MaxAttempts,
		}
	}
	return plan
}
