package conformance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	transporthttp "github.com/valksor/naatre/transport/http"
)

type streamingFixture struct {
	Profile              string                        `json:"profile"`
	ProtocolVersion      string                        `json:"protocolVersion"`
	LogicalEvents        []string                      `json:"logicalEvents"`
	TerminalOutcomes     []string                      `json:"terminalOutcomes"`
	CursorBindings       []string                      `json:"cursorBindings"`
	ReplayCapabilities   []string                      `json:"replayCapabilities"`
	SafeHistoryOutcome   string                        `json:"safeHistoryOutcome"`
	HandoffInvariant     string                        `json:"handoffInvariant"`
	RepairActions        []string                      `json:"repairActions"`
	Transports           []streamingTransport          `json:"transports"`
	NegotiationCases     []streamingNamedCase          `json:"negotiationCases"`
	AdvertisementCases   []streamingAdvertisementCase  `json:"advertisementCases"`
	FrameCases           []streamingFrameCase          `json:"frameCases"`
	SSECases             []streamingSSECase            `json:"sseCases"`
	ReplayCases          []streamingReplayCase         `json:"replayCases"`
	HistoryFailures      []streamingNamedCase          `json:"historyFailures"`
	LifecycleCases       []streamingLifecycle          `json:"lifecycleCases"`
	HandleBindings       []string                      `json:"handleBindings"`
	BrowserHandleFixture streamingBrowserHandleFixture `json:"browserHandleFixture"`
	HandleHTTP           streamingHandleHTTP           `json:"handleHTTP"`
	BrowserDeliveryPaths []streamingBrowserDelivery    `json:"browserDeliveryPaths"`
	SafeHandleFailures   []streamingHandleFailure      `json:"safeHandleFailures"`
	HandleHandoffCases   []streamingHandleHandoff      `json:"handleHandoffCases"`
	AdapterFixtures      []streamingAdapterFixture     `json:"adapterFixtures"`
	BrokerFailureCases   []streamingBrokerFailure      `json:"brokerFailureCases"`
	OperationalCases     []streamingOperationalCase    `json:"operationalCases"`
}

type streamingBrowserHandleFixture struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Listener    string `json:"listener"`
	Credentials string `json:"credentials"`
}

type streamingHandleHTTP struct {
	EstablishmentCreatedStatus int      `json:"establishmentCreatedStatus"`
	EstablishmentReusedStatus  int      `json:"establishmentReusedStatus"`
	ObservationStatus          int      `json:"observationStatus"`
	RenewalStatus              int      `json:"renewalStatus"`
	CancellationStatus         int      `json:"cancellationStatus"`
	MediaType                  string   `json:"mediaType"`
	DeliveryMediaType          string   `json:"deliveryMediaType"`
	Location                   string   `json:"location"`
	LinkRelations              []string `json:"linkRelations"`
	Fields                     []string `json:"fields"`
	ExposedHeaders             []string `json:"exposedHeaders"`
	CacheControl               string   `json:"cacheControl"`
	ReferrerPolicy             string   `json:"referrerPolicy"`
	Redirects                  string   `json:"redirects"`
}

type streamingBrowserDelivery struct {
	Name              string `json:"name"`
	Method            string `json:"method"`
	CredentialCarrier string `json:"credentialCarrier"`
	CursorCarrier     string `json:"cursorCarrier"`
	Cookies           string `json:"cookies"`
	Query             string `json:"query"`
	CORS              string `json:"cors"`
}

type streamingHandleFailure struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

type streamingHandleHandoff struct {
	Name               string   `json:"name"`
	SnapshotPosition   uint64   `json:"snapshotPosition"`
	CommittedPositions []uint64 `json:"committedPositions"`
	ReplayedPositions  []uint64 `json:"replayedPositions"`
	LivePositions      []uint64 `json:"livePositions"`
	ExpectedPositions  []uint64 `json:"expectedPositions"`
	Duplicates         int      `json:"duplicates"`
	Gap                bool     `json:"gap"`
}

type streamingAdapterFixture struct {
	Name                   string   `json:"name"`
	Frames                 []string `json:"frames"`
	Replay                 string   `json:"replay"`
	PrivateScopedDelivery  bool     `json:"privateScopedDelivery"`
	ReplayAuthorization    bool     `json:"replayAuthorization"`
	HistoryLossDetection   bool     `json:"historyLossDetection"`
	TerminalFrameRetention bool     `json:"terminalFrameRetention"`
	DuplicateSuppression   bool     `json:"duplicateSuppression"`
	BoundedRetry           bool     `json:"boundedRetry"`
	Expected               string   `json:"expected"`
}

type streamingBrokerFailure struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Retry    string `json:"retry"`
}

type streamingOperationalCase struct {
	Name                      string   `json:"name"`
	MaximumConnectionLifetime string   `json:"maximumConnectionLifetime"`
	ReconnectStagger          string   `json:"reconnectStagger"`
	MaximumReconnectAttempts  int      `json:"maximumReconnectAttempts"`
	Expected                  string   `json:"expected"`
	Data                      string   `json:"data"`
	Steps                     []string `json:"steps"`
	Records                   []string `json:"records"`
	ForbiddenRecords          []string `json:"forbiddenRecords"`
}

type streamingTransport struct {
	Name       string `json:"name"`
	Required   bool   `json:"required"`
	Advertised bool   `json:"advertised"`
	Outcome    string `json:"outcome"`
}

type streamingFrameCase struct {
	Name                 string            `json:"name"`
	Frames               []json.RawMessage `json:"frames"`
	Expected             string            `json:"expected"`
	ExpectedIssues       int               `json:"expectedIssues"`
	ExpectedState        json.RawMessage   `json:"expectedState"`
	ExpectedTerminal     string            `json:"expectedTerminal"`
	ExpectedTerminalPath []any             `json:"expectedTerminalPath"`
	ExpectedRecovery     string            `json:"expectedRecovery"`
	TrackedSequences     int               `json:"trackedSequences"`
}

type streamingSSECase struct {
	Name           string            `json:"name"`
	Chunking       string            `json:"chunking"`
	Input          string            `json:"input"`
	Expected       string            `json:"expected"`
	ExpectedFrames []json.RawMessage `json:"expectedFrames"`
}

type streamingReplayCase struct {
	Name       string   `json:"name"`
	History    []uint64 `json:"history"`
	After      uint64   `json:"after"`
	Live       []uint64 `json:"live"`
	Expected   []uint64 `json:"expected"`
	Concurrent bool     `json:"concurrent"`
}

type streamingAdvertisementCase struct {
	Name          string          `json:"name"`
	Advertisement json.RawMessage `json:"advertisement"`
	Expected      string          `json:"expected"`
}

type streamingNamedCase struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
}

type streamingLifecycle struct {
	Name     string `json:"name"`
	Trigger  string `json:"trigger"`
	Expected string `json:"expected"`
}

func TestStreamingFixture(t *testing.T) {
	t.Parallel()
	var fixture streamingFixture
	readFixture(t, "streaming.json", &fixture)
	assertStreamingHeader(t, fixture)
	for _, test := range fixture.NegotiationCases {
		t.Run("negotiation/"+test.Name, func(t *testing.T) { runStreamingNegotiationCase(t, test) })
	}
	for _, test := range fixture.AdvertisementCases {
		t.Run("advertisement/"+test.Name, func(t *testing.T) { runStreamingAdvertisementCase(t, test) })
	}
	for _, test := range fixture.FrameCases {
		t.Run("frame/"+test.Name, func(t *testing.T) { runStreamingFrameCase(t, test) })
	}
	for _, test := range fixture.SSECases {
		t.Run("sse/"+test.Name, func(t *testing.T) { runStreamingSSECase(t, test) })
	}
	for _, test := range fixture.ReplayCases {
		t.Run("replay/"+test.Name, func(t *testing.T) { runStreamingReplayCase(t, test) })
	}
	for _, test := range fixture.HistoryFailures {
		t.Run("history/"+test.Name, func(t *testing.T) { runStreamingHistoryFailure(t, test) })
	}
	for _, test := range fixture.LifecycleCases {
		t.Run("lifecycle/"+test.Name, func(t *testing.T) { runStreamingLifecycle(t, test) })
	}
	assertSubscriptionHandleFixture(t, fixture)
}

func assertSubscriptionHandleFixture(t *testing.T, fixture streamingFixture) {
	t.Helper()
	assertExactStrings(t, "handle bindings", fixture.HandleBindings, []string{"principal", "tenant", "operationIdentity", "variableIdentity", "schemaRevision", "authorizationRevision", "limits", "createdAt", "expiresAt", "deliveryProfile"})
	if fixture.BrowserHandleFixture.Path != "conformance/browser/subscription-handles.html" || fixture.BrowserHandleFixture.Listener != "ephemeral-port" || fixture.BrowserHandleFixture.Credentials != "same-origin-memory-only" || len(fixture.BrowserHandleFixture.SHA256) != 64 {
		t.Fatalf("browser handle fixture = %#v", fixture.BrowserHandleFixture)
	}
	browserFixture, err := os.ReadFile("../../" + fixture.BrowserHandleFixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(browserFixture)
	if hex.EncodeToString(digest[:]) != fixture.BrowserHandleFixture.SHA256 {
		t.Fatalf("browser handle fixture digest = %x, want %s", digest, fixture.BrowserHandleFixture.SHA256)
	}
	http := fixture.HandleHTTP
	if http.EstablishmentCreatedStatus != 201 || http.EstablishmentReusedStatus != 200 || http.ObservationStatus != 200 || http.RenewalStatus != 200 || http.CancellationStatus != 204 ||
		http.MediaType != "application/vnd.naatre.subscription-handle+json;version=1" || http.DeliveryMediaType != "text/event-stream" || http.Location != "handle-resource" || http.CacheControl != "no-store" || http.ReferrerPolicy != "no-referrer" || http.Redirects != "forbidden" {
		t.Fatalf("subscription handle HTTP contract = %#v", http)
	}
	if len(fixture.BrowserDeliveryPaths) != 2 || fixture.BrowserDeliveryPaths[0].Name != "fetch-bearer" || fixture.BrowserDeliveryPaths[0].Query != "empty" || fixture.BrowserDeliveryPaths[1].Name != "same-origin-cookie" || fixture.BrowserDeliveryPaths[1].Query != "empty" {
		t.Fatalf("browser delivery paths = %#v", fixture.BrowserDeliveryPaths)
	}
	if len(fixture.SafeHandleFailures) != 8 {
		t.Fatalf("safe handle failures = %#v", fixture.SafeHandleFailures)
	}
	for index, failure := range fixture.SafeHandleFailures {
		if index < 6 && (failure.Status != 409 || failure.Code != "REESTABLISH_REQUIRED") {
			t.Fatalf("unsafe handle distinction = %#v", failure)
		}
	}
	if len(fixture.HandleHandoffCases) != 1 || fixture.HandleHandoffCases[0].Gap || fixture.HandleHandoffCases[0].Duplicates != 0 || !slices.Equal(fixture.HandleHandoffCases[0].ExpectedPositions, []uint64{2, 3}) {
		t.Fatalf("handle handoff = %#v", fixture.HandleHandoffCases)
	}
	for _, adapter := range fixture.AdapterFixtures {
		frames := make([]protocol.StreamEventType, len(adapter.Frames))
		for index, frame := range adapter.Frames {
			frames[index] = protocol.StreamEventType(frame)
		}
		kind := runtime.SubscriptionAdapterMercure
		if adapter.Name == "in-process" {
			kind = runtime.SubscriptionAdapterInProcess
		}
		report := runtime.ReportSubscriptionBrokerFidelity(runtime.SubscriptionBrokerCapabilities{
			Adapter: kind, Frames: frames, Replay: protocol.StreamReplayCapability(adapter.Replay),
			PrivateScopedDelivery: adapter.PrivateScopedDelivery, ReplayAuthorization: adapter.ReplayAuthorization,
			HistoryLossDetection: adapter.HistoryLossDetection, TerminalFrameRetention: adapter.TerminalFrameRetention,
			DuplicateSuppression: adapter.DuplicateSuppression, BoundedRetry: adapter.BoundedRetry,
		})
		got := "fidelity-failure"
		if report.Compatible {
			got = "compatible"
		}
		if got != adapter.Expected {
			t.Fatalf("adapter %q fidelity = %#v, want %s", adapter.Name, report, adapter.Expected)
		}
	}
	if len(fixture.BrokerFailureCases) != 5 {
		t.Fatalf("broker failures = %#v", fixture.BrokerFailureCases)
	}
	for _, failure := range fixture.BrokerFailureCases {
		if failure.Retry != "bounded" || failure.Expected == "complete" {
			t.Fatalf("untruthful broker failure = %#v", failure)
		}
	}
	if len(fixture.OperationalCases) != 2 || fixture.OperationalCases[0].MaximumReconnectAttempts <= 0 || fixture.OperationalCases[1].Data != "isolated-synthetic" || len(fixture.OperationalCases[1].ForbiddenRecords) != 5 {
		t.Fatalf("operational handle cases = %#v", fixture.OperationalCases)
	}
}

func assertStreamingHeader(t *testing.T, fixture streamingFixture) {
	t.Helper()
	if fixture.Profile != "core.streaming-1" || fixture.ProtocolVersion != "1" || fixture.SafeHistoryOutcome != "history-unavailable" ||
		fixture.HandoffInvariant != "lock-atomic-snapshot-registration" {
		t.Fatalf("streaming fixture header = %#v", fixture)
	}
	assertExactStrings(t, "logical events", fixture.LogicalEvents, []string{"open", "data", "patch", "error", "complete", "keepalive", "resume", "history-unavailable"})
	assertExactStrings(t, "terminal outcomes", fixture.TerminalOutcomes, []string{"complete", "error"})
	assertExactStrings(t, "cursor bindings", fixture.CursorBindings, []string{"stream", "tenant", "principal", "authorizationRevision", "schemaRevision", "keyId", "version", "expiry"})
	assertExactStrings(t, "replay capabilities", fixture.ReplayCapabilities, []string{"none", "bounded", "durable"})
	assertExactStrings(t, "repair actions", fixture.RepairActions, []string{"restart", "refetch"})
	if len(fixture.Transports) != 2 || fixture.Transports[0] != (streamingTransport{Name: "sse-post-fetch", Required: true, Outcome: "adapter-required"}) ||
		fixture.Transports[1] != (streamingTransport{Name: "websocket", Outcome: "unsupported"}) {
		t.Fatalf("streaming transport capabilities = %#v", fixture.Transports)
	}
	if len(fixture.NegotiationCases) != 2 || fixture.NegotiationCases[0].Name != "1" || fixture.NegotiationCases[1].Name != "2" {
		t.Fatalf("streaming negotiation cases = %#v", fixture.NegotiationCases)
	}
	if len(fixture.AdvertisementCases) != 3 || fixture.AdvertisementCases[0].Name != "no-replay-live" || fixture.AdvertisementCases[1].Name != "bounded-snapshot" {
		t.Fatalf("streaming advertisement cases = %#v", fixture.AdvertisementCases)
	}
	for _, replay := range fixture.ReplayCases {
		if !replay.Concurrent {
			t.Fatalf("replay case %q is not concurrent", replay.Name)
		}
	}
}

func runStreamingFrameCase(t *testing.T, test streamingFrameCase) {
	t.Helper()
	limits := protocol.DefaultStreamLimits()
	if test.TrackedSequences > 0 {
		limits.MaxTrackedSequences = test.TrackedSequences
	}
	receiver, err := protocol.NewStreamReceiver("s", limits)
	if err != nil {
		t.Fatal(err)
	}
	got := "ok"
	for _, raw := range test.Frames {
		frame, err := protocol.DecodeStreamFrame(raw, limits)
		if err != nil {
			got = streamingErrorName(err)
			break
		}
		if _, err := receiver.Accept(frame); err != nil {
			got = streamingErrorName(err)
			break
		}
	}
	if got == "ok" {
		got = streamingErrorName(receiver.Finish())
	}
	if got != test.Expected || len(receiver.Errors()) != test.ExpectedIssues {
		t.Fatalf("frame case outcome/issues = %s/%d, want %s/%d", got, len(receiver.Errors()), test.Expected, test.ExpectedIssues)
	}
	if len(test.ExpectedState) != 0 && !equalStreamingJSON(receiver.Snapshot(), test.ExpectedState) {
		t.Fatalf("frame state = %s, want %s", receiver.Snapshot(), test.ExpectedState)
	}
	if test.ExpectedTerminal != "" {
		outcome, ok := receiver.Terminal()
		if !ok || string(outcome.Type) != test.ExpectedTerminal {
			t.Fatalf("terminal outcome = %#v, %t", outcome, ok)
		}
		if !reflect.DeepEqual(outcome.Path, test.ExpectedTerminalPath) {
			t.Fatalf("terminal path = %#v, want %#v", outcome.Path, test.ExpectedTerminalPath)
		}
	}
	if test.ExpectedRecovery != "" {
		outcome, ok := receiver.Recovery()
		if !ok || string(outcome.Recovery) != test.ExpectedRecovery {
			t.Fatalf("recovery outcome = %#v, %t", outcome, ok)
		}
	}
}

func runStreamingNegotiationCase(t *testing.T, test streamingNamedCase) {
	t.Helper()
	err := protocol.NegotiateStreamProfile(test.Name)
	got := "ok"
	if errors.Is(err, protocol.ErrUnsupportedStreamVersion) {
		got = "unsupported-version"
	}
	if got != test.Expected {
		t.Fatalf("negotiation outcome = %s (%v), want %s", got, err, test.Expected)
	}
}

func runStreamingAdvertisementCase(t *testing.T, test streamingAdvertisementCase) {
	t.Helper()
	_, err := protocol.DecodeStreamSourceAdvertisement(test.Advertisement, protocol.DefaultStreamLimits())
	got := "ok"
	if err != nil {
		got = "invalid-advertisement"
	}
	if got != test.Expected {
		t.Fatalf("advertisement outcome = %s (%v), want %s", got, err, test.Expected)
	}
}

func runStreamingSSECase(t *testing.T, test streamingSSECase) {
	t.Helper()
	decoder := transporthttp.NewSSEDecoder(transporthttp.DefaultSSELimits())
	var frames []protocol.StreamFrame
	var err error
	if test.Chunking == "octets" {
		for _, octet := range []byte(test.Input) {
			var decoded []protocol.StreamFrame
			decoded, err = decoder.Feed([]byte{octet})
			frames = append(frames, decoded...)
			if err != nil {
				break
			}
		}
	} else {
		frames, err = decoder.Feed([]byte(test.Input))
	}
	if err == nil {
		err = decoder.Finish()
	}
	got := "ok"
	if err != nil {
		got = "invalid-sse"
	}
	if got != test.Expected {
		t.Fatalf("SSE outcome = %s (%v), want %s", got, err, test.Expected)
	}
	if got != "ok" {
		if len(test.ExpectedFrames) != 0 {
			t.Fatalf("invalid SSE case declares %d expected frames", len(test.ExpectedFrames))
		}
		return
	}
	want := make([]protocol.StreamFrame, len(test.ExpectedFrames))
	for index, raw := range test.ExpectedFrames {
		want[index], err = protocol.DecodeStreamFrame(raw, protocol.DefaultStreamLimits())
		if err != nil {
			t.Fatalf("expected SSE frame %d: %v", index, err)
		}
	}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("SSE frames = %#v, want %#v", frames, want)
	}
}

func runStreamingReplayCase(t *testing.T, test streamingReplayCase) {
	t.Helper()
	for _, concurrent := range []bool{false, true} {
		attempt := test
		attempt.Concurrent = concurrent
		got := runStreamingReplayAttempt(t, attempt)
		if !slices.Equal(got, test.Expected) {
			t.Fatalf("replay positions concurrent=%t = %v, want %v", concurrent, got, test.Expected)
		}
	}
}

func runStreamingReplayAttempt(t *testing.T, test streamingReplayCase) []uint64 {
	t.Helper()
	store, err := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 16, MaxHistoryBytes: 8192, SubscriberQueue: 8})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := runtime.NewStreamCursorScope(runtime.StreamCursorScopeOptions{
		Stream: "s", Tenant: "tenant", Principal: "principal", AuthorizationRevision: "auth-r1", SchemaRevision: "schema-r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, position := range test.History {
		if err := store.Publish(scope, conformanceDataFrame(position)); err != nil {
			t.Fatal(err)
		}
	}
	options := conformanceReplayOptions(runtime.StreamReplayLimits{MaxEvents: 16, MaxBytes: 8192})
	var subscription *runtime.StreamReplaySubscription
	var subscribeErr error
	if test.Concurrent {
		registered := make(chan struct{})
		release := make(chan struct{})
		options.ReauthorizeSession = func(ctx context.Context) error {
			close(registered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		subscribed := make(chan struct{})
		go func() {
			defer close(subscribed)
			subscription, subscribeErr = subscribeConformance(t, store, scope, test.After, options)
		}()
		<-registered
		for _, position := range test.Live {
			if err := store.Publish(scope, conformanceDataFrame(position)); err != nil {
				close(release)
				t.Fatal(err)
			}
		}
		close(release)
		<-subscribed
	} else {
		subscription, subscribeErr = subscribeConformance(t, store, scope, test.After, options)
		for _, position := range test.Live {
			if err := store.Publish(scope, conformanceDataFrame(position)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if subscribeErr != nil {
		t.Fatal(subscribeErr)
	}
	defer subscription.Close()
	if test.After != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resume, nextErr := subscription.Next(ctx)
		cancel()
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if resume.Type != protocol.StreamResume || resume.Cursor == "" {
			t.Fatalf("resume frame = %#v", resume)
		}
	}
	got := make([]uint64, len(test.Expected))
	for index := range got {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		frame, err := subscription.Next(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		got[index] = frame.Position
	}
	return got
}

func runStreamingHistoryFailure(t *testing.T, test streamingNamedCase) {
	t.Helper()
	if test.Expected != "history-unavailable" {
		t.Fatalf("unsafe history outcome %q", test.Expected)
	}
	var err error
	switch test.Name {
	case "unknown-cursor", "unauthorized-cursor", "expired-cursor", "revoked-cursor":
		now := time.Unix(1_800_000_000, 0)
		codec, codecErr := runtime.NewStreamCursorCodec(runtime.StreamCursorCodecConfig{
			ActiveKeyID: "fixture", Keys: map[string][]byte{"fixture": []byte("fixture-key-material-is-32-bytes-long")},
			TTL: time.Minute, Now: func() time.Time { return now },
		})
		if codecErr != nil {
			t.Fatal(codecErr)
		}
		scope, scopeErr := runtime.NewStreamCursorScope(runtime.StreamCursorScopeOptions{
			Stream: "s", Tenant: "tenant", Principal: "principal", AuthorizationRevision: "auth-r1", SchemaRevision: "schema-r1",
		})
		if scopeErr != nil {
			t.Fatal(scopeErr)
		}
		cursor := "unknown"
		if test.Name != "unknown-cursor" {
			cursor, err = codec.Encode(scope, 1)
			if err != nil {
				t.Fatal(err)
			}
		}
		if test.Name == "unauthorized-cursor" {
			scope, scopeErr = runtime.NewStreamCursorScope(runtime.StreamCursorScopeOptions{
				Stream: "s", Tenant: "tenant", Principal: "other-principal", AuthorizationRevision: "auth-r1", SchemaRevision: "schema-r1",
			})
			if scopeErr != nil {
				t.Fatal(scopeErr)
			}
		}
		if test.Name == "expired-cursor" {
			now = now.Add(time.Minute)
		}
		store, storeErr := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: 4, MaxHistoryBytes: 8192, SubscriberQueue: 1})
		if storeErr != nil {
			t.Fatal(storeErr)
		}
		if test.Name != "unauthorized-cursor" {
			if publishErr := store.Publish(scope, conformanceDataFrame(1)); publishErr != nil {
				t.Fatal(publishErr)
			}
		}
		options := conformanceReplayOptions(runtime.StreamReplayLimits{MaxEvents: 8, MaxBytes: 8192})
		options.CursorCodec = codec
		if test.Name == "revoked-cursor" {
			options.ReauthorizeSession = func(context.Context) error { return runtime.ErrStreamAuthorizationRevoked }
		}
		_, err = store.Subscribe(context.Background(), scope, cursor, options)
	case "evicted-cursor", "over-budget-cursor":
		maximum := 2
		published := uint64(4)
		if test.Name == "over-budget-cursor" {
			maximum = 8
			published = 3
		}
		store, storeErr := runtime.NewStreamReplayBuffer(runtime.StreamReplayConfig{MaxHistoryEvents: maximum, MaxHistoryBytes: 8192, SubscriberQueue: 1})
		if storeErr != nil {
			t.Fatal(storeErr)
		}
		scope, scopeErr := runtime.NewStreamCursorScope(runtime.StreamCursorScopeOptions{
			Stream: "s", Tenant: "tenant", Principal: "principal", AuthorizationRevision: "auth-r1", SchemaRevision: "schema-r1",
		})
		if scopeErr != nil {
			t.Fatal(scopeErr)
		}
		for position := uint64(1); position <= published; position++ {
			if err := store.Publish(scope, conformanceDataFrame(position)); err != nil {
				t.Fatal(err)
			}
		}
		limits := runtime.StreamReplayLimits{MaxEvents: 8, MaxBytes: 8192}
		if test.Name == "over-budget-cursor" {
			limits.MaxEvents = 1
		}
		_, err = subscribeConformance(t, store, scope, 1, conformanceReplayOptions(limits))
	default:
		t.Fatalf("unknown history case %q", test.Name)
	}
	if !errors.Is(err, runtime.ErrStreamHistoryUnavailable) {
		t.Fatalf("history error = %v", err)
	}
}

func runStreamingLifecycle(t *testing.T, test streamingLifecycle) {
	t.Helper()
	source := &conformanceStreamSource{}
	executor, err := runtime.NewStreamCheckExecutor(4)
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.StreamSourceSessionConfig{
		Stream: "s", ProfileVersion: protocol.StreamProfileVersion, SchemaRevision: "schema-r1", Source: source,
		Advertisement: protocol.StreamSourceAdvertisement{
			ProfileVersion: protocol.StreamProfileVersion, Replay: protocol.StreamReplayNone,
			Consistency: protocol.StreamLiveBestEffort, RetentionPolicy: "none", HistoryRecovery: protocol.StreamRecoveryRestart,
		},
		Reauthorize:             func(context.Context, protocol.StreamFrame) error { return nil },
		ValidateSchema:          func(context.Context, string) error { return nil },
		CheckExecutor:           executor,
		AuthenticationExpiresAt: time.Now().Add(time.Hour),
	}
	var want error
	switch test.Trigger {
	case "cancel":
		source.block = true
	case "close":
	case "eof-before-terminal":
		source.frames = []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}}
		want = protocol.ErrStreamTruncated
	case "authentication-expired":
		source.frames = []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}}
		source.block = true
		config.AuthenticationExpiresAt = time.Now().Add(20 * time.Millisecond)
		want = runtime.ErrStreamAuthenticationExpired
	case "authorization-revoked":
		source.frames = []protocol.StreamFrame{
			{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
			{Type: protocol.StreamResume, Stream: "s", Sequence: 2, Cursor: "opaque-cursor"},
		}
		config.Reauthorize = func(context.Context, protocol.StreamFrame) error { return runtime.ErrStreamAuthorizationRevoked }
		want = runtime.ErrStreamAuthorizationRevoked
	case "schema-retired":
		source.frames = conformanceOpenDataFrames()
		checks := 0
		config.ValidateSchema = func(context.Context, string) error {
			checks++
			if checks > 1 {
				return runtime.ErrStreamSchemaRetired
			}
			return nil
		}
		want = runtime.ErrStreamSchemaRetired
	default:
		t.Fatalf("unknown lifecycle trigger %q", test.Trigger)
	}
	session, err := runtime.NewStreamSourceSession(config)
	if err != nil {
		t.Fatal(err)
	}
	switch test.Trigger {
	case "close":
		err = session.Close()
	case "cancel":
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = session.Next(ctx)
	case "eof-before-terminal":
		_, err = session.Next(context.Background())
		if err == nil {
			_, err = session.Next(context.Background())
		}
	default:
		_, err = session.Next(context.Background())
		if err == nil {
			_, err = session.Next(context.Background())
		}
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("lifecycle error = %v, want %v", err, want)
	}
	if test.Expected == "source-closed" && err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle close error = %v", err)
	}
	if source.closed.Load() != 1 {
		t.Fatalf("source close count = %d", source.closed.Load())
	}
}

func streamingErrorName(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return "ok"
	case errors.Is(err, protocol.ErrInvalidStreamFrame):
		return "invalid-frame"
	case errors.Is(err, protocol.ErrStreamDuplicateConflict):
		return "duplicate-conflict"
	case errors.Is(err, protocol.ErrStreamDuplicateExpired):
		return "duplicate-expired"
	case errors.Is(err, protocol.ErrStreamSequenceGap):
		return "sequence-gap"
	case errors.Is(err, protocol.ErrStreamPositionOrder):
		return "position-order"
	case errors.Is(err, protocol.ErrStreamPatchOrder):
		return "patch-order"
	case errors.Is(err, protocol.ErrStreamTruncated):
		return "truncated"
	default:
		return err.Error()
	}
}

func equalStreamingJSON(left, right []byte) bool {
	leftCanonical, leftErr := protocol.CanonicalizeJSON(left, protocol.DefaultLimits())
	rightCanonical, rightErr := protocol.CanonicalizeJSON(right, protocol.DefaultLimits())
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func conformanceDataFrame(position uint64) protocol.StreamFrame {
	return protocol.StreamFrame{
		Type: protocol.StreamData, Stream: "s", Sequence: position + 1,
		Position: position, HasPosition: true, Data: []byte(`true`),
	}
}

func conformanceReplayOptions(limits runtime.StreamReplayLimits) runtime.StreamReplaySubscriptionOptions {
	executor, _ := runtime.NewStreamCheckExecutor(4)
	return runtime.StreamReplaySubscriptionOptions{
		Limits: limits, FirstSequence: 3, ProfileVersion: protocol.StreamProfileVersion,
		ReauthorizeSession:      func(context.Context) error { return nil },
		Reauthorize:             func(context.Context, protocol.StreamFrame) error { return nil },
		ValidateSchema:          func(context.Context, string) error { return nil },
		CheckExecutor:           executor,
		AuthenticationExpiresAt: time.Now().Add(time.Hour),
	}
}

func subscribeConformance(t testing.TB, store *runtime.StreamReplayBuffer, scope runtime.StreamCursorScope, after uint64, options runtime.StreamReplaySubscriptionOptions) (*runtime.StreamReplaySubscription, error) {
	t.Helper()
	cursor := ""
	if after != 0 {
		codec, err := runtime.NewStreamCursorCodec(runtime.StreamCursorCodecConfig{
			ActiveKeyID: "fixture", Keys: map[string][]byte{"fixture": []byte("fixture-key-material-is-32-bytes-long")}, TTL: time.Hour,
		})
		if err != nil {
			t.Fatalf("cursor codec: %v", err)
		}
		cursor, err = codec.Encode(scope, after)
		if err != nil {
			t.Fatalf("cursor encode: %v", err)
		}
		options.CursorCodec = codec
	}
	return store.Subscribe(context.Background(), scope, cursor, options)
}

func conformanceOpenDataFrames() []protocol.StreamFrame {
	return []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: []byte(`true`)},
	}
}

type conformanceStreamSource struct {
	frames []protocol.StreamFrame
	index  int
	block  bool
	closed atomic.Int32
}

func (s *conformanceStreamSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if s.index < len(s.frames) {
		frame := s.frames[s.index]
		s.index++
		return frame, nil
	}
	if !s.block {
		return protocol.StreamFrame{}, io.EOF
	}
	<-ctx.Done()
	return protocol.StreamFrame{}, context.Cause(ctx)
}

func (s *conformanceStreamSource) Close() error {
	s.closed.Add(1)
	return nil
}
