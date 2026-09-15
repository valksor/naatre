package asyncapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valksor/naatre/asyncapi"
	"github.com/valksor/naatre/schema"
)

func TestProjectionPinnedConformanceFixture(t *testing.T) {
	t.Parallel()
	input, err := os.ReadFile("../conformance/v1/asyncapi-projection.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := asyncapi.ValidateProjectionConformanceFixture(input); err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Dependencies []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"dependencies"`
		Implementation struct {
			Files []struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		} `json:"implementation"`
	}
	if err := json.Unmarshal(input, &fixture); err != nil {
		t.Fatal(err)
	}
	files := append(fixture.Dependencies, fixture.Implementation.Files...)
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatalf("read pinned file %s: %v", file.Path, err)
		}
		digest := sha256.Sum256(content)
		if got := hex.EncodeToString(digest[:]); got != file.SHA256 {
			t.Errorf("pinned file %s digest = %s, want %s", file.Path, got, file.SHA256)
		}
	}
}

func TestProjectionRoundTripIsByteStableAndKeepsSchemaAuthority(t *testing.T) {
	t.Parallel()
	model := fixtureModel(t)
	ctx := context.Background()
	document, exported, err := asyncapi.ExportProjection(ctx, model, asyncapi.ProjectionExportOptions{})
	if err != nil || exported.Status != "ready" || exported.Profile != asyncapi.ProjectionProfile {
		t.Fatalf("ExportProjection = %#v, %v", exported, err)
	}
	var projected struct {
		Profile    string `json:"x-naatre-profile"`
		Components struct {
			Schemas  map[string]map[string]any `json:"schemas"`
			Messages map[string]struct {
				SchemaFormat string `json:"schemaFormat"`
			} `json:"messages"`
		} `json:"components"`
	}
	if err := json.Unmarshal(document, &projected); err != nil {
		t.Fatal(err)
	}
	profile := projected.Components.Schemas["ProfileEvent"]
	coreID := projected.Components.Schemas["ID"]
	properties, _ := profile["properties"].(map[string]any)
	id, _ := properties["id"].(map[string]any)
	if projected.Profile != asyncapi.ProjectionProfile || profile["x-naatre-declaration"] == nil || id["format"] != "naatre-id" ||
		coreID["x-naatre-implicit-core-scalar"] != true || projected.Components.Messages["Event"].SchemaFormat != asyncapi.ProjectionJSONDialect {
		t.Fatalf("incomplete JSON Schema projection: %#v", projected)
	}
	imported, report, err := asyncapi.ImportProjection(ctx, document, asyncapi.ProjectionImportOptions{
		Limits: asyncapi.DefaultLimits(), AllowedServerURLs: []string{"https://events.example/v1"}, AuthoritativeSchema: model.Schema,
	})
	if err != nil || report.Status != "ready" || report.Profile != asyncapi.ProjectionProfile {
		t.Fatalf("ImportProjection = %#v, %v", report, err)
	}
	wantSchema, _ := model.Schema.CanonicalJSON()
	gotSchema, gotErr := imported.Schema.CanonicalJSON()
	if gotErr != nil || !bytes.Equal(gotSchema, wantSchema) {
		t.Fatalf("schema authority changed: %v\n%s\n%s", gotErr, gotSchema, wantSchema)
	}
	regenerated, _, err := asyncapi.ExportProjection(ctx, imported, asyncapi.ProjectionExportOptions{})
	if err != nil || !bytes.Equal(regenerated, document) {
		t.Fatalf("projection round trip changed bytes: %v\n%s\n%s", err, regenerated, document)
	}
	if validation, err := asyncapi.ValidateProjection(ctx, document, asyncapi.ProjectionImportOptions{
		Limits: asyncapi.DefaultLimits(), AllowedServerURLs: []string{"https://events.example/v1"}, AuthoritativeSchema: model.Schema,
	}); err != nil || validation.Status != "ready" {
		t.Fatalf("ValidateProjection = %#v, %v", validation, err)
	}
}

func TestProjectionRejectsSchemaAuthorityAndPolicyRedefinition(t *testing.T) {
	t.Parallel()
	model := fixtureModel(t)
	document, _, err := asyncapi.ExportProjection(context.Background(), model, asyncapi.ProjectionExportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	canonicalSchema, _ := model.Schema.CanonicalJSON()
	changed := bytes.Replace(canonicalSchema, []byte(`"authorizationPolicy":"tenant"`), []byte(`"authorizationPolicy":"attacker"`), 1)
	changedAuthority, parseErr := schema.ParseDocument(changed, schema.ImportOptions{})
	if parseErr != nil {
		t.Fatalf("parse changed authority: %v", parseErr)
	}
	_, report, err := asyncapi.ImportProjection(context.Background(), document, asyncapi.ProjectionImportOptions{
		AllowedServerURLs: []string{"https://events.example/v1"}, AuthoritativeSchema: changedAuthority,
	})
	if errorCode(err) != "ASYNCAPI_SCHEMA_AUTHORITY_MISMATCH" || report.Status != "rejected" {
		t.Fatalf("authority mismatch = %#v, %v", report, err)
	}

	var wire map[string]any
	if err := json.Unmarshal(document, &wire); err != nil {
		t.Fatal(err)
	}
	operations := wire["operations"].(map[string]any)
	operations["receiveProfiles"].(map[string]any)["x-naatre-authorization-policy"] = "attacker"
	tampered, _ := json.Marshal(wire)
	_, report, err = asyncapi.ImportProjection(context.Background(), tampered, asyncapi.ProjectionImportOptions{
		AllowedServerURLs: []string{"https://events.example/v1"}, AuthoritativeSchema: model.Schema,
	})
	if errorCode(err) != "ASYNCAPI_DOCUMENT_INVALID" || report.Status != "rejected" {
		t.Fatalf("policy redefinition = %#v, %v", report, err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "attacker") || strings.Contains(err.Error(), "attacker") {
		t.Fatalf("diagnostic leaked protected metadata: %s, %v", encoded, err)
	}
}

func TestProjectionCancellationAndResourceBoundaries(t *testing.T) {
	t.Parallel()
	model := fixtureModel(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, report, err := asyncapi.ExportProjection(cancelled, model, asyncapi.ProjectionExportOptions{})
	if errorCode(err) != "ASYNCAPI_PROJECTION_CANCELLED" || report.Diagnostics[0].Code != "ASYNCAPI_PROJECTION_CANCELLED" {
		t.Fatalf("cancelled export = %#v, %v", report, err)
	}

	document, _, err := asyncapi.ExportProjection(context.Background(), model, asyncapi.ProjectionExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	limits := asyncapi.DefaultLimits()
	limits.MaxDocumentBytes = len(document)
	_, report, err = asyncapi.ImportProjection(context.Background(), document, asyncapi.ProjectionImportOptions{
		Limits: limits, AllowedServerURLs: []string{"https://events.example/v1"}, AuthoritativeSchema: model.Schema,
	})
	if err != nil || report.Status != "ready" {
		t.Fatalf("exact document boundary = %#v, %v", report, err)
	}
	limits.MaxDocumentBytes--
	_, report, err = asyncapi.ImportProjection(context.Background(), document, asyncapi.ProjectionImportOptions{
		Limits: limits, AllowedServerURLs: []string{"https://events.example/v1"}, AuthoritativeSchema: model.Schema,
	})
	if errorCode(err) != "ASYNCAPI_PROJECTION_DOCUMENT_LIMIT" || report.Diagnostics[0].Feature != "resource-limits" {
		t.Fatalf("document limit = %#v, %v", report, err)
	}
}
