package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestPersistedResolverRegistersResolvesAndPreparesApprovedDocument(t *testing.T) {
	t.Parallel()

	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	store := runtime.NewMemoryPersistedStore()
	resolver, err := runtime.NewPersistedResolver(store, runtime.PersistedResolverOptions{
		Mode:                        runtime.PersistedRegisterAtDeploy,
		SchemaRevision:              "schema-1",
		AuthorizationPolicyRevision: "policy-1",
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	if err := resolver.RegisterDeploy(context.Background(), record); err != nil {
		t.Fatalf("RegisterDeploy: %v", err)
	}

	snapshot, calls := persistedSnapshot(t, 5)
	input := []byte(fmt.Sprintf(`{"version":"1","operation":"Q","persisted":{"algorithm":"%s","canonicalVersion":"%s","digest":"%s"}}`,
		record.Reference.Algorithm, record.Reference.CanonicalVersion, record.Reference.Digest))
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-a"})
	plan, admitted, err := resolver.Admit(ctx, snapshot, input, runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if admitted.Reference != record.Reference || admitted.Name != "read-text" || admitted.Tenant != "tenant-a" {
		t.Fatalf("admitted record = %#v", admitted)
	}
	if plan.StaticCost() != 5 {
		t.Fatalf("static cost = %d, want 5", plan.StaticCost())
	}
	if calls.Load() != 0 {
		t.Fatalf("admission invoked %d handlers", calls.Load())
	}
}

func TestPersistedLifecycleRevokesNewAdmissionMigratesAndEvicts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	key := runtime.PersistedKey{Tenant: record.Tenant, Reference: record.Reference}
	var events []runtime.PersistedEvent
	resolver, err := runtime.NewPersistedResolver(runtime.NewMemoryPersistedStore(), runtime.PersistedResolverOptions{
		Mode: runtime.PersistedRegisterAtDeploy, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
		Now: func() time.Time { return now }, OnChange: func(event runtime.PersistedEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	if err := resolver.RegisterDeploy(context.Background(), record); err != nil {
		t.Fatalf("RegisterDeploy: %v", err)
	}
	snapshot, calls := persistedSnapshot(t, 5)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-a"})
	input := persistedRequest(record)
	admitted, _, err := resolver.Admit(ctx, snapshot, input, runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("initial Admit: %v", err)
	}
	if err := resolver.Revoke(context.Background(), key); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := resolver.Admit(ctx, snapshot, input, runtime.PrepareOptions{}); persistedCode(err) != "PERSISTED_REVOKED" {
		t.Fatalf("post-revocation Admit error = %v", err)
	}
	if outcome := admitted.Execute(context.Background()); len(outcome.Errors) != 0 || calls.Load() != 1 {
		t.Fatalf("already admitted outcome = %#v, calls = %d", outcome, calls.Load())
	}

	replacement := record
	replacement.Revision = "approval-2"
	replacement.RevokedAt = time.Time{}
	if err := resolver.Migrate(context.Background(), key, "wrong-revision", replacement); persistedCode(err) != "PERSISTED_REVISION_CONFLICT" {
		t.Fatalf("conflicting Migrate error = %v", err)
	}
	if err := resolver.Migrate(context.Background(), key, "approval-1", replacement); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, _, err := resolver.Admit(ctx, snapshot, input, runtime.PrepareOptions{}); err != nil {
		t.Fatalf("post-migration Admit: %v", err)
	}
	if err := resolver.Evict(context.Background(), key); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, _, err := resolver.Admit(ctx, snapshot, input, runtime.PrepareOptions{}); persistedCode(err) != "PERSISTED_NOT_FOUND" {
		t.Fatalf("post-eviction Admit error = %v", err)
	}
	wantEvents := []runtime.PersistedEventKind{runtime.PersistedRegistered, runtime.PersistedRevoked, runtime.PersistedMigrated, runtime.PersistedEvicted}
	if !slices.EqualFunc(events, wantEvents, func(event runtime.PersistedEvent, kind runtime.PersistedEventKind) bool { return event.Kind == kind }) {
		t.Fatalf("events = %v, want kinds %v", events, wantEvents)
	}
}

func TestAutomaticPersistedRegistrationRequiresAuthenticationQuotaAndApproval(t *testing.T) {
	t.Parallel()

	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	if _, err := runtime.NewPersistedResolver(runtime.NewMemoryPersistedStore(), runtime.PersistedResolverOptions{
		Mode: runtime.PersistedAutomatic, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
	}); err == nil {
		t.Fatal("automatic resolver accepted missing quota and approval hooks")
	}

	quotaErr := errors.New("quota detail must stay private")
	approvalErr := errors.New("approval detail must stay private")
	quotaCalls, approvalCalls := 0, 0
	options := runtime.PersistedResolverOptions{
		Mode: runtime.PersistedAutomatic, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
		AutomaticRevision: "auto-policy-1",
		AutomaticQuota: func(context.Context, runtime.Principal) error {
			quotaCalls++
			return quotaErr
		},
		AutomaticApproval: func(context.Context, runtime.Principal, runtime.PersistedRecord) error {
			approvalCalls++
			return approvalErr
		},
	}
	newResolver := func() *runtime.PersistedResolver {
		resolver, err := runtime.NewPersistedResolver(runtime.NewMemoryPersistedStore(), options)
		if err != nil {
			t.Fatalf("NewPersistedResolver: %v", err)
		}
		return resolver
	}
	resolver := newResolver()
	if err := resolver.RegisterAutomatic(context.Background(), record); persistedCode(err) != "AUTOMATIC_AUTHENTICATION_REQUIRED" {
		t.Fatalf("anonymous RegisterAutomatic error = %v", err)
	}
	otherTenant := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-b"})
	if err := resolver.RegisterAutomatic(otherTenant, record); persistedCode(err) != "PERSISTED_TENANT_MISMATCH" {
		t.Fatalf("cross-tenant RegisterAutomatic error = %v", err)
	}
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-a"})
	if err := resolver.RegisterAutomatic(ctx, record); persistedCode(err) != "AUTOMATIC_QUOTA_DENIED" {
		t.Fatalf("quota RegisterAutomatic error = %v", err)
	}
	if quotaCalls != 1 || approvalCalls != 0 {
		t.Fatalf("hook calls after quota denial = %d, %d", quotaCalls, approvalCalls)
	}

	options.AutomaticQuota = func(context.Context, runtime.Principal) error { return nil }
	resolver = newResolver()
	if err := resolver.RegisterAutomatic(ctx, record); persistedCode(err) != "AUTOMATIC_APPROVAL_DENIED" {
		t.Fatalf("approval RegisterAutomatic error = %v", err)
	}
	options.AutomaticApproval = func(context.Context, runtime.Principal, runtime.PersistedRecord) error { return nil }
	resolver = newResolver()
	if err := resolver.RegisterAutomatic(ctx, record); err != nil {
		t.Fatalf("RegisterAutomatic: %v", err)
	}
}

func TestAutomaticAdmissionRegistersInlineDocumentAfterCostApproval(t *testing.T) {
	t.Parallel()

	store := runtime.NewMemoryPersistedStore()
	quotaCalls, approvalCalls := 0, 0
	resolver, err := runtime.NewPersistedResolver(store, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedAutomatic, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
		AutomaticRevision: "auto-policy-1",
		AutomaticQuota: func(context.Context, runtime.Principal) error {
			quotaCalls++
			return nil
		},
		AutomaticApproval: func(_ context.Context, principal runtime.Principal, record runtime.PersistedRecord) error {
			approvalCalls++
			if record.ApprovedCost != 5 || record.Owner != principal.Subject || record.Cache.Scope != runtime.PersistedCachePrivate {
				t.Fatalf("automatic approval record = %#v", record)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	snapshot, calls := persistedSnapshot(t, 5)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-a"})
	inline := []byte(`{"version":"1","id":"first","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]},"variables":{"runtimeOnly":"first"}}`)
	plan, record, err := resolver.Admit(ctx, snapshot, inline, runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("automatic Admit: %v", err)
	}
	if record.Reference.Digest == "" || record.Revision != "auto-policy-1" || record.Operation != "Q" || plan.StaticCost() != 5 {
		t.Fatalf("automatic record = %#v, cost = %d", record, plan.StaticCost())
	}
	if calls.Load() != 0 || quotaCalls != 1 || approvalCalls != 1 {
		t.Fatalf("handler/quota/approval calls = %d/%d/%d", calls.Load(), quotaCalls, approvalCalls)
	}
	secondContext := runtime.WithPrincipal(context.Background(), runtime.Principal{
		Subject: "user-a", Tenant: "tenant-a", Claims: map[string]string{"token": "different"}, AuthorizationRevision: "auth-2",
	})
	secondInline := []byte(`{"version":"1","id":"second","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]},"variables":{"runtimeOnly":"second"}}`)
	_, secondRecord, err := resolver.Admit(secondContext, snapshot, secondInline, runtime.PrepareOptions{})
	if err != nil {
		t.Fatalf("second automatic Admit: %v", err)
	}
	if secondRecord.Reference != record.Reference || quotaCalls != 2 || approvalCalls != 2 {
		t.Fatalf("request metadata changed identity: %#v versus %#v", secondRecord.Reference, record.Reference)
	}

	lookup, err := runtime.NewPersistedResolver(store, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedLookupOnly, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	if _, admitted, err := lookup.Admit(ctx, snapshot, persistedRequest(record), runtime.PrepareOptions{}); err != nil || admitted.Reference != record.Reference {
		t.Fatalf("persisted lookup after automatic registration = %#v, %v", admitted, err)
	}
}

func TestPersistedRegistrationAndMigrationRejectStaleApprovalBeforeStorage(t *testing.T) {
	t.Parallel()

	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	store := runtime.NewMemoryPersistedStore()
	resolver, err := runtime.NewPersistedResolver(store, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedRegisterAtDeploy, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	staleSchema := record
	staleSchema.SchemaRevision = "schema-old"
	if err := resolver.RegisterDeploy(context.Background(), staleSchema); persistedCode(err) != "PERSISTED_SCHEMA_STALE" {
		t.Fatalf("stale schema registration error = %v", err)
	}
	stalePolicy := record
	stalePolicy.AuthorizationPolicyRevision = "policy-old"
	if err := resolver.RegisterDeploy(context.Background(), stalePolicy); persistedCode(err) != "PERSISTED_POLICY_STALE" {
		t.Fatalf("stale policy registration error = %v", err)
	}
	if err := resolver.RegisterDeploy(context.Background(), record); err != nil {
		t.Fatalf("RegisterDeploy: %v", err)
	}
	key := runtime.PersistedKey{Tenant: record.Tenant, Reference: record.Reference}
	replacement := record
	replacement.Revision = "approval-2"
	replacement.SchemaRevision = "schema-old"
	if err := resolver.Migrate(context.Background(), key, record.Revision, replacement); persistedCode(err) != "PERSISTED_SCHEMA_STALE" {
		t.Fatalf("stale schema migration error = %v", err)
	}
}

func TestLookupOnlyPersistedResolverRejectsEveryLifecycleWrite(t *testing.T) {
	t.Parallel()

	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	record.ApprovedCost = 0
	store := runtime.NewMemoryPersistedStore()
	if err := store.Register(context.Background(), record); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resolver, err := runtime.NewPersistedResolver(store, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedLookupOnly, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	key := runtime.PersistedKey{Tenant: record.Tenant, Reference: record.Reference}
	replacement := record
	replacement.Revision = "approval-2"
	for name, mutate := range map[string]func() error{
		"register": func() error { return resolver.RegisterDeploy(context.Background(), record) },
		"revoke":   func() error { return resolver.Revoke(context.Background(), key) },
		"migrate":  func() error { return resolver.Migrate(context.Background(), key, record.Revision, replacement) },
		"evict":    func() error { return resolver.Evict(context.Background(), key) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := mutate(); persistedCode(err) != "PERSISTED_MODE_DENIED" {
				t.Fatalf("lookup-only mutation error = %v", err)
			}
		})
	}
	if _, err := store.Lookup(context.Background(), key); err != nil {
		t.Fatalf("lookup-only mutation changed storage: %v", err)
	}
}

func TestPersistedRecordsRejectTamperingCollisionsAndUnsafeSharedCaching(t *testing.T) {
	t.Parallel()

	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	tamperedDocument := decodePersistedDocument(t, `{"operations":[{"name":"Other","kind":"query","select":[]}]}`)
	tests := []struct {
		name   string
		mutate func(*runtime.PersistedRecord)
		code   string
	}{
		{name: "document mismatch", mutate: func(value *runtime.PersistedRecord) { value.Document = tamperedDocument.CanonicalJSON() }, code: "PERSISTED_DOCUMENT_MISMATCH"},
		{name: "algorithm mismatch", mutate: func(value *runtime.PersistedRecord) { value.Reference.Algorithm = "sha-512" }, code: "PERSISTED_ALGORITHM_MISMATCH"},
		{name: "operation mismatch", mutate: func(value *runtime.PersistedRecord) { value.Operation = "Other" }, code: "PERSISTED_OPERATION_MISMATCH"},
		{name: "kind mismatch", mutate: func(value *runtime.PersistedRecord) { value.Kind = protocol.Mutation }, code: "PERSISTED_OPERATION_MISMATCH"},
		{name: "capability mismatch", mutate: func(value *runtime.PersistedRecord) { value.RequiredCapabilities = []string{"undeclared-1"} }, code: "PERSISTED_CAPABILITY_MISMATCH"},
		{name: "shared without public output", mutate: func(value *runtime.PersistedRecord) {
			value.Cache = runtime.PersistedCachePolicy{Scope: runtime.PersistedCacheShared, AuthorizationInvariant: true}
		}, code: "PERSISTED_CACHE_INVALID"},
		{name: "shared without authorization invariance", mutate: func(value *runtime.PersistedRecord) {
			value.Cache = runtime.PersistedCachePolicy{Scope: runtime.PersistedCacheShared, PublicOutput: true}
		}, code: "PERSISTED_CACHE_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := record
			test.mutate(&candidate)
			resolver, err := runtime.NewPersistedResolver(runtime.NewMemoryPersistedStore(), runtime.PersistedResolverOptions{
				Mode: runtime.PersistedRegisterAtDeploy, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
			})
			if err != nil {
				t.Fatalf("NewPersistedResolver: %v", err)
			}
			if err := resolver.RegisterDeploy(context.Background(), candidate); persistedCode(err) != test.code {
				t.Fatalf("RegisterDeploy error = %v, want %s", err, test.code)
			}
		})
	}

	store := runtime.NewMemoryPersistedStore()
	if err := store.Register(context.Background(), record); err != nil {
		t.Fatalf("Register: %v", err)
	}
	record.Document[0] = 'x'
	stored, err := store.Lookup(context.Background(), runtime.PersistedKey{Tenant: "tenant-a", Reference: record.Reference})
	if err != nil || stored.Document[0] != '{' {
		t.Fatalf("stored record was mutable: %q, %v", stored.Document, err)
	}
	collision := stored
	collision.Document = tamperedDocument.CanonicalJSON()
	if err := store.Register(context.Background(), collision); persistedCode(err) != "PERSISTED_HASH_COLLISION" {
		t.Fatalf("collision error = %v", err)
	}
	collision.Tenant = "tenant-b"
	if err := store.Register(context.Background(), collision); err != nil {
		t.Fatalf("tenant-isolated registration: %v", err)
	}
}

func TestPersistedAdmissionRejectsTenantExpiryCostAndStorageFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"text"}}]}]}`)
	record := persistedRecord(t, "tenant-a", document)
	record.ExpiresAt = now
	store := runtime.NewMemoryPersistedStore()
	if err := store.Register(context.Background(), record); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resolver, err := runtime.NewPersistedResolver(store, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedLookupOnly, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	snapshot, _ := persistedSnapshot(t, 5)
	ctx := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-a"})
	if _, _, err := resolver.Admit(ctx, snapshot, persistedRequest(record), runtime.PrepareOptions{}); persistedCode(err) != "PERSISTED_EXPIRED" {
		t.Fatalf("expiry-boundary Admit error = %v", err)
	}
	otherTenant := runtime.WithPrincipal(context.Background(), runtime.Principal{Subject: "user-a", Tenant: "tenant-b"})
	if _, _, err := resolver.Admit(otherTenant, snapshot, persistedRequest(record), runtime.PrepareOptions{}); persistedCode(err) != "PERSISTED_NOT_FOUND" {
		t.Fatalf("cross-tenant Admit error = %v", err)
	}

	record.ExpiresAt = time.Time{}
	record.ApprovedCost = 4
	costStore := runtime.NewMemoryPersistedStore()
	if err := costStore.Register(context.Background(), record); err != nil {
		t.Fatalf("Register cost record: %v", err)
	}
	costResolver, err := runtime.NewPersistedResolver(costStore, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedLookupOnly, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	if _, _, err := costResolver.Admit(ctx, snapshot, persistedRequest(record), runtime.PrepareOptions{}); persistedCode(err) != "PERSISTED_COST_EXCEEDED" {
		t.Fatalf("cost Admit error = %v", err)
	}

	storageCause := errors.New("secret database location")
	failing, err := runtime.NewPersistedResolver(failingPersistedStore{err: storageCause}, runtime.PersistedResolverOptions{
		Mode: runtime.PersistedLookupOnly, SchemaRevision: "schema-1", AuthorizationPolicyRevision: "policy-1",
	})
	if err != nil {
		t.Fatalf("NewPersistedResolver: %v", err)
	}
	_, _, err = failing.Admit(ctx, snapshot, persistedRequest(record), runtime.PrepareOptions{})
	if persistedCode(err) != "PERSISTED_STORAGE" || strings.Contains(err.Error(), "database") || !errors.Is(err, storageCause) {
		t.Fatalf("storage error = %v", err)
	}
}

func TestMemoryPersistedStoreConcurrentLifecycleIsIsolated(t *testing.T) {
	t.Parallel()

	document := decodePersistedDocument(t, `{"operations":[{"name":"Q","kind":"query","select":[]}]}`)
	store := runtime.NewMemoryPersistedStore()
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			record := persistedRecord(t, fmt.Sprintf("tenant-%d", index), document)
			record.ApprovedCost = 0
			key := runtime.PersistedKey{Tenant: record.Tenant, Reference: record.Reference}
			if err := store.Register(context.Background(), record); err != nil {
				t.Errorf("Register(%d): %v", index, err)
				return
			}
			lookedUp, err := store.Lookup(context.Background(), key)
			if err != nil || lookedUp.Tenant != record.Tenant {
				t.Errorf("Lookup(%d) = %#v, %v", index, lookedUp, err)
				return
			}
			replacement := record
			replacement.Revision = "approval-2"
			if err := store.Migrate(context.Background(), key, "approval-1", replacement); err != nil {
				t.Errorf("Migrate(%d): %v", index, err)
				return
			}
			if err := store.Revoke(context.Background(), key, time.Now()); err != nil {
				t.Errorf("Revoke(%d): %v", index, err)
			}
		}()
	}
	wait.Wait()
}

type failingPersistedStore struct{ err error }

func (s failingPersistedStore) Register(context.Context, runtime.PersistedRecord) error {
	return s.err
}
func (s failingPersistedStore) Lookup(context.Context, runtime.PersistedKey) (runtime.PersistedRecord, error) {
	return runtime.PersistedRecord{}, s.err
}
func (s failingPersistedStore) Revoke(context.Context, runtime.PersistedKey, time.Time) error {
	return s.err
}
func (s failingPersistedStore) Migrate(context.Context, runtime.PersistedKey, string, runtime.PersistedRecord) error {
	return s.err
}
func (s failingPersistedStore) Evict(context.Context, runtime.PersistedKey) error { return s.err }

func decodePersistedDocument(t testing.TB, input string) protocol.Document {
	t.Helper()
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","document":`+input+`}`, protocol.DecodeOptions{})
	return *request.Document()
}

func persistedRecord(t testing.TB, tenant string, document protocol.Document) runtime.PersistedRecord {
	t.Helper()
	digest, err := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	return runtime.PersistedRecord{
		Reference:                   protocol.PersistedReference{Algorithm: digest.Algorithm, CanonicalVersion: digest.CanonicalVersion, Digest: digest.Hex},
		Tenant:                      tenant,
		Name:                        "read-text",
		Revision:                    "approval-1",
		Owner:                       "team-a",
		Operation:                   "Q",
		Kind:                        protocol.Query,
		Document:                    document.CanonicalJSON(),
		SchemaRevision:              "schema-1",
		AuthorizationPolicyRevision: "policy-1",
		ApprovedCost:                5,
		Cache:                       runtime.PersistedCachePolicy{Scope: runtime.PersistedCachePrivate},
	}
}

func persistedRequest(record runtime.PersistedRecord) []byte {
	return []byte(fmt.Sprintf(`{"version":"1","operation":"%s","persisted":{"algorithm":"%s","canonicalVersion":"%s","digest":"%s"}}`,
		record.Operation, record.Reference.Algorithm, record.Reference.CanonicalVersion, record.Reference.Digest))
}

func persistedCode(err error) string {
	var persisted *runtime.PersistedError
	if errors.As(err, &persisted) {
		return persisted.Code
	}
	return ""
}

func persistedSnapshot(t *testing.T, cost uint64) (runtime.Snapshot, *atomic.Int64) {
	t.Helper()
	registry := runtime.NewRegistry(coreTypes(t))
	metadata := completeMetadata(runtime.ReadEffect)
	metadata.Cost = cost
	descriptor := runtime.Descriptor{
		Name: "text", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: metadata,
	}
	var calls atomic.Int64
	if err := registry.Register(runtime.BindInvocation[string](descriptor, func(context.Context, runtime.Invocation) (string, error) {
		calls.Add(1)
		return "text", nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return snapshot, &calls
}
