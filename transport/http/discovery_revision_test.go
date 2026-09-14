package http_test

import (
	"context"
	"errors"
	"testing"

	transporthttp "github.com/valksor/naatre/transport/http"
)

func TestRevisionStoreBoundsHistoryAndSupportsRollback(t *testing.T) {
	t.Parallel()
	store, err := transporthttp.NewRevisionStore(2)
	if err != nil {
		t.Fatal(err)
	}
	first := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	second := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r2","types":[],"operations":[],"members":[]}`)
	third := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r3","types":[],"operations":[],"members":[]}`)
	if err := store.Install(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(second); err != nil {
		t.Fatal(err)
	}
	if err := store.Rollback("schema-r1"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Current(context.Background())
	if err != nil || current.Revision() != "schema-r1" {
		t.Fatalf("rollback current = %q, %v", current.Revision(), err)
	}
	if err := store.Install(third); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Lookup(context.Background(), "schema-r1"); err != nil || found {
		t.Fatalf("evicted revision found=%v err=%v", found, err)
	}
	if _, found, err := store.Lookup(context.Background(), "schema-r2"); err != nil || !found {
		t.Fatalf("retained revision found=%v err=%v", found, err)
	}
	if err := store.Rollback("schema-r1"); !errors.Is(err, transporthttp.ErrDiscoveryRevisionNotFound) {
		t.Fatalf("evicted rollback error = %v", err)
	}
}

func TestRevisionStoreRejectsInvalidCapacityEmptyStateAndRevisionReuse(t *testing.T) {
	t.Parallel()
	if store, err := transporthttp.NewRevisionStore(0); err == nil || store != nil {
		t.Fatalf("zero-capacity store = %#v, %v", store, err)
	}
	store, err := transporthttp.NewRevisionStore(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current(context.Background()); !errors.Is(err, transporthttp.ErrNoDiscoveryRevision) {
		t.Fatalf("empty current error = %v", err)
	}
	first := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[],"operations":[],"members":[]}`)
	conflict := discoveryDocument(t, `{"version":"1","canonicalVersion":"c14n-1","revision":"schema-r1","types":[{"id":"Value","name":"Value","kind":"object","output":true,"fields":[{"id":"Value.id","name":"id","type":"ID"}]}],"operations":[],"members":[]}`)
	if err := store.Install(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(first); err != nil {
		t.Fatalf("idempotent install: %v", err)
	}
	if err := store.Install(conflict); !errors.Is(err, transporthttp.ErrDiscoveryRevisionConflict) {
		t.Fatalf("conflicting revision error = %v", err)
	} else {
		var discoveryErr *transporthttp.DiscoveryError
		if !errors.As(err, &discoveryErr) || discoveryErr.Code() != transporthttp.CodeDiscoveryRevisionConflict {
			t.Fatalf("conflicting revision code = %#v", discoveryErr)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Current(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled current error = %v", err)
	}
}
