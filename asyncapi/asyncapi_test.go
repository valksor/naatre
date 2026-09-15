package asyncapi_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/asyncapi"
	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func TestPinnedConformanceFixture(t *testing.T) {
	t.Parallel()
	input, err := os.ReadFile("../conformance/v1/asyncapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := asyncapi.ValidateConformanceFixture(input); err != nil {
		t.Fatal(err)
	}
}

func TestExportIsByteStableAndLinksEveryDeclaration(t *testing.T) {
	t.Parallel()
	model := fixtureModel(t)
	model.Channels[0], model.Channels[1] = model.Channels[1], model.Channels[0]
	slices.Reverse(model.Bindings)
	slices.Reverse(model.Operations)
	slices.Reverse(model.Security)
	slices.Reverse(model.Channels[1].Bindings)
	first, firstReport, err := asyncapi.Export(model)
	if err != nil {
		t.Fatalf("Export: %v; report=%#v", err, firstReport)
	}
	secondModel := fixtureModel(t)
	second, secondReport, err := asyncapi.Export(secondModel)
	if err != nil || !slices.Equal(first, second) || !reflect.DeepEqual(firstReport, secondReport) {
		t.Fatalf("export was not stable: %v\n%s\n%s", err, first, second)
	}
	if firstReport.Status != "ready" || firstReport.ExporterVersion != asyncapi.ExporterVersion {
		t.Fatalf("report = %#v", firstReport)
	}
	var document map[string]any
	if json.Unmarshal(first, &document) != nil || document["asyncapi"] != asyncapi.Version {
		t.Fatalf("document = %s", first)
	}
	assertIdentityLinks(t, document)
}

func TestImportRejectsUnsupportedSemanticsAndRemoteReferences(t *testing.T) {
	t.Parallel()
	exported, _, err := asyncapi.Export(fixtureModel(t))
	if err != nil {
		t.Fatal(err)
	}
	options := asyncapi.ImportOptions{Limits: asyncapi.DefaultLimits(), AllowedServerURLs: []string{"https://events.example/v1"}}
	imported, report, err := asyncapi.Import(exported, options)
	if err != nil || report.Status != "ready" || len(imported.Channels) == 0 {
		t.Fatalf("Import = %#v, %#v, %v", imported, report, err)
	}

	remote := strings.Replace(string(exported), "#/components/messages/Event", "https://attacker.example/event.json", 1)
	_, report, err = asyncapi.Import([]byte(remote), options)
	if errorCode(err) != "ASYNCAPI_REFERENCE_BLOCKED" || report.Status != "rejected" {
		t.Fatalf("remote reference = %#v, %v", report, err)
	}

	unsupported := strings.Replace(string(exported), `"classification":"lossless","feature":"correlation-identifiers"`, `"classification":"unsupported","feature":"correlation-identifiers"`, 1)
	_, report, err = asyncapi.Import([]byte(unsupported), options)
	if errorCode(err) != "ASYNCAPI_REQUIRED_SEMANTIC_UNSUPPORTED" || report.Status != "rejected" || len(report.Diagnostics) == 0 {
		t.Fatalf("unsupported semantic = %#v, %v", report, err)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil || !json.Valid(encoded) {
		t.Fatalf("fidelity report is not machine-readable: %v", marshalErr)
	}
}

func TestServersRequireExplicitEgressAllowlistWithoutNetworkResolution(t *testing.T) {
	t.Parallel()
	exported, _, err := asyncapi.Export(fixtureModel(t))
	if err != nil {
		t.Fatal(err)
	}
	_, report, err := asyncapi.Import(exported, asyncapi.ImportOptions{Limits: asyncapi.DefaultLimits()})
	if errorCode(err) != "ASYNCAPI_SERVER_DENIED" || report.Diagnostics[0].Feature != "network-egress" {
		t.Fatalf("default import = %#v, %v", report, err)
	}
	_, _, err = asyncapi.Import(exported, asyncapi.ImportOptions{
		Limits: asyncapi.DefaultLimits(), AllowedServerURLs: []string{"https://different.example/v1"},
	})
	if errorCode(err) != "ASYNCAPI_SERVER_DENIED" {
		t.Fatalf("wrong allowlist error = %v", err)
	}
}

func TestFixturesPreserveDistinctCorrelationAndStreamSemantics(t *testing.T) {
	t.Parallel()
	model := fixtureModel(t)
	assertStreamFixture(t, model)
	exported, report, err := asyncapi.Export(model)
	if err != nil || report.Status != "ready" {
		t.Fatalf("fixture export = %#v, %v", report, err)
	}
	imported, _, err := asyncapi.Import(exported, asyncapi.ImportOptions{
		Limits: asyncapi.DefaultLimits(), AllowedServerURLs: []string{"https://events.example/v1"},
	})
	if err != nil {
		t.Fatalf("fixture import = %v", err)
	}
	assertImportedStreamFixture(t, imported, model)
}

func assertStreamFixture(t testing.TB, model asyncapi.Model) {
	t.Helper()
	for _, channel := range model.Channels {
		if channel.Semantics.WireCompatibility {
			t.Fatalf("channel %q claimed AsyncAPI wire compatibility", channel.ID)
		}
		if channel.Semantics.Ordering == "" || channel.Semantics.Replay == "" || channel.Semantics.Terminal == "" || channel.Semantics.Errors == "" {
			t.Fatalf("channel %q omits stream semantics: %#v", channel.ID, channel.Semantics)
		}
	}
	message := model.Messages[0]
	want := map[string]string{
		"request-id": "request", "stream-id": "logical-stream", "application-event-id": "business-event", "webhook-delivery-attempt": "delivery-attempt",
	}
	for _, correlation := range message.Correlations {
		if want[correlation.ID] != correlation.Lifetime || correlation.Trust == "" {
			t.Fatalf("correlation conflated or untrusted metadata omitted: %#v", correlation)
		}
		delete(want, correlation.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing correlations: %#v", want)
	}
}

func assertImportedStreamFixture(t testing.TB, imported, original asyncapi.Model) {
	t.Helper()
	wantCorrelations := slices.Clone(original.Messages[0].Correlations)
	slices.SortFunc(wantCorrelations, func(left, right asyncapi.Correlation) int { return strings.Compare(left.ID, right.ID) })
	if !reflect.DeepEqual(imported.Messages[0].Correlations, wantCorrelations) {
		t.Fatalf("correlations changed across export/import: %#v", imported.Messages[0].Correlations)
	}
	for _, channel := range imported.Channels {
		if !reflect.DeepEqual(channel.Semantics, original.Channels[0].Semantics) {
			t.Fatalf("channel semantics changed across export/import: %#v", channel)
		}
	}
}

func TestCompatibilityClassifiesMessageChannelBindingAndSecurityChanges(t *testing.T) {
	t.Parallel()
	base := fixtureModel(t)
	assertChange := func(name string, mutate func(*asyncapi.Model), domain string, want schema.ChangeClassification) {
		t.Helper()
		after := fixtureModel(t)
		mutate(&after)
		diff := asyncapi.Diff(base, after)
		for _, change := range diff.Changes {
			if change.Domain == domain && change.Classification == want {
				return
			}
		}
		t.Fatalf("%s: no %s/%s change in %#v", name, domain, want, diff)
	}
	assertChange("message", func(value *asyncapi.Model) { value.Messages[0].PayloadType = "Other" }, "message", schema.ChangeBreaking)
	assertChange("channel", func(value *asyncapi.Model) { value.Channels[0].Address = "/moved" }, "channel", schema.ChangeBreaking)
	assertChange("binding", func(value *asyncapi.Model) { value.Bindings[0].Revision = "sse-2" }, "binding", schema.ChangeBreaking)
	assertChange("security", func(value *asyncapi.Model) { value.Security[0].Revision = "auth-2" }, "security", schema.ChangeDangerous)
}

func TestViewsUseOneExportedModelAndRedactSecrets(t *testing.T) {
	t.Parallel()
	model := fixtureModel(t)
	model.Messages[0].Examples = []json.RawMessage{json.RawMessage(`{"authorization":"Bearer protected-token","password":"secret-value"}`)}
	model.Messages = append(model.Messages, asyncapi.Message{
		ID: "Protected", NaatreID: "event.protected", Revision: "event-1", PayloadType: "SecretType", Protected: true,
	})
	exported, report, err := asyncapi.Export(model)
	if err != nil {
		t.Fatalf("Export = %#v, %v", report, err)
	}
	for _, secret := range []string{"protected-token", "secret-value", "SecretType", "event.protected"} {
		if strings.Contains(string(exported), secret) {
			t.Fatalf("export leaked %q: %s", secret, exported)
		}
	}
	options := asyncapi.ImportOptions{Limits: asyncapi.DefaultLimits(), AllowedServerURLs: []string{"https://events.example/v1"}}
	imported, _, err := asyncapi.Import(exported, options)
	if err != nil {
		t.Fatal(err)
	}
	inspect := asyncapi.Inspect(imported)
	documentation := asyncapi.Documentation(imported)
	mock := asyncapi.Mock(imported, 13)
	if inspect.ModelDigest == "" || inspect.ModelDigest != documentation.ModelDigest || inspect.ModelDigest != mock.ModelDigest {
		t.Fatalf("views did not consume one model: %#v %#v %#v", inspect, documentation, mock)
	}
	if inspect.BusinessHandlerCalls != 0 || documentation.BusinessHandlerCalls != 0 || mock.BusinessHandlerCalls != 0 {
		t.Fatal("side-effect-free views reported a business handler call")
	}
	unsafe := strings.Replace(string(exported), "<redacted>", "protected-token", 1)
	_, unsafeReport, unsafeErr := asyncapi.Import([]byte(unsafe), options)
	if errorCode(unsafeErr) != "ASYNCAPI_EXAMPLE_CREDENTIAL" || unsafeReport.Status != "rejected" {
		t.Fatalf("credential-bearing import = %#v, %v", unsafeReport, unsafeErr)
	}
	diagnostic, marshalErr := json.Marshal(unsafeReport)
	if marshalErr != nil || strings.Contains(string(diagnostic), "protected-token") || strings.Contains(unsafeErr.Error(), "protected-token") {
		t.Fatalf("credential-bearing diagnostic leaked input: %s, %v", diagnostic, unsafeErr)
	}
}

func fixtureModel(t testing.TB) asyncapi.Model {
	t.Helper()
	document := fixtureSchema(t)
	correlations := []asyncapi.Correlation{
		{ID: "request-id", Location: "$message.header#/x-naatre-request-id", Lifetime: "request", Trust: "transport-untrusted"},
		{ID: "stream-id", Location: "$message.payload#/stream", Lifetime: "logical-stream", Trust: "server-issued"},
		{ID: "application-event-id", Location: "$message.payload#/id", Lifetime: "business-event", Trust: "application-supplied-authenticated"},
		{ID: "webhook-delivery-attempt", Location: "$message.header#/naatre-webhook-id", Lifetime: "delivery-attempt", Trust: "signature-authenticated"},
	}
	semantics := asyncapi.Semantics{Ordering: "naatre-stream-sequence", Replay: "explicit-cursor", Terminal: "complete-or-final-error", Errors: "naatre-stream-error", WireCompatibility: false}
	return asyncapi.Model{
		ID: "urn:naatre:events:profiles", Title: "Profile events", Version: "1.0.0", Schema: document,
		CapabilityRevision: "events-capabilities-1", EventEnvelopeRevision: "core.events-1",
		Servers: []asyncapi.Server{{ID: "events", NaatreID: "server.events", Revision: "server-1", URL: "https://events.example/v1", Protocol: "https"}},
		Bindings: []asyncapi.Binding{
			{ID: "sse", NaatreID: "binding.sse", Revision: "sse-1", Transport: asyncapi.TransportSSE, Server: "events", Implemented: true, Evidence: []string{"transport/http/subscription_test.go"}},
			{ID: "websocket", NaatreID: "binding.websocket", Revision: "websocket-1", Transport: asyncapi.TransportWebSocket, Server: "events", Implemented: true, Evidence: []string{"conformance/independent/typescript-adapter-runtime.mjs"}},
			{ID: "webhook", NaatreID: "binding.webhook", Revision: "webhook-1", Transport: asyncapi.TransportWebhookHTTP, Server: "events", Implemented: true, Evidence: []string{"event/event_test.go"}},
		},
		Messages: []asyncapi.Message{{ID: "Event", NaatreID: "event.profile.changed", Revision: "event-1", PayloadType: "ProfileEvent", ContentType: "application/cloudevents+json", Correlations: correlations}},
		Channels: []asyncapi.Channel{
			{ID: "subscription", NaatreID: "channel.profile.subscription", Revision: "channel-1", Address: "/profiles/stream", Messages: []string{"Event"}, Bindings: []string{"sse", "websocket"}, Semantics: semantics},
			{ID: "webhook", NaatreID: "channel.profile.webhook", Revision: "channel-1", Address: "/hooks/profiles", Messages: []string{"Event"}, Bindings: []string{"webhook"}, Semantics: semantics},
		},
		Operations: []asyncapi.Operation{
			{ID: "receiveProfiles", NaatreID: "subscription.profile", Revision: "operation-1", Action: asyncapi.ActionReceive, Channel: "subscription", Messages: []string{"Event"}, Security: []string{"tenantBearer"}},
			{ID: "sendWebhook", NaatreID: "webhook.profile", Revision: "operation-1", Action: asyncapi.ActionSend, Channel: "webhook", Messages: []string{"Event"}, Security: []string{"webhookSignature"}},
		},
		Security: []asyncapi.SecurityScheme{
			{ID: "tenantBearer", NaatreID: "security.tenant-bearer", Revision: "security-1", Type: "http", Scheme: "bearer"},
			{ID: "webhookSignature", NaatreID: "security.webhook-rfc9421", Revision: "security-1", Type: "httpApiKey", Name: "Signature", In: "header"},
		},
	}
}

func fixtureSchema(t testing.TB) schema.Document {
	t.Helper()
	catalog := schema.NewCatalog()
	if err := catalog.Register(schema.TypeDescriptor{ID: "ProfileEvent", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
		"id": {ID: "ProfileEvent.id", Type: schema.TypeID(schema.ID), Required: true},
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	document, err := schema.ExportDocument(snapshot, []schema.OperationDescriptor{{
		ID: "subscription.profile", Name: "profile", Kind: protocol.Subscription, Output: "ProfileEvent", Effect: "read",
		ThreadSafety: "thread-safe", Batching: "ineligible", Transaction: "none", AuthorizationPolicy: "tenant", Idempotency: "idempotent", Cost: 1,
	}}, nil, schema.ExportOptions{Revision: "profiles-schema-1", Capabilities: []string{"subscription", "events"}})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func assertIdentityLinks(t testing.TB, document map[string]any) {
	t.Helper()
	revisions := document["x-naatre-revisions"].(map[string]any)
	assertSectionIdentityLinks(t, document, revisions, map[string]string{"servers": "server", "channels": "channel", "operations": "operation"})
	components := document["components"].(map[string]any)
	assertSectionIdentityLinks(t, components, revisions, map[string]string{
		"messages": "message", "schemas": "schema", "securitySchemes": "security", "correlationIds": "correlation",
	})
	assertBindingIdentityLinks(t, document["channels"].(map[string]any), revisions)
}

func assertSectionIdentityLinks(t testing.TB, parent, revisions map[string]any, sections map[string]string) {
	t.Helper()
	for section, kind := range sections {
		for name, raw := range parent[section].(map[string]any) {
			assertIdentityFields(t, raw.(map[string]any)["x-naatre-identity"], section+" "+name, kind, revisions)
		}
	}
}

func assertBindingIdentityLinks(t testing.TB, channels, revisions map[string]any) {
	t.Helper()
	for name, raw := range channels {
		bindings := raw.(map[string]any)["bindings"].(map[string]any)
		for bindingName, binding := range bindings {
			assertIdentityFields(t, binding.(map[string]any)["x-naatre-identity"], "channel "+name+" binding "+bindingName, "binding", revisions)
		}
	}
}

func assertIdentityFields(t testing.TB, raw any, declaration, kind string, revisions map[string]any) {
	t.Helper()
	identity, ok := raw.(map[string]any)
	if !ok || identity["kind"] != kind || identity["naatreId"] == "" || identity["revision"] == "" ||
		identity["schemaRevision"] != revisions["schema"] || identity["capabilityRevision"] != revisions["capabilities"] ||
		identity["canonicalizationRevision"] != revisions["canonicalization"] || identity["eventEnvelopeRevision"] != revisions["eventEnvelope"] {
		t.Errorf("%s has an incomplete identity link: %#v", declaration, raw)
	}
}

func errorCode(err error) string {
	var failure interface{ PublicCode() string }
	if errors.As(err, &failure) {
		return failure.PublicCode()
	}
	return ""
}

var _ = interopadapter.Lossless
