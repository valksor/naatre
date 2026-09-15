package graphqladapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/runtime"
)

const testSDL = `
schema { query: RootQuery mutation: RootMutation subscription: RootSubscription }
type RootQuery { account(id: ID!, note: String): Account! }
type RootMutation { rename(id: ID!, name: String!): Account }
type RootSubscription { accountChanged(id: ID!): Account! }
type Account {
  id: ID!
  name: String!
  secret: String
  friends: [Account!]!
}
enum Status { ACTIVE DISABLED }
input Filter { status: Status tags: [String!] }
union SearchResult = Account
`

func testPolicy(effect runtime.Effect) Policy {
	return Policy{
		Effect: effect, Idempotency: runtime.IdempotencyIdempotent, Cost: 3,
		Deterministic: true, ThreadSafety: runtime.ThreadSafe,
		Batching: runtime.BatchIneligible, Transaction: runtime.TransactionNone,
		AuthorizationPolicy: "accounts.read",
	}
}

func testImportConfig() ImportConfig {
	read := testPolicy(runtime.ReadEffect)
	write := testPolicy(runtime.WriteEffect)
	write.Idempotency = runtime.IdempotencyNonIdempotent
	write.AuthorizationPolicy = "accounts.write"
	return ImportConfig{
		AdapterID: "graphql.reference", SchemaIdentity: "accounts.schema-1",
		WireVersion: "graphql.september2025", Revision: "accounts.schema-1",
		Policies: map[string]Policy{
			"RootQuery.account": read, "RootMutation.rename": write,
			"RootSubscription.accountChanged": read,
			"Account.id":                      read, "Account.name": read,
			"Account.secret": read, "Account.friends": read,
		},
	}
}

func testImportedSchema(t *testing.T) ImportedSchema {
	t.Helper()
	imported, report, err := ImportSchema(context.Background(), []byte(testSDL), testImportConfig())
	if err != nil {
		cause := errors.Unwrap(err)
		t.Fatalf("ImportSchema: %v, cause=%v, root=%v, report=%#v", err, cause, errors.Unwrap(cause), report)
	}
	if report.Status != "ready" || report.Profile != Profile || report.Direction != interopadapter.SchemaImport {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Mappings) != 17 {
		t.Fatalf("report omitted normative #56 feature classifications: %#v", report.Mappings)
	}
	for _, mapping := range report.Mappings {
		if mapping.Classification == interopadapter.Unsupported ||
			(mapping.Classification != interopadapter.Lossless && mapping.Resolution == "") {
			t.Fatalf("report contains unresolved semantic loss: %#v", mapping)
		}
	}
	return imported
}

func TestSchemaImportRequiresPoliciesAndPreservesGraphQLNullability(t *testing.T) {
	t.Parallel()
	config := testImportConfig()
	delete(config.Policies, "Account.secret")
	_, report, err := ImportSchema(context.Background(), []byte(testSDL), config)
	if CodeOf(err) != CodePolicyRequired || report.Status != "rejected" {
		t.Fatalf("missing policy = %v, report=%#v", err, report)
	}

	imported := testImportedSchema(t)
	operations := imported.Operations()
	account := operations["account"]
	if account.OutputNullable || account.Metadata.AuthorizationPolicy != "accounts.read" || account.Metadata.Cost != 3 {
		t.Fatalf("account descriptor = %#v", account)
	}
	rename := operations["rename"]
	if !rename.OutputNullable || rename.Metadata.Effect != runtime.WriteEffect || rename.Metadata.AuthorizationPolicy != "accounts.write" {
		t.Fatalf("rename descriptor = %#v", rename)
	}
	document := imported.Document()
	if document.Revision() != "accounts.schema-1" || len(document.Operations()) != 3 {
		t.Fatalf("document revision/operations = %q/%d", document.Revision(), len(document.Operations()))
	}
}

func TestSchemaImportRejectsUnsupportedSemanticsAndResourceBoundaries(t *testing.T) {
	t.Parallel()
	config := testImportConfig()
	_, report, err := ImportSchema(context.Background(), []byte(`interface Node { id: ID! }`), config)
	if CodeOf(err) != CodeUnsupported || report.Status != "rejected" {
		t.Fatalf("interface error = %v, report=%#v", err, report)
	}

	config.Limits = Limits{MaxDocumentBytes: 8}
	_, _, err = ImportSchema(context.Background(), []byte(testSDL), config)
	if CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("byte limit error = %v", err)
	}

	config = testImportConfig()
	config.Limits = Limits{MaxTokens: 4}
	_, _, err = ImportSchema(context.Background(), []byte(testSDL), config)
	if CodeOf(err) != CodeResourceExhausted {
		t.Fatalf("token limit error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	config = testImportConfig()
	_, _, err = ImportSchema(ctx, []byte(testSDL), config)
	if CodeOf(err) != CodeCancelled {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestSchemaImportHonorsExplicitRootsAndRejectsInvalidUnions(t *testing.T) {
	t.Parallel()
	config := testImportConfig()
	minimal := `schema { query: RootQuery } type RootQuery { account(id: ID!): Account! } type Mutation { rename: Account } type Account { id: ID! }`
	config.Policies = map[string]Policy{
		"RootQuery.account": testPolicy(runtime.ReadEffect), "Mutation.rename": testPolicy(runtime.ReadEffect),
		"Account.id": testPolicy(runtime.ReadEffect),
	}
	imported, _, err := ImportSchema(context.Background(), []byte(minimal), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := imported.Operations()["rename"]; exists {
		t.Fatal("explicit schema declaration retained an undeclared default mutation root")
	}

	invalidUnion := `type Query { search: Search! } input Filter { id: ID } union Search = Filter`
	config.Policies = map[string]Policy{"Query.search": testPolicy(runtime.ReadEffect)}
	_, _, err = ImportSchema(context.Background(), []byte(invalidUnion), config)
	if CodeOf(err) != CodeSchemaInvalid {
		t.Fatalf("invalid union = %v", err)
	}
}

func TestSchemaExportUsesOnlySupportedNaatreSubset(t *testing.T) {
	t.Parallel()
	imported := testImportedSchema(t)
	output, report, err := ExportSchema(context.Background(), imported.Document(), ExportConfig{
		AdapterID: "graphql.reference", SchemaIdentity: "accounts.schema-1", WireVersion: "graphql.september2025",
	})
	if err != nil {
		t.Fatalf("ExportSchema: %v, report=%#v", err, report)
	}
	text := string(output)
	for _, expected := range []string{
		"type Account", "account(id: ID!, note: String): Account!",
		"rename(id: ID!, name: String!): Account", "accountChanged(id: ID!): Account!",
		"friends: [Account!]!", "union SearchResult = Account",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("SDL omits %q:\n%s", expected, text)
		}
	}
	if report.Status != "ready" || report.Direction != interopadapter.SchemaExport {
		t.Fatalf("report = %#v", report)
	}
	encoded, err := MarshalReport(report)
	if err != nil || !strings.Contains(string(encoded), `"profile":"core.adapters.graphql-1"`) {
		t.Fatalf("canonical report = %s, %v", encoded, err)
	}
}

func TestErrorsExposeOnlyStableCode(t *testing.T) {
	t.Parallel()
	secret := errors.New("Authorization: bearer-secret internal.example")
	err := adapterError(CodeSchemaInvalid, secret)
	if strings.Contains(err.Error(), "bearer-secret") || strings.Contains(err.Error(), "internal.example") {
		t.Fatalf("public error leaked cause: %v", err)
	}
	if !errors.Is(err, secret) || CodeOf(err) != CodeSchemaInvalid {
		t.Fatalf("trusted unwrap/code = %v/%s", errors.Unwrap(err), CodeOf(err))
	}
}
