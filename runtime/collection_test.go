package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

func TestCursorCodecBindsCanonicalCollectionScopeAndDirection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "current",
		Keys:        map[string][]byte{"current": []byte("0123456789abcdef0123456789abcdef")},
		TTL:         time.Hour,
		Now:         func() time.Time { return now },
		MaxPageSize: 50,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scope, err := runtime.NewCursorScope("users", []byte(`{"status":"active","role":"admin"}`), []byte(`[{"field":"name","direction":"asc"}]`), runtime.CursorScopeOptions{
		Tenant: "tenant-a", Authorization: "policy-7", SnapshotPolicy: runtime.SnapshotPagination, Snapshot: "snapshot-42",
	})
	if err != nil {
		t.Fatalf("NewCursorScope: %v", err)
	}
	cursor, err := codec.Encode(scope, runtime.CursorForward, runtime.CursorPosition{SortKey: "Ada", TieBreaker: "u-1"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	equivalent, err := runtime.NewCursorScope("users", []byte(`{"role":"admin","status":"active"}`), []byte(`[{"direction":"asc","field":"name"}]`), runtime.CursorScopeOptions{
		Tenant: "tenant-a", Authorization: "policy-7", SnapshotPolicy: runtime.SnapshotPagination, Snapshot: "snapshot-42",
	})
	if err != nil {
		t.Fatalf("NewCursorScope equivalent: %v", err)
	}
	position, err := codec.Decode(cursor, equivalent, runtime.CursorForward)
	if err != nil || position != (runtime.CursorPosition{SortKey: "Ada", TieBreaker: "u-1"}) {
		t.Fatalf("Decode = %#v, %v", position, err)
	}

	changed := []runtime.CursorScope{
		withCursorScope(t, "admins", `{"role":"admin","status":"active"}`, `[{"direction":"asc","field":"name"}]`, "tenant-a", "policy-7", runtime.SnapshotPagination, "snapshot-42"),
		withCursorScope(t, "users", `{"role":"member","status":"active"}`, `[{"direction":"asc","field":"name"}]`, "tenant-a", "policy-7", runtime.SnapshotPagination, "snapshot-42"),
		withCursorScope(t, "users", `{"role":"admin","status":"active"}`, `[{"direction":"desc","field":"name"}]`, "tenant-a", "policy-7", runtime.SnapshotPagination, "snapshot-42"),
		withCursorScope(t, "users", `{"role":"admin","status":"active"}`, `[{"direction":"asc","field":"name"}]`, "tenant-b", "policy-7", runtime.SnapshotPagination, "snapshot-42"),
		withCursorScope(t, "users", `{"role":"admin","status":"active"}`, `[{"direction":"asc","field":"name"}]`, "tenant-a", "policy-8", runtime.SnapshotPagination, "snapshot-42"),
		withCursorScope(t, "users", `{"role":"admin","status":"active"}`, `[{"direction":"asc","field":"name"}]`, "tenant-a", "policy-7", runtime.SnapshotPagination, "snapshot-43"),
	}
	for index, other := range changed {
		if _, err := codec.Decode(cursor, other, runtime.CursorForward); !errors.Is(err, runtime.ErrInvalidCursor) {
			t.Fatalf("scope change %d error = %v, want ErrInvalidCursor", index, err)
		}
	}
	if _, err := codec.Decode(cursor, scope, runtime.CursorBackward); !errors.Is(err, runtime.ErrInvalidCursor) {
		t.Fatalf("direction change error = %v, want ErrInvalidCursor", err)
	}
}

func TestCursorCodecRejectsTamperingExpiryAndRemovedKeys(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	config := runtime.CursorCodecConfig{
		ActiveKeyID: "old",
		Keys: map[string][]byte{
			"old": []byte("old-key-material-is-at-least-32-bytes"),
			"new": []byte("new-key-material-is-at-least-32-bytes"),
		},
		TTL: time.Minute, Now: func() time.Time { return now }, MaxPageSize: 10,
	}
	codec, err := runtime.NewCursorCodec(config)
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scope := withCursorScope(t, "users", `{}`, `[]`, "tenant-a", "policy-1", runtime.LivePagination, "")
	cursor, err := codec.Encode(scope, runtime.CursorForward, runtime.CursorPosition{SortKey: "A", TieBreaker: "1"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	tampered := cursor[:len(cursor)-1] + "A"
	if tampered == cursor {
		tampered = cursor[:len(cursor)-1] + "B"
	}
	if _, err := codec.Decode(tampered, scope, runtime.CursorForward); !errors.Is(err, runtime.ErrInvalidCursor) {
		t.Fatalf("tampered error = %v, want ErrInvalidCursor", err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := codec.Decode(cursor, scope, runtime.CursorForward); !errors.Is(err, runtime.ErrInvalidCursor) {
		t.Fatalf("expired error = %v, want ErrInvalidCursor", err)
	}
	now = now.Add(-2 * time.Minute)
	config.ActiveKeyID = "new"
	delete(config.Keys, "old")
	rotated, err := runtime.NewCursorCodec(config)
	if err != nil {
		t.Fatalf("NewCursorCodec rotated: %v", err)
	}
	if _, err := rotated.Decode(cursor, scope, runtime.CursorForward); !errors.Is(err, runtime.ErrInvalidCursor) {
		t.Fatalf("removed key error = %v, want ErrInvalidCursor", err)
	}
}

func TestPaginateUsesStableCompositeBoundariesAcrossWrites(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "k1", Keys: map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, Now: func() time.Time { return now }, MaxPageSize: 3,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scope := withCursorScope(t, "users", `{}`, `[{"field":"name"},{"field":"id"}]`, "tenant-a", "policy-1", runtime.LivePagination, "")
	entries := []runtime.PageEntry[string]{
		{Item: "Ada/u-1", Position: runtime.CursorPosition{SortKey: "Ada", TieBreaker: "u-1"}},
		{Item: "Ada/u-2", Position: runtime.CursorPosition{SortKey: "Ada", TieBreaker: "u-2"}},
		{Item: "Lin/u-3", Position: runtime.CursorPosition{SortKey: "Lin", TieBreaker: "u-3"}},
	}
	first := uint64(2)
	page, err := runtime.Paginate(codec, entries, runtime.PageRequest{First: &first, Scope: scope, IncludeTotalCount: true})
	if err != nil {
		t.Fatalf("Paginate first: %v", err)
	}
	if got := page.Items; len(got) != 2 || got[0] != "Ada/u-1" || got[1] != "Ada/u-2" || !page.PageInfo.HasNextPage || page.TotalCount == nil || *page.TotalCount != 3 {
		t.Fatalf("first page = %#v", page)
	}

	// The cursor item is deleted and rows are inserted on both sides. A live
	// cursor resumes strictly after its composite position, not after an offset.
	changed := []runtime.PageEntry[string]{
		{Item: "Aaron/u-0", Position: runtime.CursorPosition{SortKey: "Aaron", TieBreaker: "u-0"}},
		{Item: "Ada/u-1", Position: runtime.CursorPosition{SortKey: "Ada", TieBreaker: "u-1"}},
		{Item: "Bea/u-4", Position: runtime.CursorPosition{SortKey: "Bea", TieBreaker: "u-4"}},
		{Item: "Lin/u-3", Position: runtime.CursorPosition{SortKey: "Lin", TieBreaker: "u-3"}},
	}
	after := page.PageInfo.EndCursor
	next, err := runtime.Paginate(codec, changed, runtime.PageRequest{First: &first, After: after, Scope: scope})
	if err != nil {
		t.Fatalf("Paginate after delete/insert: %v", err)
	}
	if got := next.Items; len(got) != 2 || got[0] != "Bea/u-4" || got[1] != "Lin/u-3" || !next.PageInfo.HasPreviousPage {
		t.Fatalf("next page = %#v", next)
	}

	last := uint64(2)
	before, err := codec.Encode(scope, runtime.CursorBackward, runtime.CursorPosition{SortKey: "Lin", TieBreaker: "u-3"})
	if err != nil {
		t.Fatalf("Encode backward boundary: %v", err)
	}
	backward, err := runtime.Paginate(codec, changed, runtime.PageRequest{Last: &last, Before: before, Scope: scope})
	if err != nil {
		t.Fatalf("Paginate backward: %v", err)
	}
	if got := backward.Items; len(got) != 2 || got[0] != "Ada/u-1" || got[1] != "Bea/u-4" || !backward.PageInfo.HasNextPage {
		t.Fatalf("backward page = %#v", backward)
	}
}

func TestPaginateRejectsInvalidArgumentsAndUnstableOrdering(t *testing.T) {
	t.Parallel()
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "k1", Keys: map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, MaxPageSize: 2,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scope := withCursorScope(t, "users", `{}`, `[]`, "", "", runtime.LivePagination, "")
	one, two, three := uint64(1), uint64(2), uint64(3)
	for _, request := range []runtime.PageRequest{
		{Scope: scope},
		{First: &one, Last: &one, Scope: scope},
		{First: &one, Before: "cursor", Scope: scope},
		{Last: &one, After: "cursor", Scope: scope},
		{First: &three, Scope: scope},
		{First: new(uint64), Scope: scope},
	} {
		if _, err := runtime.Paginate(codec, []runtime.PageEntry[string]{{Item: "a", Position: runtime.CursorPosition{SortKey: "a", TieBreaker: "1"}}}, request); !errors.Is(err, runtime.ErrInvalidPage) {
			t.Fatalf("request %#v error = %v, want ErrInvalidPage", request, err)
		}
	}
	entries := []runtime.PageEntry[string]{
		{Item: "b", Position: runtime.CursorPosition{SortKey: "b", TieBreaker: "1"}},
		{Item: "a", Position: runtime.CursorPosition{SortKey: "a", TieBreaker: "1"}},
	}
	if _, err := runtime.Paginate(codec, entries, runtime.PageRequest{First: &two, Scope: scope}); !errors.Is(err, runtime.ErrUnstableCollection) {
		t.Fatalf("unstable order error = %v, want ErrUnstableCollection", err)
	}
	if _, err := runtime.Paginate(codec, entries[:1], runtime.PageRequest{First: &one}); !errors.Is(err, runtime.ErrInvalidPage) {
		t.Fatalf("zero scope error = %v, want ErrInvalidPage", err)
	}
}

func TestPaginateHandlesMaximumPageSizeWithoutIntegerOverflow(t *testing.T) {
	t.Parallel()
	maximum := uint64(^uint(0) >> 1)
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "k1", Keys: map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, MaxPageSize: maximum,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scope := withCursorScope(t, "users", `{}`, `[]`, "", "", runtime.LivePagination, "")
	entries := []runtime.PageEntry[string]{
		{Item: "a", Position: runtime.CursorPosition{SortKey: "a", TieBreaker: "1"}},
		{Item: "b", Position: runtime.CursorPosition{SortKey: "b", TieBreaker: "2"}},
	}
	after, err := codec.Encode(scope, runtime.CursorForward, entries[0].Position)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	page, err := runtime.Paginate(codec, entries, runtime.PageRequest{First: &maximum, After: after, Scope: scope})
	if err != nil || !slices.Equal(page.Items, []string{"b"}) {
		t.Fatalf("Paginate = %#v, %v", page, err)
	}
}

func TestCursorCodecRejectsUnallocatablePageMaximum(t *testing.T) {
	t.Parallel()
	_, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "k1", Keys: map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, MaxPageSize: ^uint64(0),
	})
	if err == nil {
		t.Fatal("NewCursorCodec accepted an unallocatable maximum page size")
	}
}

func TestExecutePreflightsScopedCursorAndPresentsCollectionPage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "k1", Keys: map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, Now: func() time.Time { return now }, MaxPageSize: 2,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	scopeFor := func(ctx context.Context) (runtime.CursorScope, error) {
		principal, _ := runtime.PrincipalFromContext(ctx)
		return runtime.NewCursorScope("users", []byte(`{"status":"active"}`), []byte(`[{"field":"name"},{"field":"id"}]`), runtime.CursorScopeOptions{
			Tenant: principal.Tenant, Authorization: principal.AuthorizationRevision, SnapshotPolicy: runtime.LivePagination,
		})
	}
	tenantA, err := scopeFor(runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", AuthorizationRevision: "policy-1"}))
	if err != nil {
		t.Fatalf("scopeFor tenant A: %v", err)
	}
	cursor, err := codec.Encode(tenantA, runtime.CursorForward, runtime.CursorPosition{SortKey: "Ada", TieBreaker: "u-1"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	collection := &runtime.CollectionMetadata{
		MaxPageSize: 2, CursorCodec: codec, CursorScope: scopeFor,
		Position: func(item any) (runtime.CursorPosition, error) {
			user, ok := item.(map[string]any)
			if !ok {
				return runtime.CursorPosition{}, errors.New("unexpected collection item")
			}
			name, ok := user["name"].(string)
			if !ok {
				return runtime.CursorPosition{}, errors.New("collection item has no stable name")
			}
			return runtime.CursorPosition{SortKey: name, TieBreaker: name}, nil
		},
	}
	snapshot, rootCalls, _ := collectionResourceSnapshotWithCost(t, 0, collection)
	requestJSON := fmt.Sprintf(`{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"usersPage","first":1,"after":{"$literal":%q},"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`, cursor)
	request, err := protocol.DecodeRequest([]byte(requestJSON), protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tenantBContext := runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-b", AuthorizationRevision: "policy-1"})
	invalid := plan.Execute(tenantBContext)
	if rootCalls.Load() != 0 || !hasExecutionCode(invalid.Errors, runtime.CodeInvalidCursor) {
		t.Fatalf("cross-tenant execution calls=%d outcome=%#v", rootCalls.Load(), invalid)
	}
	if len(invalid.Errors) != 1 || !slices.Equal(invalid.Errors[0].Path, []any{"users", "usersPage"}) {
		t.Fatalf("cross-tenant cursor path = %#v", invalid.Errors)
	}

	valid := plan.Execute(runtime.WithPrincipal(context.Background(), runtime.Principal{Tenant: "tenant-a", AuthorizationRevision: "policy-1"}))
	if len(valid.Errors) != 0 || rootCalls.Load() != 1 {
		t.Fatalf("valid execution calls=%d outcome=%#v", rootCalls.Load(), valid)
	}
	users := valid.Data["users"].(map[string]any)
	page := users["usersPage"].(map[string]any)
	items := page["items"].([]any)
	info := page["pageInfo"].(runtime.PageInfo)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "Grace" || !info.HasPreviousPage || !info.HasNextPage || info.StartCursor == "" || info.EndCursor == "" {
		t.Fatalf("presented page = %#v", page)
	}
}

func TestExecutePresentsStandardPageWithoutInputCursor(t *testing.T) {
	t.Parallel()
	collection := secureCollectionMetadata(t, 2, 0)
	snapshot, rootCalls, _ := collectionResourceSnapshotWithCost(t, 0, collection)
	request := decodeRuntimeRequestWithOptions(t, `{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"page","first":2,"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`, protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || rootCalls.Load() != 1 {
		t.Fatalf("Execute calls=%d outcome=%#v", rootCalls.Load(), outcome)
	}
	page, ok := outcome.Data["users"].(map[string]any)["page"].(map[string]any)
	if !ok {
		t.Fatalf("page presentation = %#v, want object", outcome.Data)
	}
	items, itemsOK := page["items"].([]any)
	info, infoOK := page["pageInfo"].(runtime.PageInfo)
	if !itemsOK || len(items) != 2 || !infoOK || !info.HasNextPage || info.StartCursor == "" || info.EndCursor == "" {
		t.Fatalf("page presentation = %#v", page)
	}
}

func TestExecuteReusesPreflightCursorScopeAfterHandlersStart(t *testing.T) {
	t.Parallel()
	collection := secureCollectionMetadata(t, 2, 0)
	scope, err := runtime.NewCursorScope("users", []byte(`{}`), []byte(`[]`), runtime.CursorScopeOptions{})
	if err != nil {
		t.Fatalf("NewCursorScope: %v", err)
	}
	after, err := collection.CursorCodec.Encode(scope, runtime.CursorForward, runtime.CursorPosition{SortKey: "Ada", TieBreaker: "Ada"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	scopeCalls := 0
	collection.CursorScope = func(context.Context) (runtime.CursorScope, error) {
		scopeCalls++
		if scopeCalls > 1 {
			return runtime.CursorScope{}, errors.New("scope changed after preflight")
		}
		return scope, nil
	}
	snapshot, rootCalls, _ := collectionResourceSnapshotWithCost(t, 0, collection)
	requestJSON := fmt.Sprintf(`{"version":"1","capabilities":["collection.page-1"],"document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"users","select":[{"$page":{"as":"page","first":1,"after":{"$literal":%q},"select":[{"$field":{"name":"name"}}]}}]}}]}]}}`, after)
	request, err := protocol.DecodeRequest([]byte(requestJSON), protocol.DecodeOptions{Capabilities: map[string]bool{"collection.page-1": true}})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	outcome := plan.Execute(context.Background())
	if len(outcome.Errors) != 0 || rootCalls.Load() != 1 || scopeCalls != 1 {
		t.Fatalf("Execute scopeCalls=%d rootCalls=%d outcome=%#v", scopeCalls, rootCalls.Load(), outcome)
	}
}

func TestRegistryRejectsUnsafeCollectionMetadata(t *testing.T) {
	t.Parallel()
	codec, err := runtime.NewCursorCodec(runtime.CursorCodecConfig{
		ActiveKeyID: "k1", Keys: map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")},
		TTL: time.Hour, MaxPageSize: 2,
	})
	if err != nil {
		t.Fatalf("NewCursorCodec: %v", err)
	}
	validScope := func(context.Context) (runtime.CursorScope, error) {
		return runtime.NewCursorScope("users", []byte(`{}`), []byte(`[]`), runtime.CursorScopeOptions{})
	}
	validPosition := func(any) (runtime.CursorPosition, error) {
		return runtime.CursorPosition{SortKey: "a", TieBreaker: "1"}, nil
	}
	tests := []struct {
		name       string
		output     schema.TypeID
		collection runtime.CollectionMetadata
	}{
		{"zero maximum", "Users", runtime.CollectionMetadata{}},
		{"partial cursor runtime", "Users", runtime.CollectionMetadata{MaxPageSize: 2, CursorCodec: codec}},
		{"codec maximum mismatch", "Users", runtime.CollectionMetadata{MaxPageSize: 3, CursorCodec: codec, CursorScope: validScope, Position: validPosition}},
		{"non-collection output", schema.TypeID(schema.String), runtime.CollectionMetadata{MaxPageSize: 2}},
		{"total count cost above portable maximum", "Users", runtime.CollectionMetadata{MaxPageSize: 2, TotalCountCost: schema.MaxDirectiveCost + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := runtime.NewRegistry(compositionTypes(t))
			metadata := completeMetadata(runtime.ReadEffect)
			metadata.Collection = &test.collection
			definition := runtime.BindInvocation[[]map[string]any](runtime.Descriptor{
				Name: "unsafe", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
				Input: schema.TypeID(schema.String), Output: test.output, Metadata: metadata,
			}, func(context.Context, runtime.Invocation) ([]map[string]any, error) { return nil, nil })
			if err := registry.Register(definition); err == nil || !strings.Contains(err.Error(), "collection") {
				t.Fatalf("Register error = %v, want collection metadata rejection", err)
			}
		})
	}
}

func withCursorScope(t *testing.T, collection, filters, sort, tenant, authorization string, policy runtime.PaginationConsistency, snapshot string) runtime.CursorScope {
	t.Helper()
	scope, err := runtime.NewCursorScope(collection, []byte(filters), []byte(sort), runtime.CursorScopeOptions{
		Tenant: tenant, Authorization: authorization, SnapshotPolicy: policy, Snapshot: snapshot,
	})
	if err != nil {
		t.Fatalf("NewCursorScope: %v", err)
	}
	return scope
}
