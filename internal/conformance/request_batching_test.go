package conformance_test

import (
	"slices"
	"testing"
)

type requestBatchFixture struct {
	Profile         string                      `json:"profile"`
	Envelope        requestBatchEnvelope        `json:"envelope"`
	Limits          requestBatchLimits          `json:"limits"`
	EnvelopeCases   []requestBatchCase          `json:"envelopeCases"`
	ItemCases       []requestBatchCase          `json:"itemCases"`
	AtomicCases     []requestBatchCase          `json:"atomicCases"`
	ScopeCases      []requestBatchScopeCase     `json:"scopeCases"`
	TransportCases  []requestBatchTransportCase `json:"transportCases"`
	ResponseCases   []requestBatchResponseCase  `json:"responseCases"`
	HTTPStatusCases []httpStatusCase            `json:"httpStatusCases"`
}

type requestBatchEnvelope struct {
	RequestFields             []string `json:"requestFields"`
	RequiredRequestFields     []string `json:"requiredRequestFields"`
	ItemFields                []string `json:"itemFields"`
	ResponseFields            []string `json:"responseFields"`
	ResponseItemFields        []string `json:"responseItemFields"`
	ResponseVersion           string   `json:"responseVersion"`
	ResponseItemPayload       string   `json:"responseItemPayload"`
	FailFastTriggerStatuses   []string `json:"failFastTriggerStatuses"`
	ResponseStatuses          []string `json:"responseStatuses"`
	RequiredItemID            bool     `json:"requiredItemId"`
	UniqueItemIDs             bool     `json:"uniqueItemIds"`
	DefaultPolicy             string   `json:"defaultPolicy"`
	Policies                  []string `json:"policies"`
	NestedBatches             bool     `json:"nestedBatches"`
	Notifications             bool     `json:"notifications"`
	CrossItemReferences       bool     `json:"crossItemReferences"`
	ResponseCorrelation       string   `json:"responseCorrelation"`
	NonStreamingResponseOrder string   `json:"nonStreamingResponseOrder"`
}

type requestBatchLimits struct {
	Finite                        bool     `json:"finite"`
	Dimensions                    []string `json:"dimensions"`
	PerItemLimitsAlsoApply        bool     `json:"perItemLimitsAlsoApply"`
	SharedEndToEndBudget          bool     `json:"sharedEndToEndBudget"`
	AtomicReservationBeforeWrites bool     `json:"atomicReservationBeforeWrites"`
}

type requestBatchCase struct {
	Name                   string   `json:"name"`
	Policy                 string   `json:"policy"`
	EffectivePolicy        string   `json:"effectivePolicy"`
	Kinds                  []string `json:"kinds"`
	Providers              []string `json:"providers"`
	ItemIDs                []string `json:"itemIds"`
	CompletionOrder        []string `json:"completionOrder"`
	ResponseOrder          []string `json:"responseOrder"`
	Statuses               []string `json:"statuses"`
	Effects                []string `json:"effects"`
	Codes                  []string `json:"codes"`
	EnvelopeAccepted       bool     `json:"envelopeAccepted"`
	HandlersStarted        int      `json:"handlersStarted"`
	Code                   string   `json:"code"`
	Correlated             bool     `json:"correlated"`
	UnrelatedContinues     bool     `json:"unrelatedContinues"`
	RunningMayComplete     bool     `json:"runningMayComplete"`
	CrossItemAtomic        bool     `json:"crossItemAtomic"`
	ItemScopedCapabilities bool     `json:"itemScopedCapabilities"`
	SharedAuthentication   bool     `json:"sharedAuthentication"`
	SharedTenant           bool     `json:"sharedTenant"`
	PreflightComplete      bool     `json:"preflightComplete"`
	OneTransaction         bool     `json:"oneTransaction"`
	RootFailureItemID      string   `json:"rootFailureItemId"`
	MaximumActiveItems     int      `json:"maximumActiveItems"`
	MaximumObservedActive  int      `json:"maximumObservedActive"`
}

type requestBatchScopeCase struct {
	Name           string `json:"name"`
	Policy         string `json:"policy"`
	Authentication string `json:"authentication"`
	Tenant         string `json:"tenant"`
	SchemaRevision string `json:"schemaRevision"`
	Capabilities   string `json:"capabilities"`
	Extensions     string `json:"extensions"`
	Idempotency    string `json:"idempotency"`
	Deadline       string `json:"deadline"`
}

type requestBatchTransportCase struct {
	Name                    string   `json:"name"`
	Path                    string   `json:"path"`
	Method                  string   `json:"method"`
	Transport               string   `json:"transport"`
	RequestMediaType        string   `json:"requestMediaType"`
	ResponseMediaType       string   `json:"responseMediaType"`
	ItemIDs                 []string `json:"itemIds"`
	RetainedItemIDs         []string `json:"retainedItemIds"`
	CompletionOrder         []string `json:"completionOrder"`
	Statuses                []string `json:"statuses"`
	Effects                 []string `json:"effects"`
	Code                    string   `json:"code"`
	Accepted                bool     `json:"accepted"`
	Streaming               bool     `json:"streaming"`
	Correlated              bool     `json:"correlated"`
	CarrierAccepted         bool     `json:"carrierAccepted"`
	MissingOutcomesUnknown  bool     `json:"missingOutcomesUnknown"`
	AutomaticRetry          bool     `json:"automaticRetry"`
	SeparateProfileRequired bool     `json:"separateProfileRequired"`
	AtomicConnectionSharing bool     `json:"atomicConnectionSharing"`
}

type requestBatchResponseCase struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Code        string `json:"code"`
	HasResponse bool   `json:"hasResponse"`
	HasProblem  bool   `json:"hasProblem"`
	Accepted    bool   `json:"accepted"`
}

type requestBatchNamed interface {
	requestBatchName() string
}

func (test requestBatchCase) requestBatchName() string {
	return test.Name
}

func (test requestBatchTransportCase) requestBatchName() string {
	return test.Name
}

func TestRequestBatchingFixture(t *testing.T) {
	t.Parallel()
	var fixture requestBatchFixture
	readFixture(t, "request-batching.json", &fixture)
	if fixture.Profile != "core.transport-batch-1" {
		t.Fatalf("transport batch profile = %q", fixture.Profile)
	}
	t.Run("envelope", func(t *testing.T) {
		assertRequestBatchEnvelope(t, fixture.Envelope, fixture.Limits)
		assertRequestBatchEnvelopeCases(t, fixture.EnvelopeCases)
	})
	t.Run("items", func(t *testing.T) {
		assertRequestBatchItemCases(t, fixture.ItemCases)
	})
	t.Run("atomic", func(t *testing.T) {
		assertRequestBatchAtomicCases(t, fixture.AtomicCases)
	})
	t.Run("scopes", func(t *testing.T) {
		if !slices.Equal(fixture.ScopeCases, requestBatchScopeCases) {
			t.Fatalf("batch scope inventory = %#v, want %#v", fixture.ScopeCases, requestBatchScopeCases)
		}
	})
	t.Run("transport", func(t *testing.T) {
		assertRequestBatchTransport(t, fixture.TransportCases)
		if !slices.Equal(fixture.ResponseCases, requestBatchResponseCases) {
			t.Fatalf("batch response cases = %#v, want %#v", fixture.ResponseCases, requestBatchResponseCases)
		}
		if !slices.Equal(fixture.HTTPStatusCases, requestBatchHTTPStatusCases) {
			t.Fatalf("HTTP batch status mapping = %#v, want %#v", fixture.HTTPStatusCases, requestBatchHTTPStatusCases)
		}
	})
}

func assertRequestBatchEnvelope(t *testing.T, envelope requestBatchEnvelope, limits requestBatchLimits) {
	t.Helper()
	if !slices.Equal(envelope.RequestFields, []string{"version", "policy", "items"}) ||
		!slices.Equal(envelope.RequiredRequestFields, []string{"version", "items"}) ||
		!slices.Equal(envelope.ItemFields, []string{"id", "request"}) ||
		!slices.Equal(envelope.ResponseFields, []string{"version", "items"}) ||
		!slices.Equal(envelope.ResponseItemFields, []string{"id", "status", "response", "problem"}) ||
		envelope.ResponseVersion != "1" || envelope.ResponseItemPayload != "exactly-one-of-response-or-problem" ||
		!slices.Equal(envelope.FailFastTriggerStatuses, []string{"rejected", "failed", "cancelled", "timed-out", "rate-limited", "indeterminate"}) ||
		!slices.Equal(envelope.ResponseStatuses, []string{"succeeded", "rejected", "failed", "cancelled", "timed-out", "rate-limited", "not-started", "rolled-back", "indeterminate"}) ||
		!envelope.RequiredItemID || !envelope.UniqueItemIDs || envelope.DefaultPolicy != "independent" ||
		!slices.Equal(envelope.Policies, []string{"independent", "fail-fast", "atomic"}) ||
		envelope.NestedBatches || envelope.Notifications || envelope.CrossItemReferences ||
		envelope.ResponseCorrelation != "item-id" || envelope.NonStreamingResponseOrder != "request" {
		t.Fatalf("unsafe transport batch envelope: %#v", envelope)
	}
	wantDimensions := []string{"itemCount", "compressedBytes", "decompressedBytes", "decodedTokens", "decodedNodes", "plannedCost", "responseBytes", "activeItems"}
	if !limits.Finite || !slices.Equal(limits.Dimensions, wantDimensions) || !limits.PerItemLimitsAlsoApply ||
		!limits.SharedEndToEndBudget || !limits.AtomicReservationBeforeWrites {
		t.Fatalf("incomplete aggregate transport batch limits: %#v", limits)
	}
}

func assertRequestBatchEnvelopeCases(t *testing.T, cases []requestBatchCase) {
	t.Helper()
	want := []string{"malformed-envelope", "wrong-version", "empty-items", "unknown-outer-member", "unknown-item-member", "omitted-policy-defaults-independent", "unsupported-policy", "missing-item-id", "empty-item-id", "non-string-item-id", "overlong-item-id", "duplicate-item-ids", "aggregate-item-limit", "aggregate-byte-limit", "aggregate-decoded-structure-limit", "aggregate-cost-limit", "nested-batch"}
	assertFixtureCases(t, cases, want, func(test requestBatchCase) string { return test.Name }, func(test requestBatchCase) {
		if test.Name == "omitted-policy-defaults-independent" {
			if !test.EnvelopeAccepted || test.EffectivePolicy != "independent" || test.HandlersStarted != 1 || !test.Correlated {
				t.Fatalf("omitted policy did not select independent mode: %#v", test)
			}
			return
		}
		if test.EnvelopeAccepted || test.HandlersStarted != 0 || test.Code == "" {
			t.Fatalf("envelope rejection is not deterministic: %#v", test)
		}
	})
	for _, name := range []string{"missing-item-id", "empty-item-id", "non-string-item-id", "overlong-item-id", "duplicate-item-ids"} {
		if test := requestBatchNamedCase(t, cases, name); test.Code != "INVALID_BATCH_ITEM_ID" {
			t.Fatalf("invalid item ID did not use stable failure: %#v", test)
		}
	}
}

func assertRequestBatchItemCases(t *testing.T, cases []requestBatchCase) {
	t.Helper()
	want := []string{"malformed-item-independent", "aggregate-response-allocation-exhausted-after-admission", "nested-request-id-mismatch", "out-of-order-internal-completion", "aggregate-concurrency-queues", "independent-item-cancellation", "independent-item-timeout", "independent-item-rate-limit", "fail-fast-stops-not-started", "fail-fast-cancel-stops-not-started", "fail-fast-rejection-stops-not-started", "fail-fast-timeout-stops-not-started", "fail-fast-rate-limit-stops-not-started", "fail-fast-indeterminate-stops-not-started", "mixed-query-mutation-independent", "subscription-item-rejected", "mixed-capabilities-independent", "untrusted-context-extension"}
	assertFixtureCases(t, cases, want, func(test requestBatchCase) string { return test.Name }, func(requestBatchCase) {})
	t.Run("completion", func(t *testing.T) {
		assertRequestBatchItemCompletion(t, cases)
	})
	t.Run("cancellation", func(t *testing.T) {
		assertRequestBatchItemCancellation(t, cases)
	})
	t.Run("kinds", func(t *testing.T) {
		assertRequestBatchItemKinds(t, cases)
	})
}

func assertRequestBatchItemCompletion(t *testing.T, cases []requestBatchCase) {
	t.Helper()
	malformed := requestBatchNamedCase(t, cases, "malformed-item-independent")
	if !malformed.EnvelopeAccepted || malformed.HandlersStarted != 1 || !slices.Equal(malformed.Statuses, []string{"rejected", "succeeded"}) {
		t.Fatalf("malformed item did not preserve valid independent work: %#v", malformed)
	}
	exhausted := requestBatchNamedCase(t, cases, "aggregate-response-allocation-exhausted-after-admission")
	if !exhausted.EnvelopeAccepted || exhausted.HandlersStarted != 1 || !exhausted.Correlated ||
		!slices.Equal(exhausted.ItemIDs, []string{"admitted", "exhausted"}) ||
		!slices.Equal(exhausted.Statuses, []string{"succeeded", "rejected"}) ||
		!slices.Equal(exhausted.Codes, []string{"OK", "RESOURCE_EXHAUSTED"}) {
		t.Fatalf("post-admission exhaustion lost correlated outcome: %#v", exhausted)
	}
	outOfOrder := requestBatchNamedCase(t, cases, "out-of-order-internal-completion")
	if !outOfOrder.Correlated || slices.Equal(outOfOrder.CompletionOrder, outOfOrder.ResponseOrder) || !slices.Equal(outOfOrder.ResponseOrder, outOfOrder.ItemIDs) {
		t.Fatalf("out-of-order completion lost non-streaming correlation: %#v", outOfOrder)
	}
	concurrency := requestBatchNamedCase(t, cases, "aggregate-concurrency-queues")
	if concurrency.MaximumActiveItems < 1 || concurrency.MaximumObservedActive > concurrency.MaximumActiveItems {
		t.Fatalf("aggregate item concurrency was exceeded: %#v", concurrency)
	}
}

func assertRequestBatchItemCancellation(t *testing.T, cases []requestBatchCase) {
	t.Helper()
	isolated := map[string][]string{
		"independent-item-cancellation": {"cancelled", "succeeded"},
		"independent-item-timeout":      {"timed-out", "succeeded"},
		"independent-item-rate-limit":   {"rate-limited", "succeeded"},
	}
	for name, statuses := range isolated {
		test := requestBatchNamedCase(t, cases, name)
		if !test.Correlated || !test.UnrelatedContinues || !slices.Equal(test.Statuses, statuses) {
			t.Fatalf("isolated item failure cancelled unrelated work: %#v", test)
		}
	}
	failFast := requestBatchNamedCase(t, cases, "fail-fast-stops-not-started")
	if !failFast.RunningMayComplete || failFast.HandlersStarted != 2 || !slices.Contains(failFast.Statuses, "not-started") {
		t.Fatalf("fail-fast lifecycle is ambiguous: %#v", failFast)
	}
	failFastCancel := requestBatchNamedCase(t, cases, "fail-fast-cancel-stops-not-started")
	if failFastCancel.HandlersStarted != 1 || !slices.Equal(failFastCancel.Statuses, []string{"cancelled", "not-started"}) {
		t.Fatalf("fail-fast cancellation admitted queued work: %#v", failFastCancel)
	}
	failFastCases := map[string]struct {
		statuses []string
		handlers int
	}{
		"fail-fast-rejection-stops-not-started":     {[]string{"rejected", "not-started"}, 0},
		"fail-fast-timeout-stops-not-started":       {[]string{"timed-out", "not-started"}, 1},
		"fail-fast-rate-limit-stops-not-started":    {[]string{"rate-limited", "not-started"}, 0},
		"fail-fast-indeterminate-stops-not-started": {[]string{"indeterminate", "not-started"}, 1},
	}
	for name, want := range failFastCases {
		test := requestBatchNamedCase(t, cases, name)
		if !test.EnvelopeAccepted || !test.Correlated || test.HandlersStarted != want.handlers || !slices.Equal(test.Statuses, want.statuses) {
			t.Fatalf("fail-fast trigger admitted queued work: %#v", test)
		}
	}
}

func assertRequestBatchItemKinds(t *testing.T, cases []requestBatchCase) {
	t.Helper()
	mixed := requestBatchNamedCase(t, cases, "mixed-query-mutation-independent")
	if mixed.CrossItemAtomic || !slices.Equal(mixed.Kinds, []string{"query", "mutation"}) {
		t.Fatalf("independent mixed-kind semantics are unsafe: %#v", mixed)
	}
	capabilities := requestBatchNamedCase(t, cases, "mixed-capabilities-independent")
	forged := requestBatchNamedCase(t, cases, "untrusted-context-extension")
	if !capabilities.ItemScopedCapabilities || !capabilities.SharedAuthentication || !capabilities.SharedTenant ||
		!forged.SharedAuthentication || !forged.SharedTenant || forged.HandlersStarted != 1 || !forged.Correlated ||
		!forged.EnvelopeAccepted || !slices.Equal(forged.ItemIDs, []string{"forged", "safe"}) ||
		!slices.Equal(forged.Statuses, []string{"rejected", "succeeded"}) {
		t.Fatalf("item extensions can vary trusted context: capabilities=%#v forged=%#v", capabilities, forged)
	}
}

func assertRequestBatchAtomicCases(t *testing.T, cases []requestBatchCase) {
	t.Helper()
	want := []string{"atomic-single-provider-commit", "atomic-preflight-invalid-item", "atomic-mixed-provider-rejected", "atomic-mixed-kind-rejected", "atomic-rollback", "atomic-outcome-indeterminate"}
	assertFixtureCases(t, cases, want, func(test requestBatchCase) string { return test.Name }, func(requestBatchCase) {})
	commit := requestBatchNamedCase(t, cases, "atomic-single-provider-commit")
	if !commit.OneTransaction || !commit.PreflightComplete || !commit.EnvelopeAccepted || commit.HandlersStarted != 2 ||
		commit.Policy != "atomic" || !slices.Equal(commit.Kinds, []string{"mutation", "mutation"}) ||
		!slices.Equal(commit.Providers, []string{"primary", "primary"}) ||
		!slices.Equal(commit.Statuses, []string{"succeeded", "succeeded"}) || !slices.Equal(commit.Effects, []string{"applied", "applied"}) {
		t.Fatalf("atomic commit does not use one preflighted transaction: %#v", commit)
	}
	for _, name := range []string{"atomic-preflight-invalid-item", "atomic-mixed-provider-rejected", "atomic-mixed-kind-rejected"} {
		test := requestBatchNamedCase(t, cases, name)
		want := map[string]struct {
			kinds     []string
			providers []string
			code      string
		}{
			"atomic-preflight-invalid-item":  {[]string{"mutation", "mutation"}, []string{"primary", "primary"}, "ATOMIC_BATCH_PREFLIGHT_FAILED"},
			"atomic-mixed-provider-rejected": {[]string{"mutation", "mutation"}, []string{"primary", "secondary"}, "ATOMIC_BATCH_UNSUPPORTED"},
			"atomic-mixed-kind-rejected":     {[]string{"query", "mutation"}, []string{"primary", "primary"}, "ATOMIC_BATCH_UNSUPPORTED"},
		}[name]
		if test.Policy != "atomic" || test.EnvelopeAccepted || !test.PreflightComplete || test.HandlersStarted != 0 ||
			test.Code != want.code || !slices.Equal(test.Kinds, want.kinds) || !slices.Equal(test.Providers, want.providers) {
			t.Fatalf("atomic preflight started work: %#v", test)
		}
	}
	rollback := requestBatchNamedCase(t, cases, "atomic-rollback")
	if !rollback.OneTransaction || !rollback.PreflightComplete || !rollback.EnvelopeAccepted || rollback.HandlersStarted != 2 ||
		rollback.Policy != "atomic" || rollback.RootFailureItemID != "second" ||
		!slices.Equal(rollback.ItemIDs, []string{"first", "second", "third"}) ||
		!slices.Equal(rollback.Kinds, []string{"mutation", "mutation", "mutation"}) ||
		!slices.Equal(rollback.Providers, []string{"primary", "primary", "primary"}) ||
		!slices.Equal(rollback.Statuses, []string{"rolled-back", "failed", "not-started"}) ||
		!slices.Equal(rollback.Effects, []string{"rolled-back", "rolled-back", "none"}) {
		t.Fatalf("atomic rollback is not batch-wide: %#v", rollback)
	}
	rootIndex := slices.Index(rollback.ItemIDs, rollback.RootFailureItemID)
	if rootIndex < 0 || rollback.Statuses[rootIndex] != "failed" {
		t.Fatalf("atomic root failure is not correlated: %#v", rollback)
	}
	indeterminate := requestBatchNamedCase(t, cases, "atomic-outcome-indeterminate")
	if !indeterminate.OneTransaction || !indeterminate.PreflightComplete || !indeterminate.EnvelopeAccepted || indeterminate.HandlersStarted != 2 ||
		indeterminate.Policy != "atomic" || indeterminate.RootFailureItemID != "" ||
		!slices.Equal(indeterminate.ItemIDs, []string{"first", "second", "third"}) ||
		!slices.Equal(indeterminate.Kinds, []string{"mutation", "mutation", "mutation"}) ||
		!slices.Equal(indeterminate.Providers, []string{"primary", "primary", "primary"}) ||
		!slices.Equal(indeterminate.Statuses, []string{"indeterminate", "indeterminate", "not-started"}) ||
		!slices.Equal(indeterminate.Effects, []string{"indeterminate", "indeterminate", "none"}) {
		t.Fatalf("unknown atomic outcome rewrites started or unstarted truth: %#v", indeterminate)
	}
}

var requestBatchScopeCases = []requestBatchScopeCase{
	{Name: "independent-item-scopes", Policy: "independent", Authentication: "batch", Tenant: "batch", SchemaRevision: "item", Capabilities: "item", Extensions: "item", Idempotency: "item", Deadline: "item-capped-by-batch"},
	{Name: "atomic-shared-scopes", Policy: "atomic", Authentication: "batch", Tenant: "batch", SchemaRevision: "batch", Capabilities: "compatible-item-set", Extensions: "compatible-item-set", Idempotency: "batch-exclusive", Deadline: "item-capped-by-batch"},
	{Name: "untrusted-item-context", Policy: "independent", Authentication: "batch-immutable", Tenant: "batch-immutable", SchemaRevision: "item", Capabilities: "item", Extensions: "cannot-override-context", Idempotency: "item", Deadline: "item-capped-by-batch"},
}

func assertRequestBatchTransport(t *testing.T, cases []requestBatchTransportCase) {
	t.Helper()
	want := []string{"http-batch", "http-get-rejected", "partial-item-failure", "response-truncation", "response-loss-after-commit", "partial-transport-failure", "streaming-out-of-order-requires-profile", "websocket-sharing-not-atomic", "notification-rejected", "cross-item-reference-rejected"}
	assertFixtureCases(t, cases, want, func(test requestBatchTransportCase) string { return test.Name }, func(requestBatchTransportCase) {})
	assertRequestBatchHTTPTransport(t, cases)
	assertRequestBatchResponseLoss(t, cases)
	assertRequestBatchMultiplexing(t, cases)
}

func assertRequestBatchHTTPTransport(t *testing.T, cases []requestBatchTransportCase) {
	t.Helper()
	httpBatch := requestBatchNamedCase(t, cases, "http-batch")
	if httpBatch.Path != "/v1/batch" || httpBatch.Method != "POST" ||
		httpBatch.RequestMediaType != "application/vnd.naatre.batch+json;version=1" ||
		httpBatch.ResponseMediaType != "application/vnd.naatre.batch-response+json;version=1" ||
		!httpBatch.Accepted || httpBatch.Streaming || httpBatch.AtomicConnectionSharing {
		t.Fatalf("HTTP batch binding is incomplete: %#v", httpBatch)
	}
	httpGet := requestBatchNamedCase(t, cases, "http-get-rejected")
	if httpGet.Path != "/v1/batch" || httpGet.Method != "GET" || httpGet.Accepted ||
		httpGet.Code != "METHOD_NOT_ALLOWED" || httpGet.Streaming || httpGet.AtomicConnectionSharing {
		t.Fatalf("HTTP GET batch was not deterministically rejected: %#v", httpGet)
	}
	partial := requestBatchNamedCase(t, cases, "partial-item-failure")
	if !partial.CarrierAccepted || !partial.Correlated || !slices.Equal(partial.Statuses, []string{"failed", "succeeded"}) {
		t.Fatalf("partial failure is not item-scoped: %#v", partial)
	}
}

func assertRequestBatchResponseLoss(t *testing.T, cases []requestBatchTransportCase) {
	t.Helper()
	truncated := requestBatchNamedCase(t, cases, "response-truncation")
	lost := requestBatchNamedCase(t, cases, "response-loss-after-commit")
	partialTransport := requestBatchNamedCase(t, cases, "partial-transport-failure")
	if !truncated.MissingOutcomesUnknown || truncated.Code != "TRUNCATED_RESPONSE" ||
		!slices.Equal(truncated.ItemIDs, []string{"complete", "partial", "missing"}) || !slices.Equal(truncated.RetainedItemIDs, []string{"complete"}) ||
		!lost.MissingOutcomesUnknown || lost.AutomaticRetry || lost.Code != "TRANSPORT_TERMINATED" ||
		!slices.Equal(lost.ItemIDs, []string{"committed", "unreceived"}) || !slices.Equal(lost.Effects, []string{"applied", "indeterminate"}) ||
		!partialTransport.MissingOutcomesUnknown || partialTransport.AutomaticRetry || partialTransport.Code != "TRANSPORT_TERMINATED" ||
		!slices.Equal(partialTransport.ItemIDs, []string{"delivered", "lost"}) ||
		!slices.Equal(partialTransport.RetainedItemIDs, []string{"delivered"}) ||
		!slices.Equal(partialTransport.Effects, []string{"applied", "indeterminate"}) {
		t.Fatalf("response loss rewrites execution truth: truncated=%#v lost=%#v partial=%#v", truncated, lost, partialTransport)
	}
}

func assertRequestBatchMultiplexing(t *testing.T, cases []requestBatchTransportCase) {
	t.Helper()
	for _, name := range []string{"streaming-out-of-order-requires-profile", "websocket-sharing-not-atomic"} {
		test := requestBatchNamedCase(t, cases, name)
		if !test.SeparateProfileRequired || test.AtomicConnectionSharing || !test.Correlated {
			t.Fatalf("stream multiplexing implies batching or atomicity: %#v", test)
		}
	}
	for _, name := range []string{"notification-rejected", "cross-item-reference-rejected"} {
		test := requestBatchNamedCase(t, cases, name)
		if test.Accepted || test.Code == "" {
			t.Fatalf("unsupported v1 item accepted: %#v", test)
		}
	}
}

var requestBatchResponseCases = []requestBatchResponseCase{
	{Name: "response-success", Version: "1", HasResponse: true, Accepted: true},
	{Name: "response-problem", Version: "1", HasProblem: true, Accepted: true},
	{Name: "response-both-rejected", Version: "1", HasResponse: true, HasProblem: true, Code: "INVALID_BATCH_RESPONSE_ITEM"},
	{Name: "response-neither-rejected", Version: "1", Code: "INVALID_BATCH_RESPONSE_ITEM"},
	{Name: "response-version-rejected", Version: "2", HasResponse: true, Code: "INVALID_BATCH_VERSION"},
}

var requestBatchHTTPStatusCases = []httpStatusCase{
	{Name: "accepted-batch", Status: 200, BodyKind: "batch"},
	{Name: "malformed-envelope", Status: 400, BodyKind: "problem", PublicCode: "MALFORMED_JSON"},
	{Name: "invalid-batch-envelope", Status: 400, BodyKind: "problem", PublicCode: "INVALID_BATCH_ENVELOPE"},
	{Name: "invalid-batch-policy", Status: 400, BodyKind: "problem", PublicCode: "INVALID_BATCH_POLICY"},
	{Name: "invalid-batch-version", Status: 400, BodyKind: "problem", PublicCode: "INVALID_BATCH_VERSION"},
	{Name: "invalid-batch-item-id", Status: 400, BodyKind: "problem", PublicCode: "INVALID_BATCH_ITEM_ID"},
	{Name: "unauthenticated", Status: 401, BodyKind: "problem", PublicCode: "UNAUTHENTICATED"},
	{Name: "method-not-allowed", Status: 405, BodyKind: "problem", PublicCode: "METHOD_NOT_ALLOWED"},
	{Name: "not-acceptable", Status: 406, BodyKind: "problem", PublicCode: "NOT_ACCEPTABLE"},
	{Name: "request-timeout", Status: 408, BodyKind: "problem", PublicCode: "REQUEST_TIMEOUT"},
	{Name: "request-too-large", Status: 413, BodyKind: "problem", PublicCode: "REQUEST_TOO_LARGE"},
	{Name: "unsupported-media-type", Status: 415, BodyKind: "problem", PublicCode: "UNSUPPORTED_MEDIA_TYPE"},
	{Name: "unsupported-content-encoding", Status: 415, BodyKind: "problem", PublicCode: "UNSUPPORTED_CONTENT_ENCODING"},
	{Name: "atomic-preflight-failed", Status: 422, BodyKind: "problem", PublicCode: "ATOMIC_BATCH_PREFLIGHT_FAILED"},
	{Name: "atomic-unsupported", Status: 422, BodyKind: "problem", PublicCode: "ATOMIC_BATCH_UNSUPPORTED"},
	{Name: "rate-limited", Status: 429, BodyKind: "problem", PublicCode: "RATE_LIMITED"},
	{Name: "overloaded", Status: 503, BodyKind: "problem", PublicCode: "OVERLOADED"},
	{Name: "aggregate-wire-byte-limit", Status: 413, BodyKind: "problem", PublicCode: "REQUEST_TOO_LARGE"},
	{Name: "aggregate-item-count-limit", Status: 413, BodyKind: "problem", PublicCode: "RESOURCE_EXHAUSTED"},
	{Name: "aggregate-decoded-structure-limit", Status: 413, BodyKind: "problem", PublicCode: "RESOURCE_EXHAUSTED"},
	{Name: "aggregate-cost-limit", Status: 429, BodyKind: "problem", PublicCode: "RATE_LIMITED"},
	{Name: "aggregate-response-capacity", Status: 503, BodyKind: "problem", PublicCode: "RESOURCE_EXHAUSTED"},
	{Name: "failure-after-headers", Status: 0, BodyKind: "terminate", PublicCode: "TRANSPORT_TERMINATED", HeadersSent: true},
}

func requestBatchNamedCase[T requestBatchNamed](t *testing.T, cases []T, name string) T {
	t.Helper()
	index := slices.IndexFunc(cases, func(test T) bool {
		return test.requestBatchName() == name
	})
	if index < 0 {
		var zero T
		t.Fatalf("missing transport batch case %q", name)
		return zero
	}
	return cases[index]
}
