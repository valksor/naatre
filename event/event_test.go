package event_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/event"
)

type eventsFixture struct {
	Profile        string `json:"profile"`
	Specifications struct {
		CloudEvents           string `json:"cloudEvents"`
		HTTPMessageSignatures string `json:"httpMessageSignatures"`
		DigestFields          string `json:"digestFields"`
	} `json:"specifications"`
	Envelope struct {
		SpecVersion        string   `json:"specversion"`
		RequiredAttributes []string `json:"requiredAttributes"`
		UnknownTypeDefault string   `json:"unknownTypeDefault"`
		OrderingDefault    string   `json:"orderingDefault"`
	} `json:"envelope"`
	SignaturePolicy struct {
		Profile                 string   `json:"profile"`
		Algorithm               string   `json:"algorithm"`
		CoveredComponents       []string `json:"coveredComponents"`
		MaximumValiditySeconds  int64    `json:"maximumValiditySeconds"`
		DigestBeforeSignature   bool     `json:"digestBeforeSignature"`
		ExactTransmittedContent bool     `json:"exactTransmittedContent"`
		KeySelection            string   `json:"keySelection"`
	} `json:"signaturePolicy"`
	Keys               []fixtureKey             `json:"keys"`
	SignatureVectors   []fixtureSignatureVector `json:"signatureVectors"`
	NegativeSignatures []struct {
		Name          string `json:"name"`
		Vector        int    `json:"vector"`
		Member        string `json:"member"`
		Find          string `json:"find"`
		Replace       string `json:"replace"`
		ReplaceSecret []byte `json:"replaceBase64"`
		Outcome       string `json:"outcome"`
	} `json:"negativeSignatures"`
	Replay struct {
		Vector           int      `json:"vector"`
		Results          []string `json:"results"`
		StableEventID    string   `json:"stableEventId"`
		StableDeliveryID string   `json:"stableDeliveryId"`
		WindowSeconds    int64    `json:"windowSeconds"`
	} `json:"replay"`
	Retry struct {
		Delivery                         string  `json:"delivery"`
		MaximumAttempts                  uint32  `json:"maximumAttempts"`
		InitialDelayMilliseconds         int64   `json:"initialDelayMilliseconds"`
		MaximumDelayMilliseconds         int64   `json:"maximumDelayMilliseconds"`
		Multiplier                       uint32  `json:"multiplier"`
		DelaysBeforeAttemptsMilliseconds []int64 `json:"delaysBeforeAttemptsMilliseconds"`
		Terminal                         string  `json:"terminal"`
	} `json:"retry"`
	Batch struct {
		MaximumEvents uint32 `json:"maximumEvents"`
		MaximumBytes  uint64 `json:"maximumBytes"`
		Accepted      []struct {
			Events       int `json:"events"`
			Bytes        int `json:"bytes"`
			DecodedBytes int `json:"decodedBytes"`
		} `json:"accepted"`
		Rejected []struct {
			Events       int `json:"events"`
			Bytes        int `json:"bytes"`
			DecodedBytes int `json:"decodedBytes"`
		} `json:"rejected"`
	} `json:"batch"`
	VersionSkew []struct {
		Name         string `json:"name"`
		Known        bool   `json:"known"`
		Subscribed   bool   `json:"subscribed"`
		SchemaChange string `json:"schemaChange"`
		Outcome      string `json:"outcome"`
	} `json:"versionSkew"`
	Registration struct {
		Rotation struct {
			Overlap           bool   `json:"overlap"`
			Selection         string `json:"selection"`
			AmbiguousFallback bool   `json:"ambiguousFallback"`
		} `json:"rotation"`
		Revocation string `json:"revocation"`
	} `json:"registration"`
	Recovery []struct {
		Name        string   `json:"name"`
		Before      string   `json:"before"`
		Endpoint    string   `json:"endpoint"`
		After       string   `json:"after"`
		FailureCode string   `json:"failureCode"`
		Preserve    []string `json:"preserve"`
	} `json:"recovery"`
	Redaction struct {
		SafeFields               []string `json:"safeFields"`
		ForbiddenFields          []string `json:"forbiddenFields"`
		DeadLetterPayloadDefault string   `json:"deadLetterPayloadDefault"`
	} `json:"redaction"`
}

type fixtureKey struct {
	ID        string    `json:"id"`
	Sender    string    `json:"sender"`
	Audience  string    `json:"audience"`
	Secret    []byte    `json:"secretBase64"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	State     string    `json:"state"`
}

type fixtureSignatureVector struct {
	Name            string    `json:"name"`
	KeyID           string    `json:"keyId"`
	Now             time.Time `json:"now"`
	Method          string    `json:"method"`
	TargetURI       string    `json:"targetUri"`
	ContentType     string    `json:"contentType"`
	ContentEncoding string    `json:"contentEncoding"`
	DeliveryID      string    `json:"deliveryId"`
	Timestamp       int64     `json:"timestamp,string"`
	Audience        string    `json:"audience"`
	Body            []byte    `json:"bodyBase64"`
	DecodedBody     []byte    `json:"decodedBodyBase64"`
	ContentDigest   string    `json:"contentDigest"`
	SignatureInput  string    `json:"signatureInput"`
	Signature       string    `json:"signature"`
	SignatureBase   []byte    `json:"signatureBaseBase64"`
}

func TestEnvelopeCloudEventsMapping(t *testing.T) {
	t.Parallel()
	sequence := uint64(42)
	envelope := event.Envelope{
		ID: "evt_order_123", Source: "urn:naatre:orders", Type: "com.valksor.order.created.v1",
		Time: time.Date(2026, 9, 15, 11, 59, 59, 0, time.UTC), Subject: "orders/123",
		DataSchema: "https://schemas.example.test/orders/v1", SchemaRevision: "orders-r1",
		SchemaDigest: "sha256:" + strings.Repeat("a", 64), Data: json.RawMessage(`{"orderId":"123"}`),
		OrderingKey: "customer-7", Sequence: &sequence, Extensions: map[string]any{"traceparent": "trace-1"},
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var mapped map[string]any
	if err := json.Unmarshal(encoded, &mapped); err != nil {
		t.Fatal(err)
	}
	if mapped["specversion"] != event.CloudEventsSpecVersion || mapped["id"] != envelope.ID || mapped["naatreorderingkey"] != envelope.OrderingKey || mapped["naatresequence"] != float64(sequence) {
		t.Fatalf("CloudEvents mapping = %#v", mapped)
	}
	unknown := envelope
	unknown.Type = "com.example.future.event.v7"
	if err := unknown.Validate(); err != nil {
		t.Fatalf("unknown versioned event type was rejected: %v", err)
	}
	invalid := envelope
	invalid.Sequence = nil
	if err := invalid.Validate(); err == nil {
		t.Fatal("ordering key without sequence was accepted")
	}
}

func TestSignatureConformanceVectors(t *testing.T) {
	t.Parallel()
	fixture := loadEventsFixture(t)
	validateFixtureHeader(t, fixture)
	keys := fixtureKeys(t, fixture.Keys)
	for _, vector := range fixture.SignatureVectors {
		t.Run(vector.Name, func(t *testing.T) { checkSignatureVector(t, fixture, keys, vector) })
	}
}

func validateFixtureHeader(t *testing.T, fixture eventsFixture) {
	t.Helper()
	if fixture.Profile != event.Profile || fixture.Specifications.CloudEvents != event.CloudEventsVersion ||
		fixture.Specifications.HTTPMessageSignatures != "RFC 9421" || fixture.Specifications.DigestFields != "RFC 9530" ||
		fixture.SignaturePolicy.Profile != event.SignatureProfile || fixture.SignaturePolicy.Algorithm != event.SignatureAlgorithm ||
		!fixture.SignaturePolicy.DigestBeforeSignature || !fixture.SignaturePolicy.ExactTransmittedContent || fixture.SignaturePolicy.KeySelection != "exact-keyid-only" {
		t.Fatal("event fixture header is incomplete")
	}
	if time.Duration(fixture.SignaturePolicy.MaximumValiditySeconds)*time.Second != event.MaximumSignatureValidity {
		t.Fatal("event signature validity exceeds the portable profile")
	}
	if len(fixture.Keys) < 2 || fixture.Keys[0].ID == fixture.Keys[1].ID ||
		!fixture.Keys[0].NotBefore.Before(fixture.Keys[1].NotAfter) || !fixture.Keys[1].NotBefore.Before(fixture.Keys[0].NotAfter) {
		t.Fatal("event fixture does not prove unambiguous overlapping key rotation")
	}
}

func checkSignatureVector(t *testing.T, fixture eventsFixture, keys map[string]event.VerificationKey, vector fixtureSignatureVector) {
	t.Helper()
	key := keys[vector.KeyID]
	message := fixtureMessage(vector)
	created := time.Unix(vector.Timestamp, 0)
	signed, err := event.Sign(message, event.SigningKey{ID: key.ID, Secret: key.Secret}, created, created.Add(time.Duration(fixture.SignaturePolicy.MaximumValiditySeconds)*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if signed.ContentDigest != vector.ContentDigest || signed.SignatureInput != vector.SignatureInput || signed.Signature != vector.Signature {
		t.Fatalf("signature vector mismatch: %#v", signed)
	}
	verifier := fixtureVerifier(t, fixture, keys, vector.Now, new(event.MemoryReplayStore))
	result, err := verifier.Verify(signed)
	if err != nil || result.Sender != key.Sender || result.KeyID != key.ID || result.Replay {
		t.Fatalf("verification = %#v, %v", result, err)
	}
	if vector.ContentEncoding == event.GZIPEncoding {
		checkGZIPBody(t, vector)
	}
}

func checkGZIPBody(t *testing.T, vector fixtureSignatureVector) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(vector.Body))
	if err != nil {
		t.Fatal(err)
	}
	decoded, readErr := io.ReadAll(reader)
	if readErr != nil || reader.Close() != nil || !bytes.Equal(decoded, vector.DecodedBody) {
		t.Fatalf("gzip body mismatch: %v", readErr)
	}
}

func TestSignatureFailuresAndReplay(t *testing.T) {
	t.Parallel()
	fixture := loadEventsFixture(t)
	keys := fixtureKeys(t, fixture.Keys)
	for _, negative := range fixture.NegativeSignatures {
		t.Run(negative.Name, func(t *testing.T) {
			vector := fixture.SignatureVectors[negative.Vector]
			message := fixtureMessage(vector)
			now := vector.Now
			localKeys := keys
			var expected error
			var expectedOutcome string
			switch negative.Member {
			case "body":
				message.Body = []byte(strings.Replace(string(message.Body), negative.Find, negative.Replace, 1))
				expected, expectedOutcome = event.ErrDigest, "reject-digest"
			case "targetUri":
				message.TargetURI = negative.Replace
				expected, expectedOutcome = event.ErrSignature, "reject-signature"
			case "method":
				message.Method = negative.Replace
				expected, expectedOutcome = event.ErrInvalidMessage, "reject-message"
			case "now":
				parsed, err := time.Parse(time.RFC3339, negative.Replace)
				if err != nil {
					t.Fatal(err)
				}
				now = parsed
				expected, expectedOutcome = event.ErrFreshness, "reject-freshness"
			case "secret":
				wrong := keys[vector.KeyID]
				wrong.Secret = slices.Clone(negative.ReplaceSecret)
				localKeys = map[string]event.VerificationKey{wrong.ID: wrong}
				expected, expectedOutcome = event.ErrSignature, "reject-signature"
			default:
				t.Fatalf("unknown negative member %q", negative.Member)
			}
			verifier := fixtureVerifier(t, fixture, localKeys, now, new(event.MemoryReplayStore))
			if _, err := verifier.Verify(message); !errors.Is(err, expected) || negative.Outcome != expectedOutcome {
				t.Fatalf("modified webhook outcome = %q, %v; want %q, %v", negative.Outcome, err, expectedOutcome, expected)
			}
		})
	}

	vector := fixture.SignatureVectors[fixture.Replay.Vector]
	var replayEvent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(vector.Body, &replayEvent); err != nil {
		t.Fatal(err)
	}
	store := new(event.MemoryReplayStore)
	verifier := fixtureVerifier(t, fixture, keys, vector.Now, store)
	first, firstErr := verifier.Verify(fixtureMessage(vector))
	second, secondErr := verifier.Verify(fixtureMessage(vector))
	if firstErr != nil || secondErr != nil || first.Replay || !second.Replay ||
		!slices.Equal(fixture.Replay.Results, []string{"fresh", "duplicate"}) || fixture.Replay.StableDeliveryID != vector.DeliveryID ||
		fixture.Replay.StableEventID != replayEvent.ID || fixture.Replay.WindowSeconds != fixture.SignaturePolicy.MaximumValiditySeconds {
		t.Fatalf("replay results = %#v, %#v, %v, %v", first, second, firstErr, secondErr)
	}
	expired := new(event.MemoryReplayStore)
	if replay, err := expired.CheckAndStore("endpoint-1", "wh-expired", vector.Now.Add(-2*time.Second), vector.Now.Add(-time.Second)); err != nil || replay {
		t.Fatalf("initial replay record = %t, %v", replay, err)
	}
	if replay, err := expired.CheckAndStore("endpoint-1", "wh-expired", vector.Now, vector.Now.Add(time.Second)); err != nil || replay {
		t.Fatalf("expired replay record = %t, %v", replay, err)
	}
}

func TestPortableSignatureValidity(t *testing.T) {
	t.Parallel()
	fixture := loadEventsFixture(t)
	vector := fixture.SignatureVectors[0]
	key := fixtureKeys(t, fixture.Keys)[vector.KeyID]
	created := time.Unix(vector.Timestamp, 0)
	if _, err := event.Sign(fixtureMessage(vector), event.SigningKey{ID: key.ID, Secret: key.Secret}, created, created.Add(event.MaximumSignatureValidity+time.Second)); !errors.Is(err, event.ErrInvalidMessage) {
		t.Fatalf("oversized signing window error = %v", err)
	}
	_, err := event.NewVerifier(event.VerifierConfig{
		ResolveKey:      func(string) (event.VerificationKey, bool) { return event.VerificationKey{}, false },
		ReplayStore:     new(event.MemoryReplayStore),
		MaximumValidity: event.MaximumSignatureValidity + time.Second,
	})
	if err == nil {
		t.Fatal("receiver accepted a validity window above the portable maximum")
	}
}

func TestDeliveryContracts(t *testing.T) {
	t.Parallel()
	fixture := loadEventsFixture(t)
	endpoint := event.EndpointRegistration{
		ID: "endpoint_orders_1", Tenant: "tenant-7", Revision: "endpoint-r3",
		URL: "https://hooks.example.test/v1/events", Status: event.EndpointActive, Ordering: event.OrderingStream,
		AllowedEventTypes: []string{"com.valksor.order.created.v1"}, ResolvedAddresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")},
	}
	testEndpointRegistration(t, endpoint)
	testRetryPolicy(t, fixture)
	testBatchLimits(t, fixture)
	testDeliveryRecovery(t, fixture, endpoint)
}

func testEndpointRegistration(t *testing.T, endpoint event.EndpointRegistration) {
	t.Helper()
	if err := event.ValidateEndpointRegistration(endpoint); err != nil {
		t.Fatal(err)
	}
	private := endpoint
	private.ResolvedAddresses = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if !errors.Is(event.ValidateEndpointRegistration(private), event.ErrInvalidEndpoint) {
		t.Fatal("loopback endpoint was accepted")
	}
	documentation := endpoint
	documentation.ResolvedAddresses = []netip.Addr{netip.MustParseAddr("192.0.2.1")}
	if !errors.Is(event.ValidateEndpointRegistration(documentation), event.ErrInvalidEndpoint) {
		t.Fatal("documentation endpoint was accepted")
	}
	privateLiteral := endpoint
	privateLiteral.URL = "https://127.0.0.1/v1/events"
	if !errors.Is(event.ValidateEndpointRegistration(privateLiteral), event.ErrInvalidEndpoint) {
		t.Fatal("private literal endpoint was accepted with a public address claim")
	}
	publicLiteral := endpoint
	publicLiteral.URL = "https://93.184.216.35/v1/events"
	if !errors.Is(event.ValidateEndpointRegistration(publicLiteral), event.ErrInvalidEndpoint) {
		t.Fatal("public literal endpoint was accepted with a mismatched address claim")
	}
}

func testRetryPolicy(t *testing.T, fixture eventsFixture) {
	t.Helper()
	policy := event.RetryPolicy{
		MaximumAttempts: fixture.Retry.MaximumAttempts,
		InitialDelay:    time.Duration(fixture.Retry.InitialDelayMilliseconds) * time.Millisecond,
		MaximumDelay:    time.Duration(fixture.Retry.MaximumDelayMilliseconds) * time.Millisecond,
		Multiplier:      fixture.Retry.Multiplier,
	}
	for index, expected := range fixture.Retry.DelaysBeforeAttemptsMilliseconds {
		delay, err := policy.DelayBefore(uint32(index + 2))
		if err != nil || delay != time.Duration(expected)*time.Millisecond {
			t.Fatalf("retry attempt %d = %v, %v", index+2, delay, err)
		}
	}
	if fixture.Retry.Delivery != "at-least-once" || fixture.Retry.Terminal != "dead-lettered" {
		t.Fatal("retry delivery contract is incomplete")
	}
}

func testBatchLimits(t *testing.T, fixture eventsFixture) {
	t.Helper()
	limits := event.BatchLimits{MaximumEvents: fixture.Batch.MaximumEvents, MaximumBytes: fixture.Batch.MaximumBytes}
	for _, accepted := range fixture.Batch.Accepted {
		if err := limits.Validate(accepted.Events, accepted.Bytes, accepted.DecodedBytes); err != nil {
			t.Fatalf("accepted batch failed: %v", err)
		}
	}
	for _, rejected := range fixture.Batch.Rejected {
		if err := limits.Validate(rejected.Events, rejected.Bytes, rejected.DecodedBytes); err == nil {
			t.Fatal("oversized batch was accepted")
		}
	}
}

func testDeliveryRecovery(t *testing.T, fixture eventsFixture, endpoint event.EndpointRegistration) {
	t.Helper()
	record := event.DeliveryRecord{
		Tenant: endpoint.Tenant, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision,
		DeliveryID: "wh_01", EventIDs: []string{"evt_order_123"}, PayloadReference: "outbox:42",
		State: event.DeliveryInFlight, Attempt: 2, LeaseOwner: "worker-before-crash",
	}
	recovered, err := event.RecoverDelivery(record, endpoint)
	if err != nil || recovered.State != event.DeliveryPending || recovered.LeaseOwner != "" || !deliveryIdentityEqual(record, recovered) {
		t.Fatalf("crash recovery = %#v, %v", recovered, err)
	}
	revoked := endpoint
	revoked.Status = event.EndpointRevoked
	dead, err := event.RecoverDelivery(record, revoked)
	if err != nil || dead.State != event.DeliveryDeadLettered || dead.FailureCode != "ENDPOINT_REVOKED" || !deliveryIdentityEqual(record, dead) {
		t.Fatalf("revocation recovery = %#v, %v", dead, err)
	}
	otherTenant := endpoint
	otherTenant.Tenant = "tenant-8"
	if _, err := event.RecoverDelivery(record, otherTenant); !errors.Is(err, event.ErrDeliveryOwnership) {
		t.Fatal("delivery was rebound across tenants")
	}
	finished := record
	finished.State = event.DeliverySucceeded
	if _, err := event.RecoverDelivery(finished, endpoint); !errors.Is(err, event.ErrDeliveryOwnership) {
		t.Fatal("finished delivery was reopened during crash recovery")
	}
	if len(fixture.Recovery) != 2 || fixture.Registration.Revocation != "before-every-attempt" || !fixture.Registration.Rotation.Overlap || fixture.Registration.Rotation.AmbiguousFallback {
		t.Fatal("recovery or rotation fixture is incomplete")
	}
}

func TestCompatibilityAndRedactionFixtures(t *testing.T) {
	t.Parallel()
	fixture := loadEventsFixture(t)
	testVersionSkew(t, fixture)
	testRedaction(t, fixture)
}

func testVersionSkew(t *testing.T, fixture eventsFixture) {
	t.Helper()
	type expectation struct {
		known, subscribed bool
		schemaChange      string
		outcome           string
	}
	wantSkew := map[string]expectation{
		"unknown-optional-type":   {false, false, "additive", "authenticate-acknowledge-ignore"},
		"unknown-subscribed-type": {false, true, "additive", "dead-letter-unsupported-type"},
		"known-additive-schema":   {true, true, "additive", "deliver"},
		"known-breaking-schema":   {true, true, "breaking", "dead-letter-schema-incompatible"},
	}
	for _, vector := range fixture.VersionSkew {
		expected, exists := wantSkew[vector.Name]
		if !exists || expected != (expectation{vector.Known, vector.Subscribed, vector.SchemaChange, vector.Outcome}) {
			t.Fatalf("version skew vector = %#v", vector)
		}
		delete(wantSkew, vector.Name)
	}
	if len(wantSkew) != 0 || fixture.Envelope.UnknownTypeDefault != "authenticate-acknowledge-ignore" || fixture.Envelope.OrderingDefault != "absent" {
		t.Fatal("version skew or ordering defaults are incomplete")
	}
}

func testRedaction(t *testing.T, fixture eventsFixture) {
	t.Helper()
	observation := event.DeliveryObservation{
		TenantReference: "tenant-ref", EndpointReference: "endpoint-ref", DeliveryID: "wh_01",
		EventIDs: []string{"evt_order_123"}, EventTypes: []string{"com.valksor.order.created.v1"}, Attempt: 2,
		Outcome: "retry", FailureCode: "HTTP_503", HTTPStatus: 503, NextAttempt: time.Date(2026, 9, 15, 12, 2, 0, 0, time.UTC),
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range fixture.Redaction.SafeFields {
		if _, ok := fields[field]; !ok {
			t.Errorf("safe observation field %q is absent", field)
		}
	}
	for _, field := range fixture.Redaction.ForbiddenFields {
		if _, ok := fields[field]; ok || strings.Contains(string(encoded), field) {
			t.Errorf("forbidden observation field %q is exposed", field)
		}
	}
	if fixture.Redaction.DeadLetterPayloadDefault != "opaque-reference-only" {
		t.Fatal("dead-letter redaction default is missing")
	}
}

func loadEventsFixture(t *testing.T) eventsFixture {
	t.Helper()
	content, err := os.ReadFile("../conformance/v1/events.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture eventsFixture
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func fixtureKeys(t *testing.T, values []fixtureKey) map[string]event.VerificationKey {
	t.Helper()
	keys := make(map[string]event.VerificationKey, len(values))
	for _, value := range values {
		if _, exists := keys[value.ID]; exists {
			t.Fatalf("duplicate fixture key %q", value.ID)
		}
		keys[value.ID] = event.VerificationKey{
			ID: value.ID, Sender: value.Sender, Audience: value.Audience, Secret: slices.Clone(value.Secret),
			NotBefore: value.NotBefore, NotAfter: value.NotAfter, Revoked: value.State == "revoked",
		}
	}
	return keys
}

func fixtureMessage(vector fixtureSignatureVector) event.Message {
	return event.Message{
		Method: vector.Method, TargetURI: vector.TargetURI, ContentType: vector.ContentType, ContentEncoding: vector.ContentEncoding,
		ContentDigest: vector.ContentDigest, DeliveryID: vector.DeliveryID, Timestamp: strconv.FormatInt(vector.Timestamp, 10), Audience: vector.Audience,
		SignatureInput: vector.SignatureInput, Signature: vector.Signature, Body: slices.Clone(vector.Body),
	}
}

func fixtureVerifier(t *testing.T, fixture eventsFixture, keys map[string]event.VerificationKey, now time.Time, store event.ReplayStore) *event.Verifier {
	t.Helper()
	verifier, err := event.NewVerifier(event.VerifierConfig{
		ResolveKey:  func(id string) (event.VerificationKey, bool) { key, ok := keys[id]; return key, ok },
		ReplayStore: store, Now: func() time.Time { return now }, ClockSkew: 0,
		MaximumValidity: time.Duration(fixture.SignaturePolicy.MaximumValiditySeconds) * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func deliveryIdentityEqual(left, right event.DeliveryRecord) bool {
	return left.Tenant == right.Tenant && left.EndpointID == right.EndpointID && left.EndpointRevision == right.EndpointRevision &&
		left.DeliveryID == right.DeliveryID && reflect.DeepEqual(left.EventIDs, right.EventIDs) && left.PayloadReference == right.PayloadReference
}
