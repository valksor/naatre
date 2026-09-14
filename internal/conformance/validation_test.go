package conformance_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

type portableValidationFixture struct {
	Profile      string `json:"profile"`
	FixtureSuite string `json:"fixtureSuite"`
	Trait        string `json:"trait"`
	Limits       struct {
		MaxViolations   int `json:"maxViolations"`
		MaxPatternBytes int `json:"maxPatternBytes"`
		MaxRules        int `json:"maxRules"`
		MaxRuleDepth    int `json:"maxRuleDepth"`
		MaxRuleNodes    int `json:"maxRuleNodes"`
	} `json:"limits"`
	Agreement struct {
		Order             []string `json:"order"`
		Missing           string   `json:"missing"`
		Null              string   `json:"null"`
		Client            string   `json:"client"`
		Server            string   `json:"server"`
		PersistedIdentity string   `json:"persistedIdentity"`
	} `json:"agreement"`
	Output struct {
		Phase                string `json:"phase"`
		FailureCode          string `json:"failureCode"`
		InvalidMember        string `json:"invalidMember"`
		UnrelatedSiblings    string `json:"unrelatedSiblings"`
		CascadingConstraints string `json:"cascadingConstraints"`
	} `json:"output"`
	Cases []struct {
		Name        string                       `json:"name"`
		InputJSON   string                       `json:"inputJSON"`
		Constraints schema.ConstraintSet         `json:"constraints"`
		Violations  []schema.ConstraintViolation `json:"violations"`
	} `json:"cases"`
	RejectedPatterns []string `json:"rejectedPatterns"`
	JSONSchema       struct {
		Dialect     string          `json:"dialect"`
		Exact       json.RawMessage `json:"exact"`
		Unsupported []struct {
			Schema  json.RawMessage `json:"schema"`
			Code    string          `json:"code"`
			Pointer string          `json:"pointer"`
		} `json:"unsupported"`
	} `json:"jsonSchema"`
	Unsupported         []string `json:"unsupported"`
	UnsupportedFeatures []struct {
		Feature string `json:"feature"`
		Code    string `json:"code"`
		Pointer string `json:"pointer"`
	} `json:"unsupportedFeatures"`
	Commands []string `json:"commands"`
}

func TestPortableValidationEvidence(t *testing.T) {
	t.Parallel()
	var fixture portableValidationFixture
	readFixture(t, "validation.json", &fixture)
	if fixture.Profile != "core.validation-1" || fixture.FixtureSuite != "1.0.0" || fixture.Trait != schema.ConstraintTraitID || fixture.Limits.MaxViolations != 32 || fixture.Limits.MaxPatternBytes != 1024 || fixture.Limits.MaxRules != 32 || fixture.Limits.MaxRuleDepth != 16 || fixture.Limits.MaxRuleNodes != 128 {
		t.Fatalf("validation metadata = %#v", fixture)
	}
	if !slices.Equal(fixture.Agreement.Order, []string{"substitute-variables", "apply-missing-defaults", "coerce-declared-type", "evaluate-constraints", "authorize", "execute"}) || fixture.Agreement.Missing != "not-evaluated" || fixture.Agreement.Null != "not-evaluated-after-nullability-check" || fixture.Agreement.Client != "advisory-must-match-corpus" || fixture.Agreement.Server != "authoritative-before-handler" || fixture.Agreement.PersistedIdentity != "canonical-post-default-input" {
		t.Fatalf("validation agreement = %#v", fixture.Agreement)
	}
	if fixture.Output.Phase != "completion-before-emission" || fixture.Output.FailureCode != "OUTPUT_COMPLETION" || fixture.Output.InvalidMember != "unavailable" || fixture.Output.UnrelatedSiblings != "retained" || fixture.Output.CascadingConstraints != "suppressed-after-structural-failure" {
		t.Fatalf("output validation contract = %#v", fixture.Output)
	}
	for _, vector := range fixture.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			if vector.Name == "invalid-unicode" {
				err := protocol.ValidateJSON([]byte(vector.InputJSON), protocol.Limits{})
				var diagnostic *protocol.Diagnostic
				if !errors.As(err, &diagnostic) || len(vector.Violations) != 1 || diagnostic.Code != vector.Violations[0].Code {
					t.Fatalf("strict JSON error = %v, want %#v", err, vector.Violations)
				}
				return
			}
			trait, err := schema.ConstraintTrait(vector.Constraints)
			if err != nil {
				t.Fatal(err)
			}
			err = schema.ValidateConstraintValue(json.RawMessage(vector.InputJSON), []schema.TraitDescriptor{trait})
			if len(vector.Violations) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var constraintErr *schema.ConstraintError
			if !errors.As(err, &constraintErr) {
				t.Fatalf("validation error = %v", err)
			}
			got := constraintErr.Violations()
			if len(got) != len(vector.Violations) {
				t.Fatalf("violations = %#v, want %#v", got, vector.Violations)
			}
			for index := range got {
				if got[index].ID != vector.Violations[index].ID || got[index].Code != vector.Violations[index].Code || got[index].Path != vector.Violations[index].Path {
					t.Fatalf("violation %d = %#v, want %#v", index, got[index], vector.Violations[index])
				}
			}
		})
	}
	for _, pattern := range fixture.RejectedPatterns {
		if _, err := schema.ConstraintTrait(schema.ConstraintSet{Pattern: pattern}); err == nil {
			t.Errorf("pattern %q was accepted", pattern)
		}
	}
	assertPortableJSONSchemaEvidence(t, fixture)
	if !slices.Contains(fixture.Unsupported, "cel-core-evaluation") || !slices.Contains(fixture.Unsupported, "network-ref-resolution") || len(fixture.UnsupportedFeatures) != 1 || fixture.UnsupportedFeatures[0].Feature != "cel" || fixture.UnsupportedFeatures[0].Code != "CONSTRAINT_FEATURE_UNSUPPORTED" || fixture.UnsupportedFeatures[0].Pointer != "/cel" || len(fixture.Commands) != 3 {
		t.Fatalf("unsupported boundary = %#v, commands = %#v", fixture.Unsupported, fixture.Commands)
	}
}

func assertPortableJSONSchemaEvidence(t testing.TB, fixture portableValidationFixture) {
	t.Helper()
	if fixture.JSONSchema.Dialect != schema.JSONSchema202012 {
		t.Fatalf("JSON Schema dialect = %q", fixture.JSONSchema.Dialect)
	}
	constraints, fidelity, err := schema.ImportJSONSchemaConstraints(fixture.JSONSchema.Exact, schema.JSONSchemaImportOptions{})
	if err != nil || !fidelity.Exact {
		t.Fatalf("exact JSON Schema import = %#v, %v", fidelity, err)
	}
	if _, exported, err := schema.ExportJSONSchemaConstraints(constraints); err != nil || !exported.Exact {
		t.Fatalf("exact JSON Schema export = %#v, %v", exported, err)
	}
	for _, vector := range fixture.JSONSchema.Unsupported {
		_, _, err := schema.ImportJSONSchemaConstraints(vector.Schema, schema.JSONSchemaImportOptions{})
		var mappingErr *schema.JSONSchemaError
		if !errors.As(err, &mappingErr) || len(mappingErr.Diagnostics()) != 1 || mappingErr.Diagnostics()[0].Code != vector.Code || mappingErr.Diagnostics()[0].Pointer != vector.Pointer {
			t.Fatalf("JSON Schema rejection = %v, want %s at %s", err, vector.Code, vector.Pointer)
		}
	}
}
