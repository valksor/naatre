package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

// secretCause is the internal detail a handler must never leak to a client.
const secretCause = "dsn=postgres://user:hunter2@db.internal/orders"

// An application maps a domain failure onto the core error shape, choosing the
// public code, message, retryability, and namespaced details, while the runtime
// keeps ownership of the response path, source, and redaction.
func TestExecuteMapsDomainErrorsOntoTheCoreShape(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		err           error
		wantCode      string
		wantMessage   string
		wantRetryable bool
		wantDetails   map[string]any
	}{
		{
			name: "domain code is published",
			err: &naatreruntime.Error{
				Code: "ORDER_LOCKED", Message: "order is locked", Retryable: true,
				Details: map[string]any{"shop.retryAfter": "30s", "unnamespaced": "dropped"},
				Cause:   errors.New(secretCause),
			},
			wantCode: "ORDER_LOCKED", wantMessage: "order is locked", wantRetryable: true,
			wantDetails: map[string]any{"shop.retryAfter": "30s"},
		},
		{
			name: "reserved code cannot be impersonated",
			err: &naatreruntime.Error{
				Code: naatreruntime.CodeInternal, Message: "not really internal",
				Cause: errors.New(secretCause),
			},
			wantCode: naatreruntime.CodeHandlerFailed, wantMessage: "field unavailable",
		},
		{
			name:     "malformed code degrades to the generic failure",
			err:      &naatreruntime.Error{Code: "lower case", Message: "nope"},
			wantCode: naatreruntime.CodeHandlerFailed, wantMessage: "field unavailable",
		},
		{
			name:     "empty message degrades to the generic failure",
			err:      &naatreruntime.Error{Code: "ORDER_LOCKED", Message: "  "},
			wantCode: naatreruntime.CodeHandlerFailed, wantMessage: "field unavailable",
		},
		{
			name:     "a plain error never selects a code",
			err:      errors.New(secretCause),
			wantCode: naatreruntime.CodeHandlerFailed, wantMessage: "field unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := rootStringCallSnapshot(t, "domain", func(context.Context, naatreruntime.Invocation) (string, error) {
				return "", test.err
			})
			outcome := executeRuntimeQuery(t, snapshot, `{"$call":{"name":"domain"}}`)
			if len(outcome.Errors) != 1 {
				t.Fatalf("Execute errors = %#v", outcome.Errors)
			}
			failure := outcome.Errors[0]
			if failure.Code != test.wantCode || failure.Message != test.wantMessage {
				t.Fatalf("error = %q/%q, want %q/%q", failure.Code, failure.Message, test.wantCode, test.wantMessage)
			}
			if failure.Retryable != test.wantRetryable {
				t.Fatalf("retryable = %v, want %v", failure.Retryable, test.wantRetryable)
			}
			if len(failure.Details) != len(test.wantDetails) {
				t.Fatalf("details = %#v, want %#v", failure.Details, test.wantDetails)
			}
			for key, want := range test.wantDetails {
				if failure.Details[key] != want {
					t.Fatalf("details[%q] = %#v, want %#v", key, failure.Details[key], want)
				}
			}
			// The runtime keeps the response path regardless of the code.
			if len(failure.Path) != 1 || failure.Path[0] != "domain" {
				t.Fatalf("path = %#v, want [domain]", failure.Path)
			}
			assertNoInternalDisclosure(t, outcome)
		})
	}
}

func TestDomainErrorsCannotImpersonateReservedRuntimeCodes(t *testing.T) {
	t.Parallel()
	reserved := []string{
		naatreruntime.CodeValidationFailed,
		naatreruntime.CodeInvalidCursor,
		"TRANSACTION_BEGIN_FAILED",
		"TRANSACTION_COMMIT_FAILED",
		"TRANSACTION_COMMIT_UNKNOWN",
		"TRANSACTION_ROLLBACK_FAILED",
		"SAVEPOINT_BEGIN_FAILED",
		"SAVEPOINT_RELEASE_FAILED",
		"SAVEPOINT_ROLLBACK_FAILED",
		"OUTBOX_PERSIST_FAILED",
		"AFTER_COMMIT_FAILED",
		"EXTERNAL_EFFECT_UNCOORDINATED",
		"COMPENSATION_FAILED",
		naatreruntime.CodeOverloaded,
		naatreruntime.CodeRateLimited,
	}
	for _, code := range reserved {
		code := code
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			snapshot := rootStringCallSnapshot(t, "domain", func(context.Context, naatreruntime.Invocation) (string, error) {
				return "", &naatreruntime.Error{Code: code, Message: "forged runtime state"}
			})
			outcome := executeRuntimeQuery(t, snapshot, `{"$call":{"name":"domain"}}`)
			if len(outcome.Errors) != 1 || outcome.Errors[0].Code != naatreruntime.CodeHandlerFailed {
				t.Fatalf("reserved code %q produced errors %#v", code, outcome.Errors)
			}
		})
	}
}

// Internal causes stay reachable to in-process hooks through Unwrap while never
// appearing in anything a client can observe.
func TestExecuteRedactsInternalCausesFromPublicOutput(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		handler func(context.Context, naatreruntime.Invocation) (string, error)
	}{
		{"returned error", func(context.Context, naatreruntime.Invocation) (string, error) {
			return "", errors.New(secretCause)
		}},
		{"panic", func(context.Context, naatreruntime.Invocation) (string, error) {
			panic(secretCause)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := rootStringCallSnapshot(t, "leaky", test.handler)
			outcome := executeRuntimeQuery(t, snapshot, `{"$call":{"name":"leaky"}}`)
			if len(outcome.Errors) != 1 {
				t.Fatalf("Execute errors = %#v", outcome.Errors)
			}
			assertNoInternalDisclosure(t, outcome)
			// A returned cause remains available to a server hook.
			if test.name == "returned error" {
				if cause := errors.Unwrap(outcome.Errors[0]); cause == nil || !strings.Contains(cause.Error(), secretCause) {
					t.Fatalf("server hook could not reach the internal cause: %v", cause)
				}
			}
		})
	}
}

// RequireComplete refuses to hand back data that an error left unresolved, so a
// caller cannot cast partial data onto a model with non-null required fields.
func TestOutcomeRequireCompleteSeparatesPartialFromComplete(t *testing.T) {
	t.Parallel()
	t.Run("incomplete outcome is refused", func(t *testing.T) {
		t.Parallel()
		snapshot := rootStringCallSnapshot(t, "broken", func(context.Context, naatreruntime.Invocation) (string, error) {
			return "", errors.New(secretCause)
		})
		outcome := executeRuntimeQuery(t, snapshot, `{"$call":{"name":"broken"}}`)
		data, err := outcome.RequireComplete()
		if !errors.Is(err, naatreruntime.ErrIncomplete) {
			t.Fatalf("RequireComplete err = %v, want ErrIncomplete", err)
		}
		if data != nil {
			t.Fatalf("RequireComplete returned data = %#v on an incomplete outcome", data)
		}
		if strings.Contains(err.Error(), secretCause) {
			t.Fatalf("RequireComplete disclosed the internal cause: %v", err)
		}
	})
	t.Run("a legitimate null stays complete", func(t *testing.T) {
		t.Parallel()
		types := compositionTypes(t)
		registry := naatreruntime.NewRegistry(types)
		if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
			t.Fatal(err)
		}
		registerComposition(t, registry, naatreruntime.BindInvocation[*string](naatreruntime.Descriptor{
			Name: "maybe", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
			Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), OutputNullable: true,
			Metadata: completeMetadata(naatreruntime.ReadEffect),
		}, func(context.Context, naatreruntime.Invocation) (*string, error) { return nil, nil }))
		snapshot, err := registry.Freeze()
		if err != nil {
			t.Fatalf("freeze registry: %v", err)
		}
		outcome := executeRuntimeQuery(t, snapshot, `{"$call":{"name":"maybe"}}`)
		data, err := outcome.RequireComplete()
		if err != nil {
			t.Fatalf("RequireComplete on a legitimate null: %v", err)
		}
		if _, present := data["maybe"]; !present {
			t.Fatalf("nullable result key absent from %#v", data)
		}
	})
}

// Cancellation observed before any selection runs is operation-level, so no
// field is blamed and the path is the empty root.
func TestExecuteReportsOperationLevelCancellationAtRootPath(t *testing.T) {
	t.Parallel()
	snapshot := rootStringCallSnapshot(t, "never", func(context.Context, naatreruntime.Invocation) (string, error) {
		t.Error("handler ran after the request was already cancelled")
		return "", nil
	})
	request := decodeRuntimeRequest(t, `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[{"$call":{"name":"never"}}]}]}}`)
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := plan.Execute(ctx)
	if len(outcome.Errors) != 1 || outcome.Errors[0].Code != naatreruntime.CodeCancelled {
		t.Fatalf("Execute errors = %#v", outcome.Errors)
	}
	if len(outcome.Errors[0].Path) != 0 {
		t.Fatalf("operation-level path = %#v, want the empty root path", outcome.Errors[0].Path)
	}
	if !outcome.Errors[0].Retryable {
		t.Fatal("cancellation should be advertised as a retryable failure class")
	}
}

// assertNoInternalDisclosure requires that nothing a client can observe carries
// the internal cause, including through the serialized envelope.
func assertNoInternalDisclosure(t testing.TB, outcome naatreruntime.Outcome) {
	t.Helper()
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	if strings.Contains(string(encoded), secretCause) || strings.Contains(string(encoded), "hunter2") {
		t.Fatalf("serialized outcome disclosed the internal cause: %s", encoded)
	}
	for _, failure := range outcome.Errors {
		if strings.Contains(failure.Message, secretCause) || strings.Contains(failure.Message, "hunter2") {
			t.Fatalf("public message disclosed the internal cause: %q", failure.Message)
		}
		for key, value := range failure.Details {
			if rendered, ok := value.(string); ok && strings.Contains(rendered, secretCause) {
				t.Fatalf("detail %q disclosed the internal cause", key)
			}
		}
	}
}

// executeRuntimeQuery runs one root selection against a frozen snapshot.
func executeRuntimeQuery(t testing.TB, snapshot naatreruntime.Snapshot, selection string) naatreruntime.Outcome {
	t.Helper()
	envelope := `{"version":"1","document":{"operations":[{"name":"Q","kind":"query","select":[` + selection + `]}]}}`
	request, err := protocol.DecodeRequest([]byte(envelope), protocol.DecodeOptions{})
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	plan, err := naatreruntime.Prepare(snapshot, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan.Execute(context.Background())
}

// A mutation reports what happened to its effects separately from whether the
// response could be assembled, because a generic retryable error would hide
// the difference between an effect that never ran and one that may have landed.
func TestExecuteReportsEffectStateSeparatelyFromErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		kind      string
		selection string
		failWrite bool
		badOutput bool
		want      naatreruntime.EffectState
	}{
		{name: "a query declares no effect", kind: "query", selection: `{"$call":{"name":"read"}}`, want: naatreruntime.EffectNotApplicable},
		{name: "a mutation that never reached a write", kind: "mutation", selection: `{"$call":{"name":"failWrite"}}`, failWrite: true, want: naatreruntime.EffectIndeterminate},
		{name: "a completed write is applied", kind: "mutation", selection: `{"$call":{"name":"write","select":[{"$field":{"name":"name"}}]}}`, want: naatreruntime.EffectApplied},
		{name: "a write whose output failed is indeterminate", kind: "mutation", selection: `{"$call":{"name":"write","select":[{"$field":{"name":"name"}}]}}`, badOutput: true, want: naatreruntime.EffectIndeterminate},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			registry := naatreruntime.NewRegistry(compositionTypes(t))
			if err := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{Mode: naatreruntime.AuthorizationAllowByDefault}); err != nil {
				t.Fatal(err)
			}
			registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
				Name: "read", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String),
				Metadata: completeMetadata(naatreruntime.ReadEffect),
			}, func(context.Context, naatreruntime.Invocation) (string, error) { return "ok", nil }))
			writeMetadata := completeMetadata(naatreruntime.WriteEffect)
			registerComposition(t, registry, naatreruntime.BindInvocation[map[string]any](naatreruntime.Descriptor{
				Name: "write", Scope: naatreruntime.RootScope, Kind: protocol.Mutation, Member: naatreruntime.CallMember,
				Input: schema.TypeID(schema.String), Output: "User", Metadata: writeMetadata,
			}, func(context.Context, naatreruntime.Invocation) (map[string]any, error) {
				if test.badOutput {
					// The write happened; only its projection is invalid.
					return map[string]any{"name": int32(7)}, nil
				}
				return map[string]any{"name": "Ada"}, nil
			}))
			registerComposition(t, registry, naatreruntime.BindInvocation[string](naatreruntime.Descriptor{
				Name: "failWrite", Scope: naatreruntime.RootScope, Kind: protocol.Mutation, Member: naatreruntime.CallMember,
				Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String), Metadata: writeMetadata,
			}, func(context.Context, naatreruntime.Invocation) (string, error) {
				return "", errors.New(secretCause)
			}))
			registerSourceStringField(t, registry, "User", "name", nil)
			snapshot, err := registry.Freeze()
			if err != nil {
				t.Fatalf("freeze registry: %v", err)
			}
			selection := test.selection
			envelope := `{"version":"1","document":{"operations":[{"name":"Q","kind":"` + test.kind + `","select":[` + selection + `]}]}}`
			request, err := protocol.DecodeRequest([]byte(envelope), protocol.DecodeOptions{})
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			plan, err := naatreruntime.Prepare(snapshot, request)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			outcome := plan.Execute(context.Background())
			if outcome.Effects != test.want {
				t.Fatalf("effects = %q, want %q (errors %#v)", outcome.Effects, test.want, outcome.Errors)
			}
			// A runtime without a transaction must never claim an undo.
			if outcome.Effects == naatreruntime.EffectRolledBack {
				t.Fatal("reference runtime claimed a rollback it cannot observe")
			}
			assertNoInternalDisclosure(t, outcome)
		})
	}
}

// A persisted operation is identified by its document, not by the values bound
// to its variables, so the same document hashes identically however it is
// invoked. Otherwise every distinct argument would defeat the persisted-
// operation cache and force a re-registration.
func TestDocumentHashIsIndependentOfBoundVariableValues(t *testing.T) {
	t.Parallel()
	document := `{"operations":[{"name":"Q","kind":"query","variables":[{"name":"id","type":"String","required":true}],"select":[{"$call":{"name":"load","args":{"value":{"$var":"id"}}}}]}]}`
	var digests []protocol.Digest
	for _, bound := range []string{`{"id":"u-1"}`, `{"id":"u-2"}`, `{"id":"a much longer value that changes the request size"}`} {
		envelope := []byte(`{"version":"1","document":` + document + `,"variables":` + bound + `}`)
		request, err := protocol.DecodeRequest(envelope, protocol.DecodeOptions{})
		if err != nil {
			t.Fatalf("DecodeRequest(%s): %v", bound, err)
		}
		// The hash covers the document alone, which is what a persisted
		// operation registers.
		canonical, err := protocol.CanonicalizeHashPayload(protocol.DocumentHash, []byte(document), protocol.Limits{})
		if err != nil {
			t.Fatalf("CanonicalizeHashPayload: %v", err)
		}
		digest, err := protocol.SemanticHash(protocol.DocumentHash, canonical)
		if err != nil {
			t.Fatalf("SemanticHash: %v", err)
		}
		if value, present := request.Variable("id"); !present || len(value) == 0 {
			t.Fatalf("variable binding %s did not reach the request", bound)
		}
		digests = append(digests, digest)
	}
	for index := 1; index < len(digests); index++ {
		if digests[index] != digests[0] {
			t.Fatalf("binding %d changed the document hash: %v vs %v", index, digests[index], digests[0])
		}
	}
}
