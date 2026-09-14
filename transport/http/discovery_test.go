package http_test

import (
	"context"
	"encoding/json"
	"errors"
	iohttp "net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestDiscoveryDefaultsOffWithoutCallingApplicationCode(t *testing.T) {
	t.Parallel()
	handler, err := transporthttp.NewDiscoveryHandler(transporthttp.DiscoveryConfig{
		Authenticate: func(context.Context, *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
			panic("disabled discovery authenticated a request")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "")
	assertDiscoveryProblem(t, response, iohttp.StatusNotFound, transporthttp.CodeDiscoveryDisabled)
}

func TestDiscoveryRejectsRevisionLimitAbovePortableSchemaBoundary(t *testing.T) {
	t.Parallel()
	document := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	config := validDiscoveryConfig(discoveryStore(t, document), allowMigrationDiscovery)
	config.Limits.MaxRevisionBytes = transporthttp.DefaultDiscoveryLimits().MaxRevisionBytes + 1
	if handler, err := transporthttp.NewDiscoveryHandler(config); err == nil || handler != nil {
		t.Fatalf("over-limit revision configuration = %#v, %v", handler, err)
	}
}

func TestDiscoveryReturnsReferentiallyCompleteFilteredSchemaWithoutHiddenNames(t *testing.T) {
	t.Parallel()
	store := discoveryStore(t, discoveryDocument(t, `{
		"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1",
		"types":[
			{"id":"Secret","name":"Secret","kind":"object","output":true,"fields":[{"id":"Secret.value","name":"credentialValue","type":"String"}]},
			{"id":"Public","name":"Public","kind":"object","output":true,"fields":[{"id":"Public.id","name":"id","type":"ID"},{"id":"Public.secret","name":"protectedMetadata","type":"Secret"}]}
		],
		"operations":[{"id":"query.public","name":"public","kind":"query","output":"Public","effect":"read"}],
		"members":[]
	}`))
	handler := discoveryHandler(t, store, func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
		return transporthttp.DiscoveryDecision{
			PolicyRevision: "policy-r1",
			Visibility: schema.Visibility{
				Types:      map[schema.TypeID]bool{"Public": true},
				Fields:     map[string]bool{"Public.id": true, "Public.secret": true},
				Operations: map[string]bool{"query.public": true},
			},
		}, nil
	}, transporthttp.DiscoveryLimits{})

	response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer private-credential")
	if response.Code != iohttp.StatusOK || response.Header().Get("Content-Type") != transporthttp.DiscoverySchemaMediaType {
		t.Fatalf("discovery response = %d %q: %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	for _, hidden := range []string{"Secret", "credentialValue", "protectedMetadata", "private-credential"} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Fatalf("filtered discovery leaked %q: %s", hidden, response.Body.String())
		}
	}
	filtered, err := schema.ParseDocument(response.Body.Bytes(), schema.ImportOptions{})
	if err != nil {
		t.Fatalf("parse filtered document: %v", err)
	}
	if _, err := filtered.Snapshot(); err != nil {
		t.Fatalf("filtered discovery has dangling references: %v", err)
	}
	if len(filtered.Types()) != 1 || len(filtered.Types()[0].Fields) != 1 || filtered.Types()[0].Fields[0].ID != "Public.id" {
		t.Fatalf("filtered types = %#v", filtered.Types())
	}
	if response.Header().Get("ETag") == "" || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("discovery cache headers = %#v", response.Header())
	}
}

func TestDiscoveryMigrationReportUsesFilteredCoreDiffAcrossRollingRevisions(t *testing.T) {
	t.Parallel()
	before := discoveryMigrationDocument(t, "schema-r1", false)
	after := discoveryMigrationDocument(t, "schema-r2", true)
	store := discoveryStore(t, before, after)
	handler := discoveryHandler(t, store, allowMigrationDiscovery, transporthttp.DiscoveryLimits{})

	response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryDiffPath+"?from=schema-r1", "Bearer authorized")
	if response.Code != iohttp.StatusOK || response.Header().Get("Content-Type") != transporthttp.DiscoveryMigrationMediaType {
		t.Fatalf("migration response = %d %q: %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var report transporthttp.MigrationReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Profile != transporthttp.DiscoveryProfile || report.Diff.BeforeRevision != "schema-r1" || report.Diff.AfterRevision != "schema-r2" || report.Diff.Classification != schema.ChangeBreaking {
		t.Fatalf("migration report = %#v", report)
	}
	want := map[string]schema.ChangeClassification{
		"type:Removed":                schema.ChangeBreaking,
		"field:Input.scope":           schema.ChangeBreaking,
		"field:User.name/nullable":    schema.ChangeBreaking,
		"type:Role/open":              schema.ChangeBreaking,
		"field:Input.id/default":      schema.ChangeBehaviorOnly,
		"operation:query.user/effect": schema.ChangeDangerous,
		"type:User/maxDepth":          schema.ChangeDangerous,
	}
	for path, classification := range want {
		if !slices.ContainsFunc(report.Diff.Changes, func(change schema.SchemaChange) bool {
			return change.Path == path && change.Classification == classification
		}) {
			t.Errorf("migration report missing %s=%s: %#v", path, classification, report.Diff.Changes)
		}
	}
	for _, protected := range []string{"Secret", "internalOnly", "private-credential"} {
		if strings.Contains(response.Body.String(), protected) {
			t.Fatalf("migration report leaked %q: %s", protected, response.Body.String())
		}
	}

	if err := store.Rollback("schema-r1"); err != nil {
		t.Fatal(err)
	}
	rolledBack := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer authorized")
	if rolledBack.Code != iohttp.StatusOK || !strings.Contains(rolledBack.Body.String(), `"revision":"schema-r1"`) || strings.Contains(rolledBack.Body.String(), `schema-r2`) {
		t.Fatalf("rolled-back discovery = %d %s", rolledBack.Code, rolledBack.Body.String())
	}
}

func TestDiscoveryFailuresUseStableCodesAndNeverExposePrivateCauses(t *testing.T) {
	t.Parallel()
	document := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	store := discoveryStore(t, document)
	secret := "Bearer local-private-credential"
	tests := []struct {
		name      string
		method    string
		target    string
		configure func(*transporthttp.DiscoveryConfig)
		status    int
		code      string
	}{
		{name: "method", method: iohttp.MethodPost, target: transporthttp.DiscoveryPath, status: iohttp.StatusMethodNotAllowed, code: transporthttp.CodeDiscoveryMethod},
		{name: "path", method: iohttp.MethodGet, target: "/v1/private-schema", status: iohttp.StatusNotFound, code: transporthttp.CodeDiscoveryNotFound},
		{name: "unknown method and path", method: iohttp.MethodPost, target: "/v1/private-schema", status: iohttp.StatusNotFound, code: transporthttp.CodeDiscoveryNotFound},
		{name: "unauthenticated", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Authenticate = func(context.Context, *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
				return transporthttp.DiscoveryIdentity{}, transporthttp.ErrDiscoveryUnauthenticated
			}
		}, status: iohttp.StatusUnauthorized, code: transporthttp.CodeDiscoveryUnauthenticated},
		{name: "authentication backend", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Authenticate = func(context.Context, *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
				return transporthttp.DiscoveryIdentity{}, errors.New("token database exposed " + secret)
			}
		}, status: iohttp.StatusInternalServerError, code: transporthttp.CodeDiscoveryInternal},
		{name: "forbidden", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Authorize = func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
				return transporthttp.DiscoveryDecision{}, transporthttp.ErrDiscoveryForbidden
			}
		}, status: iohttp.StatusForbidden, code: transporthttp.CodeDiscoveryForbidden},
		{name: "authorization backend", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Authorize = func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
				return transporthttp.DiscoveryDecision{}, errors.New("policy=protected-admin-rule")
			}
		}, status: iohttp.StatusInternalServerError, code: transporthttp.CodeDiscoveryInternal},
		{name: "source backend", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Source = failingDiscoverySource{err: errors.New("storage=/private/schema-store")}
		}, status: iohttp.StatusInternalServerError, code: transporthttp.CodeDiscoveryInternal},
		{name: "missing policy revision", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Authorize = func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
				return transporthttp.DiscoveryDecision{}, nil
			}
		}, status: iohttp.StatusForbidden, code: transporthttp.CodeDiscoveryForbidden},
		{name: "invalid query", method: iohttp.MethodGet, target: transporthttp.DiscoveryDiffPath + "?from=schema-r1&extra=secret", status: iohttp.StatusBadRequest, code: transporthttp.CodeDiscoveryBadRequest},
		{name: "duplicate revision", method: iohttp.MethodGet, target: transporthttp.DiscoveryDiffPath + "?from=schema-r1&from=schema-r1", status: iohttp.StatusBadRequest, code: transporthttp.CodeDiscoveryBadRequest},
		{name: "missing revision", method: iohttp.MethodGet, target: transporthttp.DiscoveryDiffPath + "?from=schema-missing", status: iohttp.StatusNotFound, code: transporthttp.CodeDiscoveryRevisionMissing},
		{name: "panic", method: iohttp.MethodGet, target: transporthttp.DiscoveryPath, configure: func(config *transporthttp.DiscoveryConfig) {
			config.Authorize = func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
				panic("implementation package /private/path")
			}
		}, status: iohttp.StatusInternalServerError, code: transporthttp.CodeDiscoveryInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validDiscoveryConfig(store, allowMigrationDiscovery)
			if test.configure != nil {
				test.configure(&config)
			}
			handler, err := transporthttp.NewDiscoveryHandler(config)
			if err != nil {
				t.Fatal(err)
			}
			response := requestDiscovery(handler, test.method, test.target, secret)
			assertDiscoveryProblem(t, response, test.status, test.code)
			if test.code == transporthttp.CodeDiscoveryUnauthenticated && response.Header().Get("WWW-Authenticate") != config.AuthenticationChallenge {
				t.Fatalf("authentication challenge = %q", response.Header().Get("WWW-Authenticate"))
			}
			for _, protected := range []string{secret, "protected-admin-rule", "/private/path", "/private/schema-store", "schema-missing", "extra"} {
				if strings.Contains(response.Body.String(), protected) {
					t.Fatalf("failure leaked %q: %s", protected, response.Body.String())
				}
			}
		})
	}
}

func TestDiscoveryPinsOneRevisionAndAuthorizationDecisionPerRequest(t *testing.T) {
	t.Parallel()
	first := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	second := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r2","types":[],"operations":[],"members":[]}`)
	store := discoveryStore(t, first)
	authorizing := make(chan string, 1)
	release := make(chan struct{})
	handler := discoveryHandler(t, store, func(ctx context.Context, _ transporthttp.DiscoveryIdentity, revision string) (transporthttp.DiscoveryDecision, error) {
		authorizing <- revision
		select {
		case <-release:
			return transporthttp.DiscoveryDecision{PolicyRevision: "policy-r1", Visibility: schema.Visibility{}}, nil
		case <-ctx.Done():
			return transporthttp.DiscoveryDecision{}, ctx.Err()
		}
	}, transporthttp.DiscoveryLimits{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer authorized")
	}()
	if revision := <-authorizing; revision != "schema-r1" {
		t.Fatalf("authorized revision = %q", revision)
	}
	if err := store.Install(second); err != nil {
		t.Fatal(err)
	}
	close(release)
	response := <-done
	if response.Code != iohttp.StatusOK || !strings.Contains(response.Body.String(), `"revision":"schema-r1"`) || strings.Contains(response.Body.String(), "schema-r2") {
		t.Fatalf("mixed rolling response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiscoveryCacheIdentityIncludesAuthorizationDimensions(t *testing.T) {
	t.Parallel()
	document := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	store := discoveryStore(t, document)
	expectedBody, err := document.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		token    string
		identity transporthttp.DiscoveryIdentity
		policy   string
		repeatOf string
	}{
		{name: "base", token: "base", identity: transporthttp.DiscoveryIdentity{Tenant: "tenant-a", Principal: "principal-a"}, policy: "policy-a"},
		{name: "tenant", token: "tenant", identity: transporthttp.DiscoveryIdentity{Tenant: "tenant-b", Principal: "principal-a"}, policy: "policy-a"},
		{name: "principal", token: "principal", identity: transporthttp.DiscoveryIdentity{Tenant: "tenant-a", Principal: "principal-b"}, policy: "policy-a"},
		{name: "policy", token: "base", identity: transporthttp.DiscoveryIdentity{Tenant: "tenant-a", Principal: "principal-a"}, policy: "policy-b"},
		{name: "nul-principal", token: "nul-principal", identity: transporthttp.DiscoveryIdentity{Tenant: "a", Principal: "b\x00c"}, policy: "policy-a"},
		{name: "nul-tenant", token: "nul-tenant", identity: transporthttp.DiscoveryIdentity{Tenant: "a\x00b", Principal: "c"}, policy: "policy-a"},
		{name: "repeat-base", token: "base", identity: transporthttp.DiscoveryIdentity{Tenant: "tenant-a", Principal: "principal-a"}, policy: "policy-a", repeatOf: "base"},
	}
	identities := make(map[string]transporthttp.DiscoveryIdentity, len(tests))
	for _, test := range tests {
		identities[test.token] = test.identity
	}
	currentPolicy := ""
	config := validDiscoveryConfig(store, func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
		return transporthttp.DiscoveryDecision{PolicyRevision: currentPolicy, Visibility: schema.Visibility{}}, nil
	})
	config.Authenticate = func(_ context.Context, request *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
		identity, found := identities[strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")]
		if !found {
			return transporthttp.DiscoveryIdentity{}, transporthttp.ErrDiscoveryUnauthenticated
		}
		return identity, nil
	}
	handler, err := transporthttp.NewDiscoveryHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	etagsByName := make(map[string]string, len(tests))
	etagOwners := make(map[string]string, len(tests))
	for _, test := range tests {
		currentPolicy = test.policy
		response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer "+test.token)
		if response.Code != iohttp.StatusOK || response.Header().Get("ETag") == "" {
			t.Fatalf("%s response = %d, etag %q", test.name, response.Code, response.Header().Get("ETag"))
		}
		if response.Body.String() != string(expectedBody) {
			t.Fatalf("%s body exposed request-specific state: %s", test.name, response.Body.String())
		}
		for _, private := range []string{test.identity.Tenant, test.identity.Principal, test.policy} {
			if len(private) >= 4 && strings.Contains(response.Header().Get("ETag"), private) {
				t.Fatalf("%s cache response exposed %q", test.name, private)
			}
		}
		if test.repeatOf != "" {
			if response.Header().Get("ETag") != etagsByName[test.repeatOf] {
				t.Fatalf("%s cache identity = %q, want %q", test.name, response.Header().Get("ETag"), etagsByName[test.repeatOf])
			}
			continue
		}
		if previous, found := etagOwners[response.Header().Get("ETag")]; found {
			t.Fatalf("%s and %s share cache identity %q", previous, test.name, response.Header().Get("ETag"))
		}
		etagsByName[test.name] = response.Header().Get("ETag")
		etagOwners[response.Header().Get("ETag")] = test.name
	}
}

func TestDiscoverySnapshotsVisibilityDecisionForMigration(t *testing.T) {
	t.Parallel()
	store := discoveryStore(t, discoveryMigrationDocument(t, "schema-r1", false), discoveryMigrationDocument(t, "schema-r2", true))
	visibility := allowMigrationVisibility()
	source := mutatingLookupSource{RevisionStore: store, mutate: func() {
		visibility.Types["Secret"] = true
		visibility.Fields["Secret.value"] = true
	}}
	handler := discoveryHandler(t, source, func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
		return transporthttp.DiscoveryDecision{PolicyRevision: "policy-r1", Visibility: visibility}, nil
	}, transporthttp.DiscoveryLimits{})

	response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryDiffPath+"?from=schema-r1", "Bearer authorized")
	if response.Code != iohttp.StatusOK {
		t.Fatalf("migration response = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "Secret") || strings.Contains(response.Body.String(), "internalOnly") {
		t.Fatalf("migration used a mutated authorization decision: %s", response.Body.String())
	}
}

func TestDiscoveryCancellationAndResourceLimits(t *testing.T) {
	t.Parallel()
	document := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	store := discoveryStore(t, document)

	t.Run("authentication cancellation", func(t *testing.T) {
		started := make(chan struct{})
		config := validDiscoveryConfig(store, allowMigrationDiscovery)
		config.Authenticate = func(ctx context.Context, _ *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
			close(started)
			<-ctx.Done()
			return transporthttp.DiscoveryIdentity{}, ctx.Err()
		}
		assertActiveDiscoveryCancellation(t, config, transporthttp.DiscoveryPath, started)
	})

	t.Run("current source cancellation", func(t *testing.T) {
		started := make(chan struct{})
		config := validDiscoveryConfig(discoverySourceFuncs{
			current: func(ctx context.Context) (schema.Document, error) {
				close(started)
				<-ctx.Done()
				return document, nil
			},
		}, allowMigrationDiscovery)
		assertActiveDiscoveryCancellation(t, config, transporthttp.DiscoveryPath, started)
	})

	t.Run("authorization cancellation", func(t *testing.T) {
		started := make(chan struct{})
		config := validDiscoveryConfig(store, func(ctx context.Context, _ transporthttp.DiscoveryIdentity, _ string) (transporthttp.DiscoveryDecision, error) {
			close(started)
			<-ctx.Done()
			return transporthttp.DiscoveryDecision{PolicyRevision: "policy-r1", Visibility: schema.Visibility{}}, nil
		})
		assertActiveDiscoveryCancellation(t, config, transporthttp.DiscoveryPath, started)
	})

	t.Run("revision lookup cancellation", func(t *testing.T) {
		started := make(chan struct{})
		config := validDiscoveryConfig(discoverySourceFuncs{
			current: store.Current,
			lookup: func(ctx context.Context, revision string) (schema.Document, bool, error) {
				before, found, err := store.Lookup(context.Background(), revision)
				close(started)
				<-ctx.Done()
				return before, found, err
			},
		}, allowMigrationDiscovery)
		assertActiveDiscoveryCancellation(t, config, transporthttp.DiscoveryDiffPath+"?from=schema-r1", started)
	})

	t.Run("concurrency", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		config := validDiscoveryConfig(store, allowMigrationDiscovery)
		config.Limits.MaxConcurrent = 1
		config.Authenticate = func(ctx context.Context, _ *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return transporthttp.DiscoveryIdentity{Tenant: "tenant", Principal: "principal"}, nil
			case <-ctx.Done():
				return transporthttp.DiscoveryIdentity{}, ctx.Err()
			}
		}
		handler, err := transporthttp.NewDiscoveryHandler(config)
		if err != nil {
			t.Fatal(err)
		}
		firstDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			firstDone <- requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer one")
		}()
		<-entered
		busy := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer two")
		assertDiscoveryProblem(t, busy, iohttp.StatusServiceUnavailable, transporthttp.CodeDiscoveryBusy)
		close(release)
		if first := <-firstDone; first.Code != iohttp.StatusOK {
			t.Fatalf("first bounded request = %d %s", first.Code, first.Body.String())
		}
	})

	t.Run("response bytes", func(t *testing.T) {
		handler := discoveryHandler(t, store, allowMigrationDiscovery, transporthttp.DiscoveryLimits{MaxResponseBytes: 1})
		response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer authorized")
		assertDiscoveryProblem(t, response, iohttp.StatusInternalServerError, transporthttp.CodeDiscoveryLimit)
	})

	t.Run("document bytes", func(t *testing.T) {
		authorized := false
		config := validDiscoveryConfig(store, func(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
			authorized = true
			return transporthttp.DiscoveryDecision{PolicyRevision: "policy-r1", Visibility: schema.Visibility{}}, nil
		})
		config.Limits.MaxDocumentBytes = 1
		handler, err := transporthttp.NewDiscoveryHandler(config)
		if err != nil {
			t.Fatal(err)
		}
		response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer authorized")
		assertDiscoveryProblem(t, response, iohttp.StatusInternalServerError, transporthttp.CodeDiscoveryLimit)
		if !authorized {
			t.Fatal("document size was exposed before authorization")
		}
	})

	t.Run("current revision bytes", func(t *testing.T) {
		handler := discoveryHandler(t, store, allowMigrationDiscovery, transporthttp.DiscoveryLimits{MaxRevisionBytes: len("schema-r1") - 1})
		response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryPath, "Bearer authorized")
		assertDiscoveryProblem(t, response, iohttp.StatusInternalServerError, transporthttp.CodeDiscoveryInternal)
	})

	t.Run("from revision bytes", func(t *testing.T) {
		handler := discoveryHandler(t, store, allowMigrationDiscovery, transporthttp.DiscoveryLimits{MaxRevisionBytes: len("schema-r1")})
		response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryDiffPath+"?from=schema-r100", "Bearer authorized")
		assertDiscoveryProblem(t, response, iohttp.StatusBadRequest, transporthttp.CodeDiscoveryBadRequest)
	})

	t.Run("migration changes", func(t *testing.T) {
		rolling := discoveryStore(t, discoveryMigrationDocument(t, "schema-r1", false), discoveryMigrationDocument(t, "schema-r2", true))
		handler := discoveryHandler(t, rolling, allowMigrationDiscovery, transporthttp.DiscoveryLimits{MaxMigrationChanges: 1})
		response := requestDiscovery(handler, iohttp.MethodGet, transporthttp.DiscoveryDiffPath+"?from=schema-r1", "Bearer authorized")
		assertDiscoveryProblem(t, response, iohttp.StatusInternalServerError, transporthttp.CodeDiscoveryLimit)
	})
}

func allowMigrationDiscovery(context.Context, transporthttp.DiscoveryIdentity, string) (transporthttp.DiscoveryDecision, error) {
	return transporthttp.DiscoveryDecision{
		PolicyRevision: "policy-r1",
		Visibility:     allowMigrationVisibility(),
	}, nil
}

func allowMigrationVisibility() schema.Visibility {
	return schema.Visibility{
		Types:      map[schema.TypeID]bool{"Input": true, "Role": true, "Removed": true, "User": true},
		Fields:     map[string]bool{"Input.id": true, "Input.scope": true, "Removed.id": true, "User.name": true},
		EnumValues: map[string]bool{"Role.USER": true},
		Operations: map[string]bool{"query.user": true},
	}
}

type mutatingLookupSource struct {
	*transporthttp.RevisionStore
	mutate func()
}

func (s mutatingLookupSource) Lookup(ctx context.Context, revision string) (schema.Document, bool, error) {
	s.mutate()
	return s.RevisionStore.Lookup(ctx, revision)
}

type failingDiscoverySource struct{ err error }

func (s failingDiscoverySource) Current(context.Context) (schema.Document, error) {
	return schema.Document{}, s.err
}

func (s failingDiscoverySource) Lookup(context.Context, string) (schema.Document, bool, error) {
	return schema.Document{}, false, s.err
}

type discoverySourceFuncs struct {
	current func(context.Context) (schema.Document, error)
	lookup  func(context.Context, string) (schema.Document, bool, error)
}

func (s discoverySourceFuncs) Current(ctx context.Context) (schema.Document, error) {
	return s.current(ctx)
}

func (s discoverySourceFuncs) Lookup(ctx context.Context, revision string) (schema.Document, bool, error) {
	if s.lookup == nil {
		return schema.Document{}, false, nil
	}
	return s.lookup(ctx, revision)
}

func validDiscoveryConfig(source transporthttp.DiscoverySource, authorize transporthttp.DiscoveryAuthorizer) transporthttp.DiscoveryConfig {
	return transporthttp.DiscoveryConfig{
		Enabled: true,
		Source:  source,
		Authenticate: func(context.Context, *iohttp.Request) (transporthttp.DiscoveryIdentity, error) {
			return transporthttp.DiscoveryIdentity{Tenant: "tenant-a", Principal: "principal-a"}, nil
		},
		Authorize:               authorize,
		AuthenticationChallenge: `Bearer realm="schema-discovery"`,
	}
}

func discoveryHandler(t *testing.T, source transporthttp.DiscoverySource, authorize transporthttp.DiscoveryAuthorizer, limits transporthttp.DiscoveryLimits) iohttp.Handler {
	t.Helper()
	config := validDiscoveryConfig(source, authorize)
	config.Limits = limits
	handler, err := transporthttp.NewDiscoveryHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func assertActiveDiscoveryCancellation(t *testing.T, config transporthttp.DiscoveryConfig, target string, started <-chan struct{}) {
	t.Helper()
	handler, err := transporthttp.NewDiscoveryHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(iohttp.MethodGet, target, nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(response, request); close(done) }()
	<-started
	cancel()
	<-done
	assertDiscoveryProblem(t, response, iohttp.StatusRequestTimeout, transporthttp.CodeDiscoveryCancelled)
}

func discoveryStore(t *testing.T, documents ...schema.Document) *transporthttp.RevisionStore {
	t.Helper()
	store, err := transporthttp.NewRevisionStore(8)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents {
		if err := store.Install(document); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func discoveryDocument(t *testing.T, input string) schema.Document {
	t.Helper()
	encoded := []byte(input)
	if !json.Valid(encoded) {
		t.Fatalf("discovery fixture is not JSON: %q", input)
	}
	document, err := schema.ParseDocument(encoded, schema.ImportOptions{})
	if err != nil {
		t.Fatalf("parse discovery document: %v", err)
	}
	if document.Revision() == "" {
		t.Fatal("discovery fixture has no revision")
	}
	return document
}

func discoveryMigrationDocument(t *testing.T, revision string, evolved bool) schema.Document {
	t.Helper()
	if !evolved {
		return discoveryDocument(t, strings.ReplaceAll(`{
			"version":"1","canonicalVersion":"c14n-1","revision":"REVISION",
			"types":[
				{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"ID"}]},
				{"id":"Role","name":"Role","kind":"enum","output":true,"open":true,"enumMembers":[{"id":"Role.USER","name":"USER"}]},
				{"id":"Removed","name":"Removed","kind":"object","output":true,"fields":[{"id":"Removed.id","name":"id","type":"ID"}]},
				{"id":"Secret","name":"Secret","kind":"object","output":true,"fields":[{"id":"Secret.value","name":"internalOnly","type":"String"}]},
				{"id":"User","name":"User","kind":"object","output":true,"maxDepth":4,"fields":[{"id":"User.name","name":"name","type":"String","nullable":true}]}
			],
			"operations":[{"id":"query.user","name":"user","kind":"query","input":"Input","output":"User","effect":"read"}],"members":[]
		}`, "REVISION", revision))
	}
	return discoveryDocument(t, strings.ReplaceAll(`{
		"version":"1","canonicalVersion":"c14n-1","revision":"REVISION",
		"types":[
			{"id":"Input","name":"Input","kind":"input-object","input":true,"fields":[{"id":"Input.id","name":"id","type":"ID","default":"anonymous"},{"id":"Input.scope","name":"scope","type":"String","required":true}]},
			{"id":"Role","name":"Role","kind":"enum","output":true,"open":false,"enumMembers":[{"id":"Role.USER","name":"USER"}]},
			{"id":"Secret","name":"Secret","kind":"object","output":true,"fields":[{"id":"Secret.value","name":"internalOnly","type":"String"}]},
			{"id":"User","name":"User","kind":"object","output":true,"maxDepth":2,"fields":[{"id":"User.name","name":"name","type":"String"}]}
		],
		"operations":[{"id":"query.user","name":"user","kind":"query","input":"Input","output":"User","effect":"write"}],"members":[]
	}`, "REVISION", revision))
}

func requestDiscovery(handler iohttp.Handler, method, target, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertDiscoveryProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("problem status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var problem struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v: %s", err, response.Body.String())
	}
	if problem.Status != status || problem.Code != code {
		t.Fatalf("problem = %#v, want %d/%s", problem, status, code)
	}
}
