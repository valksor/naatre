package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/event"
)

var (
	testSecretA = []byte("0123456789abcdef0123456789abcdef")
	testSecretB = []byte("fedcba9876543210fedcba9876543210")
)

func TestSQLiteOutboxCommitDuplicateRestartAndFence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "webhook.db")
	store := openTestStore(t, path, &now)
	if _, err := store.db.Exec(`CREATE TABLE business_state (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(t, "wh_commit", 42)

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO business_state (id) VALUES ('rolled-back')`); err != nil {
		t.Fatal(err)
	}
	if created, err := store.EnqueueTx(context.Background(), tx, delivery); err != nil || !created {
		t.Fatalf("rolled-back enqueue = %v, %v", created, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), delivery.Record.DeliveryID); ErrorCode(err) != CodeInvalidRecord {
		t.Fatalf("rolled-back delivery load = %v", err)
	}

	tx, err = store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO business_state (id) VALUES ('committed')`); err != nil {
		t.Fatal(err)
	}
	if created, err := store.EnqueueTx(context.Background(), tx, delivery); err != nil || !created {
		t.Fatalf("committed enqueue = %v, %v", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, &now)
	if created, err := store.Enqueue(context.Background(), delivery); err != nil || created {
		t.Fatalf("idempotent enqueue = %v, %v", created, err)
	}
	conflict := delivery
	conflict.Record.PayloadReference = "outbox:other"
	if _, err := store.Enqueue(context.Background(), conflict); ErrorCode(err) != CodeInvalidRecord {
		t.Fatalf("conflicting duplicate = %v", err)
	}
	claimed, found, err := store.Claim(context.Background(), "worker-a")
	if err != nil || !found || claimed.Record.Attempt != 1 || claimed.Fence != 1 {
		t.Fatalf("claim = %#v, %v, %v", claimed, found, err)
	}
	now = now.Add(31 * time.Second)
	if recovered, err := store.RecoverExpired(context.Background(), 1); err != nil || recovered != 1 {
		t.Fatalf("recover = %d, %v", recovered, err)
	}
	reclaimed, found, err := store.Claim(context.Background(), "worker-b")
	if err != nil || !found || reclaimed.Record.Attempt != 2 || reclaimed.Fence != 2 {
		t.Fatalf("reclaim = %#v, %v, %v", reclaimed, found, err)
	}
	if err := store.Succeed(context.Background(), claimed.Record.DeliveryID, claimed.Fence, http.StatusNoContent); ErrorCode(err) != CodeFenceRejected {
		t.Fatalf("stale fence completion = %v", err)
	}
	if err := store.Succeed(context.Background(), reclaimed.Record.DeliveryID, reclaimed.Fence, http.StatusNoContent); err != nil {
		t.Fatal(err)
	}
	finished, err := store.Load(context.Background(), delivery.Record.DeliveryID)
	if err != nil || finished.Record.State != event.DeliverySucceeded || finished.Record.Attempt != 2 {
		t.Fatalf("finished delivery = %#v, %v", finished, err)
	}
}

func TestDispatcherDurableOutcomes(t *testing.T) {
	t.Parallel()
	t.Run("rotation-success", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
		store := openTestStore(t, filepath.Join(t.TempDir(), "success.db"), &now)
		delivery := testDelivery(t, "wh_rotation", 42)
		mustEnqueue(t, store, delivery)
		connector := &fixtureConnector{statuses: []int{http.StatusNoContent}, verifyKey: testSecretB, keyID: "key-b", now: &now}
		dispatcher := testDispatcher(t, store, &now, connector, staticResolver{testAddress()}, activeEndpoint(), "key-b", testSecretB)
		observations, err := dispatcher.RunOnce(context.Background())
		if err != nil || len(observations) != 1 || observations[0].Outcome != string(event.DeliverySucceeded) || connector.calls != 1 {
			t.Fatalf("rotation delivery = %#v, calls=%d, err=%v", observations, connector.calls, err)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
		store := openTestStore(t, filepath.Join(t.TempDir(), "redirect.db"), &now)
		mustEnqueue(t, store, testDelivery(t, "wh_redirect", 42))
		dispatcher := testDispatcher(t, store, &now, &fixtureConnector{statuses: []int{http.StatusTemporaryRedirect}}, staticResolver{testAddress()}, activeEndpoint(), "key-a", testSecretA)
		if _, err := dispatcher.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertState(t, store, "wh_redirect", event.DeliveryDeadLettered, CodeRedirect, 1)
		letters, err := store.DeadLetters(context.Background(), 1)
		if err != nil || len(letters) != 1 || strings.Contains(mustJSON(t, letters), "outbox:") || strings.Contains(mustJSON(t, letters), "hooks.example") {
			t.Fatalf("safe dead letters = %#v, %v", letters, err)
		}
	})

	t.Run("dns-rebinding", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
		store := openTestStore(t, filepath.Join(t.TempDir(), "rebind.db"), &now)
		mustEnqueue(t, store, testDelivery(t, "wh_rebind", 42))
		connector := &fixtureConnector{}
		dispatcher := testDispatcher(t, store, &now, connector, staticResolver{netip.MustParseAddr("1.1.1.1")}, activeEndpoint(), "key-a", testSecretA)
		if _, err := dispatcher.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertState(t, store, "wh_rebind", event.DeliveryDeadLettered, CodeDNSRebinding, 1)
		if connector.calls != 0 {
			t.Fatal("connector ran after DNS rebinding")
		}
	})

	t.Run("revoked-endpoint", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
		store := openTestStore(t, filepath.Join(t.TempDir(), "revoked.db"), &now)
		mustEnqueue(t, store, testDelivery(t, "wh_revoked", 42))
		endpoint := activeEndpoint()
		endpoint.Status = event.EndpointRevoked
		dispatcher := testDispatcher(t, store, &now, &fixtureConnector{}, staticResolver{testAddress()}, endpoint, "key-a", testSecretA)
		if _, err := dispatcher.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertState(t, store, "wh_revoked", event.DeliveryDeadLettered, CodeEndpointRevoked, 1)
	})

	t.Run("exhausted-retry", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
		store := openTestStore(t, filepath.Join(t.TempDir(), "retry.db"), &now)
		mustEnqueue(t, store, testDelivery(t, "wh_retry", 42))
		connector := &fixtureConnector{statuses: []int{503, 503, 503, 503, 503, 503}}
		resolver := &countingResolver{addresses: []netip.Addr{testAddress()}}
		dispatcher := testDispatcher(t, store, &now, connector, resolver, activeEndpoint(), "key-a", testSecretA)
		wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
		for attempt := 0; attempt < 6; attempt++ {
			if _, err := dispatcher.RunOnce(context.Background()); err != nil {
				t.Fatalf("attempt %d: %v", attempt+1, err)
			}
			current, err := store.Load(context.Background(), "wh_retry")
			if err != nil {
				t.Fatal(err)
			}
			if attempt < len(wantDelays) {
				if got := current.NextAttempt.Sub(now); got != wantDelays[attempt] {
					t.Fatalf("attempt %d delay = %s, want %s", attempt+1, got, wantDelays[attempt])
				}
				now = current.NextAttempt
			}
		}
		assertState(t, store, "wh_retry", event.DeliveryDeadLettered, CodeRetryExhausted, 6)
		if resolver.Calls() != 6 {
			t.Fatalf("DNS resolution calls = %d, want one per attempt", resolver.Calls())
		}
	})
}

func TestReceiverDurableDuplicateReorderTamperStaleAndRotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "receiver.db")
	store := openTestStore(t, path, &now)
	keys := map[string]event.VerificationKey{
		"key-a": verificationKey("key-a", testSecretA),
		"key-b": verificationKey("key-b", testSecretB),
	}
	receiver := testReceiver(t, store, &now, keys)
	if replay, err := store.CheckAndStore("expiry-audience", "expiry-id", now, now.Add(time.Second)); err != nil || replay {
		t.Fatalf("fresh replay receipt = %v, %v", replay, err)
	}
	if replay, err := store.CheckAndStore("expiry-audience", "expiry-id", now, now.Add(time.Second)); err != nil || !replay {
		t.Fatalf("duplicate replay receipt = %v, %v", replay, err)
	}
	if replay, err := store.CheckAndStore("expiry-audience", "expiry-id", now.Add(time.Second), now.Add(2*time.Second)); err != nil || replay {
		t.Fatalf("expired replay receipt = %v, %v", replay, err)
	}
	if err := store.recordSequences(context.Background(), []sequenceValue{{Stream: "zero-stream", Sequence: 0}}); err != nil {
		t.Fatalf("first zero sequence = %v", err)
	}
	first := signedRequest(t, "wh_receive_1", 42, "key-a", testSecretA, now)
	result, err := receiver.VerifyRequest(context.Background(), first)
	if err != nil || result.Outcome != ReceiveAccepted || len(result.Events) != 1 || *result.Events[0].Sequence != 42 {
		t.Fatalf("first receive = %#v, %v", result, err)
	}
	duplicate := signedRequest(t, "wh_receive_1", 42, "key-a", testSecretA, now)
	result, err = receiver.VerifyRequest(context.Background(), duplicate)
	if err != nil || result.Outcome != ReceiveDuplicate || len(result.Events) != 0 {
		t.Fatalf("duplicate receive = %#v, %v", result, err)
	}

	rotated := signedRequest(t, "wh_receive_2", 43, "key-b", testSecretB, now)
	result, err = receiver.VerifyRequest(context.Background(), rotated)
	if err != nil || result.Verification.KeyID != "key-b" {
		t.Fatalf("rotated receive = %#v, %v", result, err)
	}
	reordered := signedRequest(t, "wh_receive_3", 42, "key-b", testSecretB, now)
	if _, err := receiver.VerifyRequest(context.Background(), reordered); ErrorCode(err) != CodeEventReordered {
		t.Fatalf("reordered receive = %v", err)
	}

	tampered := signedRequest(t, "wh_receive_4", 44, "key-a", testSecretA, now)
	tampered.Body = io.NopCloser(strings.NewReader(strings.Replace(readRequestBody(t, tampered), `"naatresequence":44`, `"naatresequence":45`, 1)))
	if _, err := receiver.VerifyRequest(context.Background(), tampered); ErrorCode(err) != CodeSignatureInvalid {
		t.Fatalf("tampered receive = %v", err)
	}
	stale := signedRequest(t, "wh_receive_5", 44, "key-a", testSecretA, now.Add(-10*time.Minute))
	if _, err := receiver.VerifyRequest(context.Background(), stale); ErrorCode(err) != CodeStale {
		t.Fatalf("stale receive = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path, &now)
	receiver = testReceiver(t, store, &now, keys)
	afterRestart := signedRequest(t, "wh_receive_6", 43, "key-a", testSecretA, now)
	if _, err := receiver.VerifyRequest(context.Background(), afterRestart); ErrorCode(err) != CodeEventReordered {
		t.Fatalf("durable sequence after restart = %v", err)
	}
	duplicateAfterRestart := signedRequest(t, "wh_receive_2", 43, "key-b", testSecretB, now)
	if result, err := receiver.VerifyRequest(context.Background(), duplicateAfterRestart); err != nil || result.Outcome != ReceiveDuplicate {
		t.Fatalf("durable duplicate after restart = %#v, %v", result, err)
	}
}

func TestCancellationLimitsAndPublicFailureRedaction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "limits.db"), &now)
	delivery := testDelivery(t, "wh_cancel", 42)
	mustEnqueue(t, store, delivery)
	ctx, cancel := context.WithCancel(context.Background())
	connector := &fixtureConnector{cancel: cancel}
	dispatcher := testDispatcher(t, store, &now, connector, staticResolver{testAddress()}, activeEndpoint(), "key-a", testSecretA)
	if _, err := dispatcher.RunOnce(ctx); ErrorCode(err) != CodeCancelled {
		t.Fatalf("cancelled dispatcher = %v", err)
	}
	assertState(t, store, "wh_cancel", event.DeliveryPending, CodeCancelled, 1)

	oversize := testDelivery(t, "wh_large", 42)
	oversize.Body = append(oversize.Body, make([]byte, DefaultLimits().MaxPayloadBytes)...)
	if _, err := store.Enqueue(context.Background(), oversize); ErrorCode(err) != CodeResourceExhausted && ErrorCode(err) != CodeInvalidRecord {
		t.Fatalf("oversize enqueue = %v", err)
	}
	for _, err := range []error{
		publicError(CodeStoreUnavailable, "webhook storage is unavailable", errors.New("sql secret-token hooks.example outbox:42 10.0.0.1")),
		publicError(CodeSignatureInvalid, "webhook request authentication failed", errors.New("signature secret-token")),
	} {
		for _, forbidden := range []string{"secret-token", "hooks.example", "outbox:42", "10.0.0.1", "sql"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Errorf("public failure exposed %q: %v", forbidden, err)
			}
		}
	}
	limiter, err := NewMemoryLimiter(1, len(delivery.Body))
	if err != nil {
		t.Fatal(err)
	}
	release, err := limiter.Acquire(context.Background(), "tenant-1", "endpoint_orders_1", len(delivery.Body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Acquire(context.Background(), "tenant-1", "endpoint_orders_1", 1); ErrorCode(err) != CodeResourceExhausted {
		t.Fatalf("concurrency limit = %v", err)
	}
	release()
}

type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return slices.Clone(r), nil
}

type countingResolver struct {
	mu        sync.Mutex
	addresses []netip.Addr
	calls     int
}

func (r *countingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return slices.Clone(r.addresses), nil
}

func (r *countingResolver) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type fixtureConnector struct {
	mu        sync.Mutex
	statuses  []int
	calls     int
	verifyKey []byte
	keyID     string
	now       *time.Time
	cancel    context.CancelFunc
}

func (c *fixtureConnector) Deliver(ctx context.Context, endpoint event.EndpointRegistration, message event.Message, address netip.Addr, _ int64) (AttemptResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if address != testAddress() || endpoint.URL != message.TargetURI {
		return AttemptResponse{}, publicError(CodeDNSRebinding, "webhook endpoint resolution changed", nil)
	}
	if c.cancel != nil {
		c.cancel()
		return AttemptResponse{}, publicError(CodeCancelled, "webhook attempt was cancelled", ctx.Err())
	}
	if len(c.verifyKey) != 0 {
		verifier, err := event.NewVerifier(event.VerifierConfig{
			ResolveKey: func(id string) (event.VerificationKey, bool) {
				return event.VerificationKey{ID: c.keyID, Sender: "urn:naatre:sender:orders", Audience: endpoint.ID, Secret: c.verifyKey,
					NotBefore: c.now.Add(-time.Hour), NotAfter: c.now.Add(time.Hour)}, id == c.keyID
			},
			ReplayStore: &event.MemoryReplayStore{}, Now: func() time.Time { return *c.now }, MaximumValidity: event.MaximumSignatureValidity,
		})
		if err != nil {
			return AttemptResponse{}, err
		}
		if _, err := verifier.Verify(message); err != nil {
			return AttemptResponse{}, err
		}
	}
	status := http.StatusNoContent
	if len(c.statuses) >= c.calls {
		status = c.statuses[c.calls-1]
	}
	return AttemptResponse{StatusCode: status}, nil
}

func openTestStore(t *testing.T, path string, now *time.Time) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(context.Background(), path, StoreConfig{
		Now: func() time.Time { return *now }, LeaseDuration: 30 * time.Second, Retention: time.Hour,
		Retry:  event.RetryPolicy{MaximumAttempts: 6, InitialDelay: time.Second, MaximumDelay: 8 * time.Second, Multiplier: 2},
		Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testAddress() netip.Addr { return netip.MustParseAddr("93.184.216.34") }

func activeEndpoint() event.EndpointRegistration {
	return event.EndpointRegistration{
		ID: "endpoint_orders_1", Tenant: "tenant-1", Revision: "endpoint-r1", URL: "https://hooks.example.com/v1/events",
		Status: event.EndpointActive, Ordering: event.OrderingStream, AllowedEventTypes: []string{"com.valksor.order.created.v1"},
		ResolvedAddresses: []netip.Addr{testAddress()},
	}
}

func testDelivery(t *testing.T, id string, sequence uint64) Delivery {
	t.Helper()
	body := testEnvelopeBody(t, sequence)
	return Delivery{
		Record: event.DeliveryRecord{
			Tenant: "tenant-1", EndpointID: "endpoint_orders_1", EndpointRevision: "endpoint-r1", DeliveryID: id,
			EventIDs: []string{"evt_order_123"}, PayloadReference: "outbox:42", State: event.DeliveryPending,
		},
		EventTypes: []string{"com.valksor.order.created.v1"}, ContentType: event.JSONContentType,
		ContentEncoding: event.IdentityEncoding, Body: body,
	}
}

func testEnvelopeBody(t *testing.T, sequence uint64) []byte {
	t.Helper()
	envelope := event.Envelope{
		ID: "evt_order_123", Source: "urn:naatre:orders", Type: "com.valksor.order.created.v1",
		Time: time.Date(2026, 9, 15, 11, 59, 59, 0, time.UTC), DataSchema: "https://schemas.example.com/orders/v1",
		SchemaRevision: "orders-r1", SchemaDigest: "sha256:" + strings.Repeat("a", 64), Data: json.RawMessage(`{"orderId":"123"}`),
		OrderingKey: "customer-7", Sequence: &sequence,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustEnqueue(t *testing.T, store *SQLiteStore, delivery Delivery) {
	t.Helper()
	if created, err := store.Enqueue(context.Background(), delivery); err != nil || !created {
		t.Fatalf("enqueue = %v, %v", created, err)
	}
}

func testDispatcher(t *testing.T, store *SQLiteStore, now *time.Time, connector Connector, resolver Resolver, endpoint event.EndpointRegistration, keyID string, secret []byte) Dispatcher {
	t.Helper()
	limiter, err := NewMemoryLimiter(1, DefaultLimits().MaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	return Dispatcher{
		Store: store, Endpoints: EndpointSourceFunc(func(context.Context, string, string) (event.EndpointRegistration, bool) { return endpoint, true }),
		Keys: KeySourceFunc(func(context.Context, event.EndpointRegistration, time.Time) (SigningGeneration, bool) {
			return SigningGeneration{Key: event.SigningKey{ID: keyID, Secret: secret}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}, true
		}),
		Resolver: resolver, Connector: connector, Limiter: limiter, WorkerID: "worker-1", MaxBatch: 1,
		Now: func() time.Time { return *now }, Validity: event.MaximumSignatureValidity,
	}
}

func assertState(t *testing.T, store *SQLiteStore, id string, state event.DeliveryState, code string, attempt uint32) {
	t.Helper()
	delivery, err := store.Load(context.Background(), id)
	if err != nil || delivery.Record.State != state || delivery.Record.FailureCode != code || delivery.Record.Attempt != attempt {
		t.Fatalf("delivery state = %#v, %v; want %s %s attempt %d", delivery, err, state, code, attempt)
	}
}

func verificationKey(id string, secret []byte) event.VerificationKey {
	return event.VerificationKey{
		ID: id, Sender: "urn:naatre:sender:orders", Audience: "endpoint_orders_1", Secret: secret,
		NotBefore: time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC), NotAfter: time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC),
	}
}

func testReceiver(t *testing.T, store *SQLiteStore, now *time.Time, keys map[string]event.VerificationKey) *Receiver {
	t.Helper()
	verifier, err := event.NewVerifier(event.VerifierConfig{
		ResolveKey:  func(id string) (event.VerificationKey, bool) { key, ok := keys[id]; return key, ok },
		ReplayStore: store, Now: func() time.Time { return *now }, MaximumValidity: event.MaximumSignatureValidity,
		MaximumBodyBytes: event.DefaultMaximumBodyBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(ReceiverConfig{
		Verifier: verifier, Store: store, TargetURI: activeEndpoint().URL,
		MaximumBodyBytes: event.DefaultMaximumBodyBytes, MaximumEvents: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receiver
}

func signedRequest(t *testing.T, deliveryID string, sequence uint64, keyID string, secret []byte, created time.Time) *http.Request {
	t.Helper()
	message, err := event.Sign(event.Message{
		Method: http.MethodPost, TargetURI: activeEndpoint().URL, ContentType: event.JSONContentType,
		ContentEncoding: event.IdentityEncoding, DeliveryID: deliveryID, Audience: activeEndpoint().ID,
		Body: testEnvelopeBody(t, sequence),
	}, event.SigningKey{ID: keyID, Secret: secret}, created, created.Add(event.MaximumSignatureValidity))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, message.TargetURI, strings.NewReader(string(message.Body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", message.ContentType)
	request.Header.Set("Content-Encoding", message.ContentEncoding)
	request.Header.Set("Content-Digest", message.ContentDigest)
	request.Header.Set("Naatre-Webhook-Id", message.DeliveryID)
	request.Header.Set("Naatre-Webhook-Timestamp", message.Timestamp)
	request.Header.Set("Naatre-Webhook-Audience", message.Audience)
	request.Header.Set("Signature-Input", message.SignatureInput)
	request.Header.Set("Signature", message.Signature)
	return request
}

func readRequestBody(t *testing.T, request *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
