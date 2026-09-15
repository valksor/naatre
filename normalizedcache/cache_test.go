package normalizedcache_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/normalizedcache"
	"github.com/valksor/naatre/schema"
)

func TestIdentityIsAliasIndependentOpaqueAndScopeBound(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	left := normalizedcache.Scope{Subject: "reader", Tenant: "tenant-a", AuthorizationRevision: "auth-1"}
	right := normalizedcache.Scope{Subject: "reader", Tenant: "tenant-b", AuthorizationRevision: "auth-1"}
	canonicalFields := map[string]json.RawMessage{"id": json.RawMessage(`"customer-secret-42"`)}
	fromAliasA := identify(t, snapshot, left, canonicalFields)
	fromAliasB := identify(t, snapshot, left, canonicalFields)
	otherTenant := identify(t, snapshot, right, canonicalFields)
	if fromAliasA != fromAliasB || fromAliasA.Reference() != fromAliasB.Reference() {
		t.Fatal("canonical identity changed across aliases or response locations")
	}
	if fromAliasA == otherTenant || fromAliasA.Reference() != otherTenant.Reference() {
		t.Fatal("entity reference and authorization scope were not separated")
	}
	if strings.Contains(fromAliasA.Reference(), "customer-secret-42") {
		t.Fatalf("opaque reference exposed identity field: %s", fromAliasA.Reference())
	}
	if _, err := normalizedcache.Identify(snapshot, "String", left, canonicalFields); !errors.Is(err, normalizedcache.ErrUnknownEntity) {
		t.Fatalf("non-entity identity error = %v", err)
	}
}

func TestPartialStreamMergingPreservesKnownValuesAndPaginationWindows(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	scope := normalizedcache.Scope{Subject: "reader", Tenant: "tenant-a", AuthorizationRevision: "auth-1"}
	entity := identify(t, snapshot, scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	cache := normalizedcache.New(snapshot)
	name := fieldKey(t, "name", `{}`)
	firstPage := fieldKey(t, "friends", `{"first":10}`)
	secondPage := fieldKey(t, "friends", `{"after":"cursor-1","first":10}`)

	merge(t, cache, normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 2, Fields: []normalizedcache.FieldPatch{
		{Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage(`"Alice"`)},
		{Key: firstPage, State: normalizedcache.FieldValue, Value: json.RawMessage(`"page-1"`)},
	}})
	result, err := cache.Merge(normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 1, Fields: []normalizedcache.FieldPatch{
		{Key: name, State: normalizedcache.FieldNull},
		{Key: secondPage, State: normalizedcache.FieldValue, Value: json.RawMessage(`"page-2"`)},
		{Key: fieldKey(t, "futureField", `{}`), State: normalizedcache.FieldValue, Value: json.RawMessage(`true`)},
	}}, normalizedcache.ConflictReject)
	if err != nil || len(result.Preserved) != 1 || len(result.IgnoredFields) != 1 || result.IgnoredFields[0] != "futureField" {
		t.Fatalf("out-of-order partial merge = %#v, %v", result, err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"Alice"`)
	assertField(t, cache, entity, firstPage, normalizedcache.FieldValue, `"page-1"`)
	assertField(t, cache, entity, secondPage, normalizedcache.FieldValue, `"page-2"`)

	for order, state := range map[uint64]normalizedcache.FieldState{3: normalizedcache.FieldAbsent, 4: normalizedcache.FieldSkipped, 5: normalizedcache.FieldPending, 6: normalizedcache.FieldFailed} {
		merge(t, cache, normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: order, Fields: []normalizedcache.FieldPatch{{Key: name, State: state}}})
		assertField(t, cache, entity, name, normalizedcache.FieldValue, `"Alice"`)
	}
	if _, err := cache.Merge(normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 7, Fields: []normalizedcache.FieldPatch{{Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage("null")}}}, normalizedcache.ConflictReject); err == nil {
		t.Fatal("value-state null was accepted as an explicit null")
	}
	merge(t, cache, normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 8, Fields: []normalizedcache.FieldPatch{{Key: name, State: normalizedcache.FieldNull}}})
	assertField(t, cache, entity, name, normalizedcache.FieldNull, "null")
}

func TestRevisionLineageFencesStaleAndDivergentResponses(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	scope := normalizedcache.Scope{Subject: "reader", Tenant: "tenant", AuthorizationRevision: "auth"}
	entity := identify(t, snapshot, scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	cache := normalizedcache.New(snapshot)
	name := fieldKey(t, "name", `{}`)
	friends := fieldKey(t, "friends", `{}`)
	merge(t, cache, patch(entity, name, "r1", "", 1, `"one"`))
	merge(t, cache, patch(entity, name, "r2", "r1", 2, `"two"`))
	if _, err := cache.Merge(normalizedcache.EntityPatch{Entity: entity, Revision: "r2", Order: 2, Fields: []normalizedcache.FieldPatch{
		{Key: friends, State: normalizedcache.FieldValue, Value: json.RawMessage(`"partial"`)},
		{Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage(`"conflict"`)},
	}}, normalizedcache.ConflictReject); !errors.Is(err, normalizedcache.ErrRevisionConflict) {
		t.Fatalf("same-order conflict error = %v", err)
	}
	if _, ok := cache.Lookup(entity, friends); ok {
		t.Fatal("failed multi-field merge left a partial cache write")
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"two"`)
	if _, err := cache.Merge(patch(entity, name, "r1", "", 3, `"late"`), normalizedcache.ConflictReject); !errors.Is(err, normalizedcache.ErrStaleRevision) {
		t.Fatalf("older known revision error = %v", err)
	}
	if _, err := cache.Merge(patch(entity, name, "fork", "unrelated", 4, `"fork"`), normalizedcache.ConflictReject); !errors.Is(err, normalizedcache.ErrRevisionConflict) {
		t.Fatalf("divergent revision error = %v", err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"two"`)
	if _, err := cache.Merge(patch(entity, name, "fork", "unrelated", 4, `"fork"`), normalizedcache.ConflictReplaceIncoming); err != nil {
		t.Fatalf("explicit conflict policy: %v", err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"fork"`)
}

func TestMutationInvalidationIsSynchronousAndScopeBound(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	cache := normalizedcache.New(snapshot)
	scopeA := normalizedcache.Scope{Subject: "reader-a", Tenant: "tenant-a", AuthorizationRevision: "auth-1"}
	scopeB := normalizedcache.Scope{Subject: "reader-b", Tenant: "tenant-b", AuthorizationRevision: "auth-1"}
	requestA := requestKey(t, scopeA)
	requestB := requestKey(t, scopeB)
	entityA := identify(t, snapshot, scopeA, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	entityB := identify(t, snapshot, scopeB, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	name := fieldKey(t, "name", `{}`)
	for _, entity := range []normalizedcache.EntityID{entityA, entityB} {
		merge(t, cache, normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 1, Fields: []normalizedcache.FieldPatch{{
			Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage(`"cached"`), Tags: []string{"User:list"},
		}}})
	}
	for _, key := range []normalizedcache.RequestKey{requestA, requestB} {
		if err := cache.StoreRequest(key, normalizedcache.RequestEntry{Data: json.RawMessage(`{"users":[1]}`), Tags: []string{"User:list"}}, normalizedcache.CachePrivate); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.ApplyMutationEffect(normalizedcache.MutationEffect{Scope: scopeA, Tags: []string{"User:list"}}); err != nil {
		t.Fatal(err)
	}
	if _, state := cache.LookupRequest(requestA, time.Now()); state != normalizedcache.FreshnessMiss {
		t.Fatalf("mutated request scope remained %s", state)
	}
	if _, state := cache.LookupRequest(requestB, time.Now()); state != normalizedcache.FreshnessFresh {
		t.Fatalf("other tenant request state = %s", state)
	}
	if _, ok := cache.Lookup(entityA, name); ok {
		t.Fatal("matching normalized entity survived mutation invalidation")
	}
	if _, ok := cache.Lookup(entityB, name); !ok {
		t.Fatal("other tenant normalized entity was invalidated")
	}
	if err := cache.StoreRequest(requestB, normalizedcache.RequestEntry{Data: json.RawMessage(`{}`)}, normalizedcache.CacheNoStore); err != nil {
		t.Fatal(err)
	}
	if _, state := cache.LookupRequest(requestB, time.Now()); state != normalizedcache.FreshnessMiss {
		t.Fatalf("no-store request remained %s", state)
	}
}

func TestOptimisticMutationReconciliationOutcomes(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	cache := normalizedcache.New(snapshot)
	scope := normalizedcache.Scope{Subject: "writer", Tenant: "tenant", AuthorizationRevision: "auth"}
	entity := identify(t, snapshot, scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	other := identify(t, snapshot, scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-2"`)})
	name := fieldKey(t, "name", `{}`)
	merge(t, cache, patch(entity, name, "r1", "", 1, `"base"`))

	begin := func(id, value string) {
		t.Helper()
		err := cache.BeginOptimistic(normalizedcache.OptimisticMutation{
			ID:     id,
			Effect: normalizedcache.MutationEffect{Scope: scope, Tags: []string{"User:list"}},
			Patches: []normalizedcache.OptimisticPatch{{
				Entity:           entity,
				ExpectedRevision: currentRevision(t, cache, entity),
				Fields: []normalizedcache.FieldPatch{{
					Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage(value),
				}},
			}},
		})
		if err != nil {
			t.Fatalf("BeginOptimistic(%s): %v", id, err)
		}
	}

	begin("reject", `"optimistic-reject"`)
	assertOptimistic(t, cache, entity, name, `"optimistic-reject"`, false)
	if err := cache.ResolveOptimistic("reject", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationRejected}); err != nil {
		t.Fatal(err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"base"`)

	begin("first-layer", `"first"`)
	begin("second-layer", `"second"`)
	assertOptimistic(t, cache, entity, name, `"second"`, false)
	if err := cache.ResolveOptimistic("first-layer", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationRejected}); err != nil {
		t.Fatal(err)
	}
	assertOptimistic(t, cache, entity, name, `"second"`, false)
	if err := cache.ResolveOptimistic("second-layer", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationRejected}); err != nil {
		t.Fatal(err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"base"`)

	begin("timeout", `"optimistic-timeout"`)
	if err := cache.ResolveOptimistic("timeout", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationTimedOut}); err != nil {
		t.Fatal(err)
	}
	assertOptimistic(t, cache, entity, name, `"optimistic-timeout"`, true)
	if err := cache.ResolveOptimistic("timeout", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationUnknown}); err != nil {
		t.Fatal(err)
	}
	if state, ok := cache.OptimisticState("timeout"); !ok || state != normalizedcache.OptimisticUnknown {
		t.Fatalf("unknown commit state = %q, %t", state, ok)
	}
	if err := cache.ResolveOptimistic("timeout", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationCommitted, Patches: []normalizedcache.EntityPatch{patch(entity, name, "r2", "r1", 2, `"server"`)}}); err != nil {
		t.Fatal(err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"server"`)

	begin("late", `"tentative-late"`)
	merge(t, cache, patch(entity, name, "r3", "r2", 3, `"newer"`))
	err := cache.ResolveOptimistic("late", normalizedcache.MutationResolution{Outcome: normalizedcache.MutationCommitted, Patches: []normalizedcache.EntityPatch{
		patch(other, name, "r1", "", 1, `"must-not-stick"`),
		patch(entity, name, "r2", "r1", 4, `"late-server"`),
	}})
	if !errors.Is(err, normalizedcache.ErrStaleRevision) {
		t.Fatalf("late committed response error = %v", err)
	}
	assertField(t, cache, entity, name, normalizedcache.FieldValue, `"newer"`)
	if _, ok := cache.Lookup(other, name); ok {
		t.Fatal("failed optimistic reconciliation left a partial committed patch")
	}
}

func TestScopeInvalidationClearsCacheAndCancelsSubscriptions(t *testing.T) {
	for _, reason := range []normalizedcache.ScopeInvalidation{normalizedcache.ScopeLogout, normalizedcache.ScopeRevoked, normalizedcache.ScopeTenantSwitch} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			snapshot := cacheSchema(t)
			cache := normalizedcache.New(snapshot)
			scope := normalizedcache.Scope{Subject: "reader", Tenant: "old", AuthorizationRevision: "revoked"}
			entity := identify(t, snapshot, scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
			name := fieldKey(t, "name", `{}`)
			merge(t, cache, patch(entity, name, "r1", "", 1, `"private"`))
			request := requestKey(t, scope)
			if err := cache.StoreRequest(request, normalizedcache.RequestEntry{Data: json.RawMessage(`{}`)}, normalizedcache.CachePrivate); err != nil {
				t.Fatal(err)
			}
			var cancelled normalizedcache.ScopeInvalidation
			if err := cache.RegisterSubscription("live-1", scope, func(actual normalizedcache.ScopeInvalidation) { cancelled = actual }); err != nil {
				t.Fatal(err)
			}
			cache.InvalidateScope(scope, reason)
			if _, ok := cache.Lookup(entity, name); ok {
				t.Fatalf("entity survived %s", reason)
			}
			if _, state := cache.LookupRequest(request, time.Now()); state != normalizedcache.FreshnessMiss {
				t.Fatalf("request survived %s", reason)
			}
			if cancelled != reason {
				t.Fatalf("subscription cancellation = %q", cancelled)
			}
		})
	}
}

func TestRequestFreshnessTransitions(t *testing.T) {
	t.Parallel()
	cache := normalizedcache.New(cacheSchema(t))
	key := requestKey(t, normalizedcache.Scope{Subject: "reader", Tenant: "tenant", AuthorizationRevision: "auth"})
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	entry := normalizedcache.RequestEntry{Data: json.RawMessage(`{"ok":true}`), Freshness: normalizedcache.Freshness{
		FreshUntil: now.Add(time.Minute), StaleUntil: now.Add(2 * time.Minute),
	}}
	if err := cache.StoreRequest(key, entry, normalizedcache.CachePrivate); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		at   time.Time
		want normalizedcache.FreshnessState
	}{{now, normalizedcache.FreshnessFresh}, {now.Add(90 * time.Second), normalizedcache.FreshnessStale}, {now.Add(3 * time.Minute), normalizedcache.FreshnessMiss}} {
		if _, state := cache.LookupRequest(key, test.at); state != test.want {
			t.Fatalf("freshness at %s = %s, want %s", test.at, state, test.want)
		}
	}
}

func TestFieldFreshnessTransitions(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	cache := normalizedcache.New(snapshot)
	scope := normalizedcache.Scope{Subject: "reader", Tenant: "tenant", AuthorizationRevision: "auth"}
	entity := identify(t, snapshot, scope, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	name := fieldKey(t, "name", `{}`)
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	merge(t, cache, normalizedcache.EntityPatch{Entity: entity, Revision: "r1", Order: 1, Fields: []normalizedcache.FieldPatch{{
		Key: name, State: normalizedcache.FieldValue, Value: json.RawMessage(`"fresh"`), Freshness: normalizedcache.Freshness{
			FreshUntil: now.Add(time.Minute), StaleUntil: now.Add(2 * time.Minute),
		},
	}}})
	for _, test := range []struct {
		at   time.Time
		want normalizedcache.FreshnessState
	}{{now, normalizedcache.FreshnessFresh}, {now.Add(90 * time.Second), normalizedcache.FreshnessStale}, {now.Add(3 * time.Minute), normalizedcache.FreshnessMiss}} {
		if _, state := cache.LookupAt(entity, name, test.at); state != test.want {
			t.Fatalf("field freshness at %s = %s, want %s", test.at, state, test.want)
		}
	}
}

func TestMutationEffectsCannotCrossAuthorizationScope(t *testing.T) {
	t.Parallel()
	snapshot := cacheSchema(t)
	cache := normalizedcache.New(snapshot)
	scopeA := normalizedcache.Scope{Subject: "reader", Tenant: "a", AuthorizationRevision: "auth"}
	scopeB := normalizedcache.Scope{Subject: "reader", Tenant: "b", AuthorizationRevision: "auth"}
	entityB := identify(t, snapshot, scopeB, map[string]json.RawMessage{"id": json.RawMessage(`"u-1"`)})
	err := cache.ApplyMutationEffect(normalizedcache.MutationEffect{Scope: scopeA, Entities: []normalizedcache.EntityID{entityB}})
	if err == nil {
		t.Fatal("cross-scope mutation effect was accepted")
	}
}

func cacheSchema(t testing.TB) schema.Snapshot {
	t.Helper()
	catalog := schema.NewCatalog()
	err := catalog.Register(schema.TypeDescriptor{ID: "User", Name: "User", Kind: schema.ObjectType, Output: true, Entity: &schema.EntityDescriptor{Keys: []string{"id"}}, Fields: map[string]schema.FieldDescriptor{
		"id": {ID: "User.id", Type: schema.TypeID(schema.ID)}, "name": {ID: "User.name", Type: schema.TypeID(schema.String), Nullable: true},
		"friends": {ID: "User.friends", Type: schema.TypeID(schema.String)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func identify(t testing.TB, snapshot schema.Snapshot, scope normalizedcache.Scope, fields map[string]json.RawMessage) normalizedcache.EntityID {
	t.Helper()
	entity, err := normalizedcache.Identify(snapshot, "User", scope, fields)
	if err != nil {
		t.Fatal(err)
	}
	return entity
}

func fieldKey(t testing.TB, name, arguments string) normalizedcache.FieldKey {
	t.Helper()
	key, err := normalizedcache.NewFieldKey(name, json.RawMessage(arguments), "lv-LV", "default", "schema-r1")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func requestKey(t testing.TB, scope normalizedcache.Scope) normalizedcache.RequestKey {
	t.Helper()
	key, err := normalizedcache.NewRequestKey(scope, "schema-r1", json.RawMessage(`{"operation":"Users","variables":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func patch(entity normalizedcache.EntityID, key normalizedcache.FieldKey, revision, base string, order uint64, value string) normalizedcache.EntityPatch {
	return normalizedcache.EntityPatch{Entity: entity, Revision: revision, BaseRevision: base, Order: order, Fields: []normalizedcache.FieldPatch{{Key: key, State: normalizedcache.FieldValue, Value: json.RawMessage(value)}}}
}

func merge(t testing.TB, cache *normalizedcache.Cache, patch normalizedcache.EntityPatch) {
	t.Helper()
	if _, err := cache.Merge(patch, normalizedcache.ConflictReject); err != nil {
		t.Fatal(err)
	}
}

func assertField(t testing.TB, cache *normalizedcache.Cache, entity normalizedcache.EntityID, key normalizedcache.FieldKey, state normalizedcache.FieldState, value string) {
	t.Helper()
	field, ok := cache.Lookup(entity, key)
	if !ok || field.State != state || string(field.Value) != value || field.OptimisticMutation != "" {
		t.Fatalf("field = %#v, %t; want %s %s", field, ok, state, value)
	}
}

func assertOptimistic(t testing.TB, cache *normalizedcache.Cache, entity normalizedcache.EntityID, key normalizedcache.FieldKey, value string, pending bool) {
	t.Helper()
	field, ok := cache.Lookup(entity, key)
	if !ok || string(field.Value) != value || field.OptimisticMutation == "" || field.PendingReconciliation != pending {
		t.Fatalf("optimistic field = %#v, %t", field, ok)
	}
}

func currentRevision(t testing.TB, cache *normalizedcache.Cache, entity normalizedcache.EntityID) string {
	t.Helper()
	revision, ok := cache.CurrentRevision(entity)
	if !ok {
		t.Fatal("entity revision is absent")
	}
	return revision
}
