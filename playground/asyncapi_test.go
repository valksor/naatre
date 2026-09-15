package playground_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/valksor/naatre/asyncapi"
	"github.com/valksor/naatre/playground"
)

func TestPlaygroundConsumesAuthorizedAsyncAPIModelWithoutHandlers(t *testing.T) {
	t.Parallel()
	model := asyncapi.Model{
		ID: "urn:naatre:playground:events", Title: "Playground events", Version: "1.0.0", Schema: testSchema(t),
		CapabilityRevision: "playground-events-1", EventEnvelopeRevision: "core.events-1",
		Servers:    []asyncapi.Server{{ID: "events", NaatreID: "server.events", Revision: "server-1", URL: "https://api.example/events", Protocol: "https"}},
		Bindings:   []asyncapi.Binding{{ID: "webhook", NaatreID: "binding.webhook", Revision: "webhook-1", Transport: asyncapi.TransportWebhookHTTP, Server: "events", Implemented: true, Evidence: []string{"playground-offline-test"}}},
		Messages:   []asyncapi.Message{{ID: "AccountEvent", NaatreID: "event.account", Revision: "event-1", PayloadType: "User", ContentType: "application/cloudevents+json", Correlations: []asyncapi.Correlation{{ID: "event-id", Location: "$message.payload#/id", Lifetime: "business-event", Trust: "authenticated"}}}},
		Channels:   []asyncapi.Channel{{ID: "accounts", NaatreID: "channel.accounts", Revision: "channel-1", Address: "/accounts", Messages: []string{"AccountEvent"}, Bindings: []string{"webhook"}, Semantics: asyncapi.Semantics{Ordering: "event-stream", Replay: "delivery-lineage", Terminal: "delivery-outcome", Errors: "safe-code"}}},
		Operations: []asyncapi.Operation{{ID: "sendAccounts", NaatreID: "operation.accounts", Revision: "operation-1", Action: asyncapi.ActionSend, Channel: "accounts", Messages: []string{"AccountEvent"}, Security: []string{"signature"}}},
		Security:   []asyncapi.SecurityScheme{{ID: "signature", NaatreID: "security.signature", Revision: "security-1", Type: "httpApiKey", Name: "Signature", In: "header"}},
	}
	exported, _, err := asyncapi.Export(model)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := playground.NewHandler(playground.Config{
		Schema: testSchema(t), AsyncAPIDocument: exported, AllowedAsyncAPIServers: []string{"https://api.example/events"}, Limits: playground.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serve(t, handler, http.MethodGet, "/v1/asyncapi", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"businessHandlerCalls":0`) || !strings.Contains(response.Body.String(), `"channels":["accounts"]`) {
		t.Fatalf("AsyncAPI playground view = %d %s", response.Code, response.Body.String())
	}
}
