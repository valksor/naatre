package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

// opaqueCause is the internal detail every failing fixture handler carries. No
// vector may let it reach the public response.
const opaqueCause = "opaque internal cause"

// fixtureHandler is the declared behaviour of one root call in a vector.
type fixtureHandler struct {
	Output json.RawMessage `json:"output"`
	Error  *struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		Retryable bool           `json:"retryable"`
		Details   map[string]any `json:"details"`
		Opaque    bool           `json:"opaque"`
	} `json:"error"`
}

// err renders the declared failure. An opaque failure must surface as the
// generic code with nothing disclosed.
func (h fixtureHandler) err() error {
	if h.Error == nil {
		return nil
	}
	if h.Error.Opaque || h.Error.Code == "" {
		return errors.New(opaqueCause)
	}
	return &runtime.Error{
		Code: h.Error.Code, Message: h.Error.Message, Retryable: h.Error.Retryable,
		Details: h.Error.Details, Cause: errors.New(opaqueCause),
	}
}

type partialResultVector struct {
	Name                  string                    `json:"name"`
	Document              json.RawMessage           `json:"document"`
	Handlers              map[string]fixtureHandler `json:"handlers"`
	CancelBeforeExecution bool                      `json:"cancelBeforeExecution"`
	ExpectedData          json.RawMessage           `json:"expectedData"`
	ExpectedErrors        []struct {
		Code      string         `json:"code"`
		Path      []any          `json:"path"`
		Retryable bool           `json:"retryable"`
		Details   map[string]any `json:"details"`
	} `json:"expectedErrors"`
	Complete bool `json:"complete"`
}

// The executor consumes the portable partial-result fixture as one client of a
// language-neutral contract, rather than as the definition of that contract.
func TestExecutorConsumesPortablePartialResultVectors(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "conformance", "v1", "partial-results.json"))
	if err != nil {
		t.Fatalf("read partial-results fixture: %v", err)
	}
	var fixture struct {
		Vectors []partialResultVector `json:"vectors"`
	}
	if err := json.Unmarshal(content, &fixture); err != nil {
		t.Fatalf("decode partial-results fixture: %v", err)
	}
	if len(fixture.Vectors) == 0 {
		t.Fatal("partial-results fixture declares no vectors")
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			outcome := runPartialResultVector(t, vector)
			assertPartialResultErrors(t, vector, outcome)
			assertCanonicalJSONEqual(t, outcome.Data, vector.ExpectedData)
			assertPartialResultCompleteness(t, vector, outcome)
		})
	}
}

func assertPartialResultCompleteness(t *testing.T, vector partialResultVector, outcome runtime.Outcome) {
	t.Helper()
	data, err := outcome.RequireComplete()
	if vector.Complete {
		if err != nil {
			t.Fatalf("RequireComplete on a complete outcome: %v", err)
		}
		if data == nil {
			t.Fatal("RequireComplete returned no data for a complete outcome")
		}
		return
	}
	if !errors.Is(err, runtime.ErrIncomplete) {
		t.Fatalf("RequireComplete err = %v, want ErrIncomplete", err)
	}
	if data != nil {
		t.Fatalf("RequireComplete returned data %#v for an incomplete outcome", data)
	}
	if bytes.Contains([]byte(err.Error()), []byte(opaqueCause)) {
		t.Fatalf("RequireComplete disclosed an internal cause: %v", err)
	}
}

func runPartialResultVector(t *testing.T, vector partialResultVector) runtime.Outcome {
	t.Helper()
	registry := runtime.NewRegistry(partialResultTypes(t))
	if err := registry.ConfigureAuthorization(runtime.AuthorizationConfig{Mode: runtime.AuthorizationAllowByDefault}); err != nil {
		t.Fatal(err)
	}
	registerPartialResultMembers(t, registry)
	registerPartialResultRoots(t, registry, vector)
	snapshot, err := registry.Freeze()
	if err != nil {
		t.Fatalf("freeze registry: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{
		"version": "1", "document": json.RawMessage(vector.Document),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := runtime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if vector.CancelBeforeExecution {
		cancel()
	}
	return plan.Execute(ctx)
}

func partialResultTypes(t *testing.T) schema.Snapshot {
	t.Helper()
	return freezeCompositionTypes(t, []schema.TypeDescriptor{
		{ID: "Profile", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"bio":      {Type: schema.TypeID(schema.String)},
			"nickname": {Type: schema.TypeID(schema.String)},
		}},
		{ID: "Person", Kind: schema.ObjectType, Output: true, Fields: map[string]schema.FieldDescriptor{
			"name":     {Type: schema.TypeID(schema.String), Required: true},
			"nickname": {Type: schema.TypeID(schema.String), Nullable: true},
			"profile":  {Type: "Profile"},
		}},
		{ID: "People", Kind: schema.ListType, Output: true, Element: "Person"},
	}...)
}

// registerPartialResultRoots binds the root calls the fixture declares. Only
// the roots a vector names are given its declared behaviour; the rest stay
// registered so every vector shares one schema.
func registerPartialResultRoots(t *testing.T, registry *runtime.Registry, vector partialResultVector) {
	t.Helper()
	person := vector.Handlers["person"]
	var personOutput map[string]any
	decodeFixtureJSON(t, person.Output, &personOutput)
	registerComposition(t, registry, runtime.BindInvocation[map[string]any](runtime.Descriptor{
		Name: "person", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "Person", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (map[string]any, error) {
		return personOutput, person.err()
	}))

	people := vector.Handlers["people"]
	var peopleOutput []any
	decodeFixtureJSON(t, people.Output, &peopleOutput)
	registerComposition(t, registry, runtime.BindInvocation[[]any](runtime.Descriptor{
		Name: "people", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: "People", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) ([]any, error) {
		return peopleOutput, people.err()
	}))

	other := vector.Handlers["other"]
	registerComposition(t, registry, runtime.BindInvocation[string](runtime.Descriptor{
		Name: "other", Scope: runtime.RootScope, Kind: protocol.Query, Member: runtime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String),
		Metadata: completeMetadata(runtime.ReadEffect),
	}, func(context.Context, runtime.Invocation) (string, error) {
		return "ok", other.err()
	}))
}

func registerPartialResultMembers(t *testing.T, registry *runtime.Registry) {
	t.Helper()
	registerSourceStringField(t, registry, "Person", "name", nil)
	registerSourceStringField(t, registry, "Profile", "nickname", nil)
	registerSourceStringField(t, registry, "Profile", "bio", nil)
	// nickname is nullable on Person, so it needs a presence-preserving reader
	// rather than the asserting helper used for non-null members.
	registerComposition(t, registry, runtime.BindField[map[string]any, *string](runtime.Descriptor{
		Name: "nickname", Scope: runtime.ObjectScope, Owner: "Person", Member: runtime.FieldMember,
		Output: schema.TypeID(schema.String), OutputNullable: true,
		Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (*string, error) {
		text, ok := source["nickname"].(string)
		if !ok {
			return nil, nil
		}
		return &text, nil
	}))
	registerComposition(t, registry, runtime.BindField[map[string]any, map[string]any](runtime.Descriptor{
		Name: "profile", Scope: runtime.ObjectScope, Owner: "Person", Member: runtime.FieldMember,
		Output: "Profile", Metadata: completeMetadata(runtime.ReadEffect),
	}, func(_ context.Context, source map[string]any) (map[string]any, error) {
		value, _ := source["profile"].(map[string]any)
		return value, nil
	}))
}

func assertPartialResultErrors(t *testing.T, vector partialResultVector, outcome runtime.Outcome) {
	t.Helper()
	if len(outcome.Errors) != len(vector.ExpectedErrors) {
		t.Fatalf("errors = %#v, want %#v", outcome.Errors, vector.ExpectedErrors)
	}
	for index, expected := range vector.ExpectedErrors {
		actual := outcome.Errors[index]
		if actual.Code != expected.Code || actual.Retryable != expected.Retryable {
			t.Fatalf("error %d = %q retryable=%v, want %q retryable=%v",
				index, actual.Code, actual.Retryable, expected.Code, expected.Retryable)
		}
		assertCanonicalJSONEqual(t, actual.Path, expected.Path)
		for key, want := range expected.Details {
			if actual.Details[key] != want {
				t.Fatalf("error %d details[%q] = %#v, want %#v", index, key, actual.Details[key], want)
			}
		}
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	if bytes.Contains(encoded, []byte(opaqueCause)) {
		t.Fatalf("serialized outcome disclosed an internal cause: %s", encoded)
	}
}

// assertCanonicalJSONEqual compares two values by their canonical encoding, so
// member order never decides the result. Either side may be a Go value or a
// json.RawMessage carrying fixture text.
func assertCanonicalJSONEqual(t *testing.T, actual, expected any) {
	t.Helper()
	canonical := func(label string, value any) []byte {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", label, err)
		}
		normalized, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{})
		if err != nil {
			t.Fatalf("canonicalize %s: %v", label, err)
		}
		return normalized
	}
	actualCanonical, expectedCanonical := canonical("actual", actual), canonical("expected", expected)
	if !bytes.Equal(actualCanonical, expectedCanonical) {
		t.Fatalf("value = %s, want %s", actualCanonical, expectedCanonical)
	}
}

func decodeFixtureJSON(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	if len(raw) == 0 {
		return
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode fixture value: %v", err)
	}
}
